package tools

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

// This file is the table-driven regression test named in TECHDEBT.md's
// "профилактика" entry, item 4: several tools were found to bypass the
// permission model one way or another, and a one-off fix per tool leaves
// the next one free to make the same mistake. toolClassifications below is
// the register every tool must appear in; toolClassificationsCoverAllTools
// makes an unclassified tool — one added to config.AllToolNames() but never
// given an entry here — a red test rather than a silent gap.
//
// writes/confined are judgment calls about a tool's *capability*, not
// about whether its current implementation happens to gate it correctly —
// that is what the behavioral tests below the table check. A tool is
// "writes" when running it can leave a filesystem or network side effect
// a user did not ask for line by line (a shell command, a file mutation, an
// outbound HTTP request, a worktree create/merge/remove); it is "confined"
// when that side effect is expressed as a path this package can compare
// against a confined workspace's boundary. Read-only queries (glob, grep,
// ls, the lsp_* lookups, task/thread status and list, ...) and in-process
// control actions with no filesystem or network footprint (todos, ask_parent,
// task_cancel, job_kill, ...) are "writes: false" — not because nothing
// happens when they run, but because they are not the class of escape this
// table exists to catch.
type toolClassification struct {
	name     string
	writes   bool
	confined bool
	// run, when non-nil, builds the tool with perms and workingDir and
	// issues one call whose sole state-changing target is target — an
	// absolute path for tools that take one, ignored otherwise. It is the
	// behavioral proof that writes/confined are not just an unenforced
	// claim in this table. Left nil for tools this package cannot
	// exercise without dependencies outside it (an LSP client, another
	// package's manager) — those are still listed and classified, just
	// not behaviorally re-proven here; see the doc comment above
	// toolsWithoutBehavioralCoverage.
	run func(t *testing.T, perms permission.Service, workingDir, target string) fantasy.ToolResponse
}

// toolsWithoutBehavioralCoverage names entries whose permission/confinement
// gating cannot be exercised from this package alone, and why:
//
//   - "agent", "agentic_fetch": constructed in internal/agent, not this
//     package (agent_tool.go, agentic_fetch_tool.go) — importing that
//     package back from a tools test would cycle.
//   - "lsp_rename", "lsp_replace_symbol": both reach their permission and
//     confinement checks only after resolveSymbol succeeds against a real
//     LSP client (see lsp_rename.go); there is no fake LSP client in this
//     package to drive that path without one. Both do call
//     confinementRefusal and requirePermission per a direct source read —
//     see lsp_rename.go and lsp_replace_symbol.go — this just is not
//     re-proven at runtime here.
var toolsWithoutBehavioralCoverage = map[string]string{
	"agent":               "built in internal/agent; importing it here would cycle",
	AgenticFetchToolName:  "built in internal/agent; importing it here would cycle",
	RenameToolName:        "reaches its gates only after a real LSP resolves the symbol",
	ReplaceSymbolToolName: "reaches its gates only after a real LSP resolves the symbol",
}

func toolClassifications() []toolClassification {
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	newBg := func() *shell.BackgroundShellManager { return shell.NewBackgroundShellManager() }

	return []toolClassification{
		{name: "agent", writes: true},
		{
			name: BashToolName, writes: true, confined: true,
			run: func(t *testing.T, perms permission.Service, workingDir, target string) fantasy.ToolResponse {
				tool := NewBashTool(perms, workingDir, attribution, "test-model", newBg())
				resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
					ID: "call-1", Name: BashToolName,
					Input: mustJSONInput(t, BashParams{Command: "cp x " + target}),
				})
				require.NoError(t, err)
				return resp
			},
		},
		{name: SennitInfoToolName, writes: false},
		{name: SennitLogsToolName, writes: false},
		{name: JobOutputToolName, writes: false},
		{name: JobKillToolName, writes: false},
		{
			name: DownloadToolName, writes: true, confined: true,
			run: func(t *testing.T, perms permission.Service, workingDir, target string) fantasy.ToolResponse {
				tool := NewDownloadTool(perms, workingDir, nil)
				resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
					ID: "call-1", Name: DownloadToolName,
					Input: mustJSONInput(t, DownloadParams{URL: "https://example.invalid/x", FilePath: target}),
				})
				require.NoError(t, err)
				return resp
			},
		},
		{
			name: EditToolName, writes: true, confined: true,
			run: func(t *testing.T, perms permission.Service, workingDir, target string) fantasy.ToolResponse {
				tool := NewEditTool(nil, perms, &mockHistoryService{}, mockFileTrackerService{}, workingDir)
				resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
					ID: "call-1", Name: EditToolName,
					Input: mustJSONInput(t, EditParams{FilePath: target, OldString: "original", NewString: "edited"}),
				})
				require.NoError(t, err)
				return resp
			},
		},
		{
			name: MultiEditToolName, writes: true, confined: true,
			run: func(t *testing.T, perms permission.Service, workingDir, target string) fantasy.ToolResponse {
				tool := NewMultiEditTool(nil, perms, &mockHistoryService{}, mockFileTrackerService{}, workingDir)
				resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
					ID: "call-1", Name: MultiEditToolName,
					Input: mustJSONInput(t, MultiEditParams{
						FilePath: target,
						Edits:    []MultiEditOperation{{OldString: "original", NewString: "edited"}},
					}),
				})
				require.NoError(t, err)
				return resp
			},
		},
		{name: DiagnosticsToolName, writes: false},
		{name: ReferencesToolName, writes: false},
		{name: LSPRestartToolName, writes: false},
		{name: SymbolsToolName, writes: false},
		{name: DefinitionToolName, writes: false},
		{name: CallHierarchyToolName, writes: false},
		{name: RenameToolName, writes: true, confined: true},
		{name: ReplaceSymbolToolName, writes: true, confined: true},
		{
			name: FetchToolName, writes: true,
			run: func(t *testing.T, perms permission.Service, workingDir, _ string) fantasy.ToolResponse {
				tool := NewFetchTool(perms, workingDir, nil)
				resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
					ID: "call-1", Name: FetchToolName,
					Input: mustJSONInput(t, FetchParams{URL: "https://example.invalid/x", Format: "text"}),
				})
				require.NoError(t, err)
				return resp
			},
		},
		{name: AgenticFetchToolName, writes: true},
		{
			name: WebFetchToolName, writes: true,
			run: func(t *testing.T, perms permission.Service, workingDir, _ string) fantasy.ToolResponse {
				tool := NewWebFetchTool(perms, workingDir, nil)
				resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
					ID: "call-1", Name: WebFetchToolName,
					Input: mustJSONInput(t, WebFetchParams{URL: "https://example.invalid/x"}),
				})
				require.NoError(t, err)
				return resp
			},
		},
		{
			name: WebSearchToolName, writes: true,
			run: func(t *testing.T, perms permission.Service, workingDir, _ string) fantasy.ToolResponse {
				tool := NewWebSearchTool(perms, workingDir, nil, nil)
				resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
					ID: "call-1", Name: WebSearchToolName,
					Input: mustJSONInput(t, WebSearchParams{Query: "test"}),
				})
				require.NoError(t, err)
				return resp
			},
		},
		{name: GlobToolName, writes: false},
		{name: GrepToolName, writes: false},
		{name: RipgrepToolName, writes: false},
		{name: LSToolName, writes: false},
		{name: QuestionToolName, writes: false},
		{name: TodosToolName, writes: false},
		{name: ReadToolName, writes: false},
		{
			name: WriteToolName, writes: true, confined: true,
			run: func(t *testing.T, perms permission.Service, workingDir, target string) fantasy.ToolResponse {
				tool := NewWriteTool(nil, perms, &mockHistoryService{}, mockFileTrackerService{}, workingDir)
				resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
					ID: "call-1", Name: WriteToolName,
					Input: mustJSONInput(t, WriteParams{FilePath: target, Content: "overwritten\n"}),
				})
				require.NoError(t, err)
				return resp
			},
		},
		{name: ListMCPResourcesToolName, writes: false},
		{name: ReadMCPResourceToolName, writes: false},
		{
			name: ThreadCreateToolName, writes: true,
			run: func(t *testing.T, perms permission.Service, _, _ string) fantasy.ToolResponse {
				tool := NewThreadCreateTool(panicThreadManager{}, perms)
				resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
					ID: "call-1", Name: ThreadCreateToolName,
					Input: mustJSONInput(t, ThreadCreateParams{Name: "t1", Goal: "do a thing"}),
				})
				require.NoError(t, err)
				return resp
			},
		},
		{name: ThreadListToolName, writes: false},
		{name: ThreadStatusToolName, writes: false},
		{name: ThreadWaitToolName, writes: false},
		{
			name: ThreadMergeToolName, writes: true,
			run: func(t *testing.T, perms permission.Service, _, _ string) fantasy.ToolResponse {
				tool := NewThreadMergeTool(panicThreadManager{}, perms)
				resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
					ID: "call-1", Name: ThreadMergeToolName,
					Input: mustJSONInput(t, ThreadMergeParams{ID: "t1"}),
				})
				require.NoError(t, err)
				return resp
			},
		},
		{
			name: ThreadRemoveToolName, writes: true,
			run: func(t *testing.T, perms permission.Service, _, _ string) fantasy.ToolResponse {
				tool := NewThreadRemoveTool(panicThreadManager{}, perms)
				resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
					ID: "call-1", Name: ThreadRemoveToolName,
					Input: mustJSONInput(t, ThreadRemoveParams{ID: "t1"}),
				})
				require.NoError(t, err)
				return resp
			},
		},
		{name: TaskListToolName, writes: false},
		{name: TaskResultToolName, writes: false},
		{name: TaskCancelToolName, writes: false},
		{name: TaskSendToolName, writes: false},
		{name: TaskOutputToolName, writes: false},
		{name: AskParentToolName, writes: false},
	}
}

// panicThreadManager is a ThreadManager whose every method panics. It is
// only ever handed to a tool alongside a permission service that denies the
// request, so none of these should be reached — a panic here means the
// tool under test called into the manager before honoring the denial.
type panicThreadManager struct{}

func (panicThreadManager) Create(context.Context, ThreadCreateArgs) (ThreadInfo, error) {
	panic("permission_coverage_test: ThreadManager reached despite a denied request")
}

func (panicThreadManager) List(context.Context) ([]ThreadInfo, error) {
	panic("permission_coverage_test: ThreadManager reached despite a denied request")
}

func (panicThreadManager) Get(context.Context, string) (ThreadInfo, error) {
	panic("permission_coverage_test: ThreadManager reached despite a denied request")
}

func (panicThreadManager) Send(context.Context, string, string) (SendOutcome, error) {
	panic("permission_coverage_test: ThreadManager reached despite a denied request")
}

func (panicThreadManager) Wait(context.Context, []string, time.Duration) error {
	panic("permission_coverage_test: ThreadManager reached despite a denied request")
}

func (panicThreadManager) Merge(context.Context, string) (ThreadInfo, error) {
	panic("permission_coverage_test: ThreadManager reached despite a denied request")
}

func (panicThreadManager) Remove(context.Context, string, bool, bool) error {
	panic("permission_coverage_test: ThreadManager reached despite a denied request")
}

// denyingPermissions (filemutation_test.go) already refuses every request
// and reports no confinement boundary, which is exactly the deny-side
// counterpart this file needs to confinedTestPermissions
// (confinement_test.go, which always grants) — reused here rather than
// redeclared.

// seedFileInside creates a fresh directory with one file in it containing
// "original\n", so an edit/multiedit target has something to match against.
func seedFileInside(t *testing.T) (workingDir, target string) {
	t.Helper()
	workingDir = t.TempDir()
	target = filepath.Join(workingDir, "in.txt")
	require.NoError(t, os.WriteFile(target, []byte("original\n"), 0o644))
	return workingDir, target
}

// TestToolClassificationsCoverAllRegisteredTools is the automatic half of
// this file: config.AllToolNames() is the same registry the agent actually
// builds its tool set from (internal/agent/tool_registry.go reads it
// indirectly via the allowed/disabled-tools resolution, and internal/ui
// uses the same list to verify every tool renders). Any tool name present
// there but missing from toolClassifications — or a stale name here that
// no longer exists there — fails this test, so a new tool ships with a
// writes/confined judgment call on record instead of silently skipping it.
func TestToolClassificationsCoverAllRegisteredTools(t *testing.T) {
	t.Parallel()

	registered := config.AllToolNames()
	var classified []string
	for _, c := range toolClassifications() {
		classified = append(classified, c.name)
	}

	for _, name := range registered {
		require.True(t, slices.Contains(classified, name),
			"tool %q is registered but has no entry in toolClassifications; classify it (writes/confined) before merging", name)
	}
	for _, name := range classified {
		require.True(t, slices.Contains(registered, name),
			"toolClassifications has an entry %q that config.AllToolNames() does not know; remove it or fix the name", name)
	}
}

// TestWritingToolsRequirePermission is the behavioral proof for every
// classification with writes: true and a run func: denied permission must
// stop the tool before it does anything, for every tool this package can
// construct on its own.
func TestWritingToolsRequirePermission(t *testing.T) {
	t.Parallel()

	for _, c := range toolClassifications() {
		if !c.writes {
			continue
		}
		if c.run == nil {
			if _, ok := toolsWithoutBehavioralCoverage[c.name]; !ok {
				t.Fatalf("tool %q is writes:true with no run func and no entry in toolsWithoutBehavioralCoverage explaining why", c.name)
			}
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			workingDir, target := seedFileInside(t)
			resp := c.run(t, denyingPermissions{}, workingDir, target)
			require.True(t, resp.IsError, "a denied permission request must stop %q, not run it", c.name)
		})
	}
}

// TestConfinedWritingToolsRefuseOutsideTargets is the confinement half:
// for every classification with confined: true, a target outside the
// boundary must be refused even though permission is granted (yolo) — the
// same shape TestBashTool_ConfinedWorkspaceRefusesAn*Outside and its
// siblings in confinement_test.go pin per tool, run here once for every
// tool the table says is confined so a future tool cannot skip it by
// omission.
func TestConfinedWritingToolsRefuseOutsideTargets(t *testing.T) {
	t.Parallel()

	for _, c := range toolClassifications() {
		if !c.confined {
			continue
		}
		if c.run == nil {
			if _, ok := toolsWithoutBehavioralCoverage[c.name]; !ok {
				t.Fatalf("tool %q is confined:true with no run func and no entry in toolsWithoutBehavioralCoverage explaining why", c.name)
			}
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			workingDir, outside, perms := writeOutsideAttempt(t)
			resp := c.run(t, perms, workingDir, outside)
			require.True(t, resp.IsError, "a target outside the confined workspace must be refused by %q", c.name)
			require.Contains(t, resp.Content, "outside this workspace")
		})
	}
}
