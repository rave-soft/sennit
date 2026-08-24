package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/rave-soft/sennit/internal/toolmeta"
	"github.com/stretchr/testify/require"
)

// This file is the parallel-capability counterpart to
// permission_coverage_test.go, and like that file it is exhaustive over
// every built-in tool: each tool appears in exactly one of the
// classifications below, so adding a built-in tool without classifying it
// fails TestParallelClassificationIsExhaustive.
//
// The rule for parallel (fantasy.NewParallelAgentTool): the fantasy
// dispatcher runs parallel tools concurrently — up to five at a time in
// separate goroutines within one assistant turn — so a parallel tool's
// handler must be demonstrably free of shared mutable state. "Demonstrably"
// here means the handler only does one of:
//
//   - read-only filesystem/search work (stat, walk, exec rg, in-memory
//     doublestar) under the working directory,
//   - read-only access to an immutable snapshot captured at construction,
//   - one-way idempotent publication (sync.OnceValue) whose result does
//     not depend on call order,
//
// and nothing else: no file writes, no filetracker, no LSP manager access
// (even a read-only query may lazily start clients via lspManager.Start),
// no thread/task manager, no background-shell manager, no permission
// prompts, no session state, no outbound requests.
//
// The classification splits in three, and the split is what this file must
// not blur:
//
//   - t0ParallelAllowList: the tools THIS task promoted to parallel. Their
//     flags are new, and they carry the behavioral proof:
//     TestParallelAllowListIsStateless runs them concurrently and asserts
//     deterministic, state-free results.
//   - legacyParallelExceptions: tools that were already
//     NewParallelAgentTool before this task and are NOT re-audited here.
//     Each is an explicit exception with its reason and its tech-debt
//     status; their flags are pinned only by
//     TestParallelFlagsMatchClassification.
//   - sequentialDenyList: everything else, each with the concrete shared
//     state its handler touches, which is why it must stay on the
//     dispatcher's sequential path.
//
// The flag check (TestParallelFlagsMatchClassification) is exhaustive: it
// constructs every possible built-in tool this package can build — the
// T0 allow-list, the legacy exceptions, and the sequential tools alike —
// and asserts the dispatcher-visible Info().Parallel agrees with the
// classification. The only tools that cannot be built from this package at
// all are the coordinator-built agent tools ("agent", "ask_parent",
// "agentic_fetch"); their flags are pinned by source inspection of the
// constructors in internal/agent, which are verified by runtime tests in that package.

// t0ParallelAllowList is this task's parallel set.
var t0ParallelAllowList = []string{
	GrepToolName,       // searchFiles: exec rg or in-memory walk; read-only
	RipgrepToolName,    // same searchFiles backend as grep; read-only
	GlobToolName,       // rg --files or gitignore-aware walk; read-only
	LSToolName,         // tree listing; outside-working-dir paths hit a pure permission check
	SennitLogsToolName, // tails a fixed log file path
	SennitInfoToolName, // renders the snapshot captured at construction
	GitStatusToolName,  // validated read-only git status; no shared Go state
	GitDiffToolName,    // validated read-only git diff; no shared Go state
	GitLogToolName,     // validated read-only git log; no shared Go state
}

// legacyParallelExceptions are the tools that already carried
// NewParallelAgentTool before this task. They are classified explicitly —
// as legacy exceptions, not as T0 decisions — and each entry names the
// state its handler touches and the debt that leaves it un-audited here.
var legacyParallelExceptions = []struct {
	name   string
	reason string
}{
	{"agent", "runs a sub-agent; built by the coordinator (internal/agent/agent_tool.go). Pre-existing parallel flag; no T0 re-audit — tech debt"},
	{FetchToolName, "outbound HTTP via an injected *http.Client; read-only w.r.t. local state. Pre-existing parallel flag; no T0 re-audit — tech debt"},
	{WebFetchToolName, "outbound HTTP via an injected *http.Client; read-only w.r.t. local state. Pre-existing parallel flag; no T0 re-audit — tech debt"},
	{WebSearchToolName, "outbound HTTP through a SearchBackend; read-only w.r.t. local state. Pre-existing parallel flag; no T0 re-audit — tech debt"},
	{AgenticFetchToolName, "runs a sub-agent over an outbound fetch; built by the coordinator. Pre-existing parallel flag; no T0 re-audit — tech debt"},
	{DownloadToolName, "MUTATING: writes the downloaded file into the workspace while marked parallel. Pre-existing flag, kept untouched by T0; re-audit before relying on it — tech debt"},
	{ListMCPResourcesToolName, "shared *mcp.Registry client lifecycle + permission gate. Pre-existing parallel flag; no T0 re-audit — tech debt"},
	{ReadMCPResourceToolName, "reads an MCP resource through shared registry state and permission gate; pre-existing parallel flag, retained as legacy behavior"},
}

// sequentialDenyList is the complement: every other built-in tool, each
// with the concrete shared state its handler touches, which is why it must
// stay on the dispatcher's sequential path.
var sequentialDenyList = []struct {
	name   string
	reason string
}{
	// Files.
	{ReadToolName, "filetracker RMW (RecordRead/RecordPartialRead) + lazy LSP start (openInLSPs) + permission gate outside working dir"},
	{MultiReadToolName, "sequential batch read delegates shared filetracker, LSP, and permission state"},
	{BashToolName, "executes commands, background shells, git, permission prompts"},
	{EditToolName, "writes files, filetracker, permission prompts"},
	{MultiEditToolName, "writes files, filetracker, permission prompts"},
	{WriteToolName, "writes/overwrites files, permission prompts"},
	// LSP: every access path may lazily start clients (manager.Start).
	{DiagnosticsToolName, "lazy LSP start + workspace didChange notifications"},
	{ReferencesToolName, "resolveSymbolResults → lspManager.Start"},
	{SymbolsToolName, "lspManager.Start"},
	{DefinitionToolName, "resolveSymbol → lspManager.Start"},
	{CallHierarchyToolName, "resolveSymbol → lspManager.Start"},
	{RenameToolName, "writes files through the LSP"},
	{ReplaceSymbolToolName, "writes files through the LSP"},
	{LSPRestartToolName, "restarts LSP clients"},
	// Sessions, queues, processes, threads, tasks.
	{TodosToolName, "session state read/write"},
	{QuestionToolName, "interactive prompt on the session"},
	{JobOutputToolName, "background-shell manager state; wait=true blocks"},
	{JobKillToolName, "kills background shell processes"},
	{ThreadListToolName, "thread manager state"},
	{ThreadStatusToolName, "thread manager state"},
	{ThreadCreateToolName, "starts a thread"},
	{ThreadWaitToolName, "blocks on thread manager state"},
	{ThreadMergeToolName, "merges a thread, git worktree"},
	{ThreadRemoveToolName, "removes a thread"},
	{TaskListToolName, "task manager state"},
	{TaskResultToolName, "task manager state"},
	{TaskOutputToolName, "task manager state"},
	{TaskSendToolName, "posts into a task queue"},
	{TaskCancelToolName, "cancels a task"},
	{AskParentToolName, "parent session state; built by the coordinator"},
}

// optionalToolNames are built-in tools that exist in this package but are
// not part of config.AllToolNames() (the default set). They must still be
// classified, and TestParallelClassificationIsExhaustive checks them too:
//
//   - thread_send is deliberately absent from the default set (see the
//     comment in config.allToolNames) but is still a real tool any agent
//     config can enable, so it is pinned to sequential like thread_wait.
var optionalToolNames = []struct {
	name     string
	parallel bool
	reason   string
}{
	{ThreadSendToolName, false, "posts into a thread queue; not in the default tool set, enabled per agent config"},
}

// classificationFor returns the expected parallel flag for a built-in tool
// name. It is a lookup, not a default: an unclassified name is a bug and
// fails the calling test.
func classificationFor(t *testing.T, name string) bool {
	t.Helper()
	if slices.Contains(t0ParallelAllowList, name) {
		return true
	}
	for _, entry := range legacyParallelExceptions {
		if entry.name == name {
			return true
		}
	}
	for _, entry := range sequentialDenyList {
		if entry.name == name {
			return false
		}
	}
	for _, entry := range optionalToolNames {
		if entry.name == name {
			return entry.parallel
		}
	}
	t.Fatalf("tool %q is missing from the parallel classification", name)
	return false
}

// classifiedToolNames is the union of every name the classification knows:
// the T0 allow-list, the legacy exceptions, the deny list and the optional
// tools.
func classifiedToolNames() []string {
	out := append([]string{}, t0ParallelAllowList...)
	for _, entry := range legacyParallelExceptions {
		out = append(out, entry.name)
	}
	for _, entry := range sequentialDenyList {
		out = append(out, entry.name)
	}
	for _, entry := range optionalToolNames {
		out = append(out, entry.name)
	}
	return out
}

// TestParallelClassificationIsExhaustive pins the invariant the rest of the
// file relies on: the classification names are unique;
// config.AllToolNames() (the default set) plus the optional tools are
// covered with no gaps; and the three buckets (T0 allow-list, legacy
// exceptions, sequential) do not overlap. This is the gate that makes a
// future built-in tool fail here until it is classified — and the gate
// that makes a tool silently move between the T0 set and the legacy
// exceptions fail here until the move is a conscious edit.
func TestParallelClassificationIsExhaustive(t *testing.T) {
	t.Parallel()

	registered := config.AllToolNames()
	seen := map[string]string{}
	add := func(name, bucket string) {
		if prev, ok := seen[name]; ok {
			t.Fatalf("tool %q appears in both %q and %q — every tool must be classified exactly once", name, prev, bucket)
		}
		seen[name] = bucket
	}
	for _, name := range t0ParallelAllowList {
		require.Contains(t, registered, name, "T0 allow-list tool %q is not a registered built-in tool", name)
		add(name, "t0ParallelAllowList")
	}
	for _, entry := range legacyParallelExceptions {
		require.Contains(t, registered, entry.name, "legacy exception %q is not a registered built-in tool", entry.name)
		add(entry.name, "legacyParallelExceptions")
	}
	for _, entry := range sequentialDenyList {
		require.Contains(t, registered, entry.name, "deny-list tool %q is not a registered built-in tool", entry.name)
		add(entry.name, "sequentialDenyList")
	}
	for _, entry := range optionalToolNames {
		require.NotContains(t, registered, entry.name, "optional tool %q is actually in the default set — move it to sequentialDenyList", entry.name)
		add(entry.name, "optionalToolNames")
	}

	for _, name := range registered {
		require.Contains(t, seen, name, "built-in tool %q is not classified in the parallel classification", name)
	}
	require.Len(t, seen, len(registered)+len(optionalToolNames),
		"classification covers %d of %d default + %d optional tools", len(seen), len(registered), len(optionalToolNames))
}

// buildForInfo returns a constructed instance of every possible built-in
// tool this package can build — the T0 allow-list, the legacy exceptions
// (with test doubles for their dependencies: a nil *http.Client, a stub
// SearchBackend, an empty mcp.Registry), and the sequential tools. The
// only tools it cannot build are the coordinator-built agent tools, which
// are named in coordinatorBuiltToolNames.
func buildForInfo(t *testing.T, name string) fantasy.AgentTool {
	t.Helper()
	dir := t.TempDir()
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	perms := &mockPermissionService{}
	switch name {
	case ReadToolName:
		return NewReadTool(nil, nil, nil, nil, dir)
	case MultiReadToolName:
		return NewMultiReadTool(nil, nil, nil, nil, dir)
	case GlobToolName:
		return NewGlobTool(dir, config.ToolGlob{})
	case GrepToolName:
		return NewGrepTool(dir, config.ToolGrep{})
	case RipgrepToolName:
		return NewRipgrepTool(dir, config.ToolGrep{})
	case LSToolName:
		return NewLsTool(nil, dir, config.ToolLs{})
	case SennitLogsToolName:
		return NewSennitLogsTool(filepath.Join(dir, "nonexistent.log"))
	case SennitInfoToolName:
		return NewSennitInfoTool(testConfigStore(t, dir), nil, nil, nil, nil, nil, nil)
	case BashToolName:
		return NewBashTool(nil, dir, attribution, "test-model", shell.NewBackgroundShellManager())
	case GitStatusToolName:
		return NewGitStatusTool(dir)
	case GitDiffToolName:
		return NewGitDiffTool(dir)
	case GitLogToolName:
		return NewGitLogTool(dir)
	case EditToolName:
		return NewEditTool(nil, nil, &mockHistoryService{}, mockFileTrackerService{}, dir)
	case MultiEditToolName:
		return NewMultiEditTool(nil, nil, &mockHistoryService{}, mockFileTrackerService{}, dir)
	case WriteToolName:
		return NewWriteTool(nil, nil, &mockHistoryService{}, mockFileTrackerService{}, dir)
	case TodosToolName:
		return NewTodosTool(nil)
	case QuestionToolName:
		return NewQuestionTool(nil)
	case JobOutputToolName:
		return NewJobOutputTool(shell.NewBackgroundShellManager())
	case JobKillToolName:
		return NewJobKillTool(shell.NewBackgroundShellManager())
	case DiagnosticsToolName:
		return NewDiagnosticsTool(nil)
	case ReferencesToolName:
		return NewReferencesTool(nil, dir)
	case SymbolsToolName:
		return NewSymbolsTool(nil, dir)
	case DefinitionToolName:
		return NewDefinitionTool(nil, dir)
	case CallHierarchyToolName:
		return NewCallHierarchyTool(nil, dir)
	case LSPRestartToolName:
		return NewLSPRestartTool(nil)
	case RenameToolName:
		return NewRenameTool(nil, nil, &mockHistoryService{}, mockFileTrackerService{}, dir)
	case ReplaceSymbolToolName:
		return NewReplaceSymbolTool(nil, nil, &mockHistoryService{}, mockFileTrackerService{}, dir)
	case ThreadListToolName:
		return NewThreadListTool(panicThreadManager{})
	case ThreadStatusToolName:
		return NewThreadStatusTool(panicThreadManager{})
	case ThreadCreateToolName:
		return NewThreadCreateTool(panicThreadManager{}, nil)
	case ThreadSendToolName:
		return NewThreadSendTool(panicThreadManager{})
	case ThreadWaitToolName:
		return NewThreadWaitTool(panicThreadManager{})
	case ThreadMergeToolName:
		return NewThreadMergeTool(panicThreadManager{}, nil)
	case ThreadRemoveToolName:
		return NewThreadRemoveTool(panicThreadManager{}, nil)
	case TaskListToolName:
		return NewTaskListTool(panicTaskManager{})
	case TaskResultToolName:
		return NewTaskResultTool(panicTaskManager{})
	case TaskOutputToolName:
		return NewTaskOutputTool(panicTaskManager{})
	case TaskSendToolName:
		return NewTaskSendTool(panicTaskManager{})
	case TaskCancelToolName:
		return NewTaskCancelTool(panicTaskManager{}, nil)
	case AskParentToolName:
		return NewAskParentTool(nil)
	// Legacy parallel exceptions: built with test doubles. A nil
	// *http.Client is enough — Info() never performs a request, and the
	// tools' behavior under concurrency is out of scope for T0 (see
	// legacyParallelExceptions).
	case FetchToolName:
		return NewFetchTool(perms, dir, nil)
	case WebFetchToolName:
		return NewWebFetchTool(perms, dir, nil)
	case WebSearchToolName:
		return NewWebSearchTool(perms, dir, nil, &stubSearchBackend{})
	case DownloadToolName:
		return NewDownloadTool(perms, dir, nil)
	case ListMCPResourcesToolName:
		return NewListMCPResourcesTool(testConfigStore(t, dir), mcp.NewRegistry(), perms)
	case ReadMCPResourceToolName:
		return NewReadMCPResourceTool(testConfigStore(t, dir), mcp.NewRegistry(), perms)
	}
	return nil
}

// testConfigStore builds a config.ConfigStore carrying a minimal config
// rooted at dir. It deliberately does not run setDefaults (unexported in
// the config package); instead it initializes the few map fields the
// sennit_info writers dereference directly (c.Providers, c.MCP), so the
// tool runs the same code paths production does without a full Load.
func testConfigStore(t *testing.T, dir string) *config.ConfigStore {
	t.Helper()
	cfg := &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		MCP:       make(map[string]config.MCPConfig),
	}
	return config.NewTestStore(t, cfg, config.WithWorkingDir(dir))
}

// TestParallelFlagsMatchClassification is the unexpected-flag guard: for
// every possible built-in tool this package can construct — T0 allow-list,
// legacy exceptions, and sequential tools alike — the ToolInfo the
// dispatcher actually sees must agree with the classification. It fails if
// a constructor silently starts (or stops) marking a tool parallel: the
// only way to change the parallel set is to change the classification in
// this file in the same commit. Coordinator-built agent and agentic_fetch
// tools are verified by runtime tests in internal/agent.
func TestToolMetadataMatchesConstructedInfo(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool)
	for _, descriptor := range toolmeta.Builtins() {
		if descriptor.Builder == toolmeta.BuilderCoordinator {
			continue
		}
		t.Run(descriptor.Name, func(t *testing.T) {
			t.Parallel()
			tool := buildForInfo(t, descriptor.Name)
			require.NotNilf(t, tool, "metadata tool %q has no constructor fixture", descriptor.Name)
			info := tool.Info()
			require.Equal(t, descriptor.Name, info.Name)
			require.Equal(t, descriptor.ParallelSafe, info.Parallel)
			require.NotEmpty(t, info.InputSchema, "constructor must own a non-empty schema")
		})
		seen[descriptor.Name] = true
	}
	for _, descriptor := range toolmeta.Builtins() {
		if descriptor.Builder == toolmeta.BuilderToolsPackage {
			require.Truef(t, seen[descriptor.Name], "metadata tool %q was not checked", descriptor.Name)
		}
	}
}

func TestParallelFlagsMatchClassification(t *testing.T) {
	t.Parallel()

	for _, name := range classifiedToolNames() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if name == "agent" || name == AgenticFetchToolName {
				t.Skip("verified through real coordinator construction in internal/agent")
			}
			tool := buildForInfo(t, name)
			require.NotNil(t, tool,
				"buildForInfo has no builder for %q — extend it, or name it in coordinatorBuiltToolNames with its source pin", name)
			info := tool.Info()
			require.Equal(t, name, info.Name, "constructed tool's name does not match its registry entry")
			expected := classificationFor(t, name)
			require.Equal(t, expected, info.Parallel,
				"tool %q: Info().Parallel=%v but the classification expects %v", name, info.Parallel, expected)
		})
	}
}

// TestParallelAllowListIsStateless is the behavioral half of the T0
// allow-list: it runs every allow-list handler concurrently — the same
// shape the dispatcher applies: several instances, several different
// arguments, all at once, under -race (the CI test task passes -race).
// The assertions: every concurrent run returns exactly the reference
// response for its arguments, the log file and the working directory are
// byte-identical before and after (no temp files, no caches, no
// deletions), and sennit_info's output is identical across concurrent
// calls against the same config store.
func TestParallelAllowListIsStateless(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "alpha.go"), []byte("package alpha\n\nconst A = 1\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "beta.go"), []byte("package beta\n\nconst B = 2\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "gamma.txt"), []byte("gamma content\nsecond line\n"), 0o644))

	// sennit_logs reads a fixed log file path; a file whose entries
	// straddle the 8 KiB backward-read chunk boundary exercises the
	// remainder-carrying path under concurrency.
	logFile := filepath.Join(dir, "sennit.log")
	var logBuf strings.Builder
	for i := range 60 {
		entry := map[string]any{
			"time":    "2026-02-05T12:00:00Z",
			"level":   "INFO",
			"source":  "main.go",
			"msg":     "entry",
			"seq":     i,
			"payload": strings.Repeat("x", 500),
		}
		b, err := json.Marshal(entry)
		require.NoError(t, err)
		logBuf.Write(b)
		logBuf.WriteByte('\n')
	}
	require.NoError(t, os.WriteFile(logFile, []byte(logBuf.String()), 0o644))

	infoStore := testConfigStore(t, dir)
	infoTool := NewSennitInfoTool(infoStore, nil, nil, nil, nil, nil, nil)

	snapshot, err := snapshotTree(t, dir)
	require.NoError(t, err)

	type callCase struct {
		tool   fantasy.AgentTool
		params string
		key    string
	}
	// key is tool name + instance + params: two sennit_logs cases that
	// share the default params `{}` (the existing file and the missing
	// file) must not collide on a name-only key.
	mkCase := func(tool fantasy.AgentTool, params string) callCase {
		return callCase{tool: tool, params: params, key: tool.Info().Name + "\x00" + fmt.Sprintf("%p", tool) + "\x00" + params}
	}
	cases := []callCase{
		mkCase(NewGrepTool(dir, config.ToolGrep{}), `{"pattern":"const","include":"*.go"}`),
		mkCase(NewGrepTool(dir, config.ToolGrep{}), `{"pattern":"const","include":"*.go","case_insensitive":true}`),
		mkCase(NewGlobTool(dir, config.ToolGlob{}), `{"pattern":"*.go"}`),
		mkCase(NewGlobTool(dir, config.ToolGlob{}), `{"pattern":"sub/**"}`),
		mkCase(NewLsTool(nil, dir, config.ToolLs{}), `{}`),
		mkCase(NewLsTool(nil, dir, config.ToolLs{}), `{"ignore":["*.txt"]}`),
		mkCase(NewSennitLogsTool(logFile), `{}`),
		mkCase(NewSennitLogsTool(logFile), `{"lines":5}`),
		mkCase(NewSennitLogsTool(logFile), `{"lines":500}`),
		mkCase(NewSennitLogsTool(filepath.Join(dir, "missing.log")), `{}`),
		mkCase(infoTool, `{}`),
		mkCase(infoTool, `{"models_for":"nonexistent-provider"}`),
	}
	// Always exercise the ripgrep handler. The injected executable is a
	// deterministic stand-in for rg's JSON protocol, so this does not depend
	// on PATH or testing.Testing's production safeguard.
	ripgrepCommand := func(ctx context.Context, pattern, path, include string, caseInsensitive bool) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `printf '%s\n' '{"type":"match","data":{"path":{"text":"`+filepath.Join(dir, `alpha.go`)+`"},"lines":{"text":"const A = 1\n"},"line_number":3,"submatches":[{"start":0}]}}'`)
	}
	cases = append(cases,
		mkCase(NewRipgrepTool(dir, config.ToolGrep{}, withRipgrepCommand(ripgrepCommand)), `{"pattern":"const","include":"*.go"}`),
		mkCase(NewRipgrepTool(dir, config.ToolGrep{}, withRipgrepCommand(ripgrepCommand)), `{"pattern":"second line","include":"*.txt"}`),
	)

	run := func(t *testing.T, tc callCase) string {
		t.Helper()
		resp, err := tc.tool.Run(t.Context(), fantasy.ToolCall{
			ID:    "call-" + tc.tool.Info().Name,
			Name:  tc.tool.Info().Name,
			Input: tc.params,
		})
		require.NoError(t, err)
		require.False(t, resp.IsError, "tool %s errored: %s", tc.tool.Info().Name, resp.Content)
		// ls walks the tree concurrently (csync.NewSlice in
		// fsext.ListDirectory), so its line order is not guaranteed
		// stable run-to-run even on a static tree — that is
		// non-determinism, not a state mutation. Normalize it: the
		// assertions still catch a missing/extra entry, and the tree
		// snapshot covers the rest.
		if tc.tool.Info().Name == LSToolName {
			lines := strings.Split(resp.Content, "\n")
			sort.Strings(lines)
			return strings.Join(lines, "\n")
		}
		return resp.Content
	}

	// Reference responses, sequential.
	ref := make(map[string]string, len(cases))
	for _, tc := range cases {
		ref[tc.key] = run(t, tc)
	}
	// sennit_info: every concurrent call against the same store must
	// return byte-identical output (the snapshot model the allow-list
	// rests on). The reference run above is one of them.
	require.NotEmpty(t, ref[infoTool.Info().Name+"\x00"+fmt.Sprintf("%p", infoTool)+"\x00{}"], "sennit_info reference response missing")

	// Concurrent fan-out: 4 goroutines race over the same cases, offset so
	// the rg/fallback paths, file walks and log reads overlap.
	const goroutines = 4
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range cases {
				idx := (i + g) % len(cases)
				tc := cases[idx]
				if got := run(t, tc); got != ref[tc.key] {
					t.Errorf("concurrent %s: response differs from sequential reference", tc.tool.Info().Name)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	// The tree — including the log file — must be byte-identical: none of
	// the handlers created, modified or deleted anything (e.g. a cache or
	// temp file).
	after, err := snapshotTree(t, dir)
	require.NoError(t, err)
	require.Equal(t, snapshot, after, "allow-list tools mutated the working directory while running concurrently")
}

// snapshotTree returns a deterministic {relPath: content} snapshot of a
// directory tree.
func snapshotTree(t *testing.T, root string) (map[string]string, error) {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	return out, err
}

// panicTaskManager is a TaskManager whose every method panics. It is only
// ever handed to a tool for the Info() check, which never calls the
// manager; a panic here means a test started running a tool it should not
// have. (panicThreadManager is declared in permission_coverage_test.go.)
type panicTaskManager struct{}

func (panicTaskManager) Create(context.Context, TaskCreateArgs) (TaskInfo, error) {
	panic("parallel_tools_test: TaskManager.Create called")
}

func (panicTaskManager) List(context.Context) ([]TaskInfo, error) {
	panic("parallel_tools_test: TaskManager.List called")
}

func (panicTaskManager) Get(context.Context, string) (TaskInfo, error) {
	panic("parallel_tools_test: TaskManager.Get called")
}

func (panicTaskManager) Cancel(context.Context, string, string) error {
	panic("parallel_tools_test: TaskManager.Cancel called")
}

func (panicTaskManager) Send(context.Context, string, string) (SendOutcome, error) {
	panic("parallel_tools_test: TaskManager.Send called")
}

func (panicTaskManager) Output(context.Context, string, int) (TaskOutput, error) {
	panic("parallel_tools_test: TaskManager.Output called")
}
