package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"mvdan.cc/sh/v3/syntax"
)

// mustJSONInput marshals v the way the agent runtime encodes tool-call
// arguments, so a path containing a Windows drive letter and backslashes
// round-trips through the same decoder the tool uses. Hand-built JSON
// strings that splice a path in with `+` break on Windows: `\U` in
// `C:\Users\...` is not a legal JSON escape, so the input never reaches the
// confinement check the test is trying to exercise.
func mustJSONInput(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// confinedTestPermissions is a permission service that grants everything —
// exactly like a workspace running under yolo, which is what a thread
// inherits from the main agent — while reporting a confinement boundary.
// The point of these tests is that the boundary holds when the permission
// flow has stopped asking anything.
type confinedTestPermissions struct {
	permission.Service
	dir string
}

func (p *confinedTestPermissions) Request(context.Context, permission.CreatePermissionRequest) (bool, error) {
	return true, nil
}

type recordingConfinedPermissions struct {
	*confinedTestPermissions
	requests []permission.CreatePermissionRequest
}

func (p *recordingConfinedPermissions) Request(_ context.Context, request permission.CreatePermissionRequest) (bool, error) {
	p.requests = append(p.requests, request)
	return false, nil
}
func (p *confinedTestPermissions) ConfinedDir() string  { return p.dir }
func (p *confinedTestPermissions) ConfineToWorkingDir() {}

// confinedTestCtx carries the session id the writing tools require.
func confinedTestCtx(t *testing.T) context.Context {
	t.Helper()
	return context.WithValue(t.Context(), SessionIDContextKey, "test-session")
}

// writeOutsideAttempt sets up a confined workspace and a file that lives
// outside it, and returns the tool call payload aimed at that outside file.
func writeOutsideAttempt(t *testing.T) (workdir, outside string, perms *confinedTestPermissions) {
	t.Helper()
	root := t.TempDir()
	workdir = filepath.Join(root, "worktree")
	require.NoError(t, os.MkdirAll(workdir, 0o755))
	elsewhere := filepath.Join(root, "main-checkout")
	require.NoError(t, os.MkdirAll(elsewhere, 0o755))
	outside = filepath.Join(elsewhere, "leaked.go")
	require.NoError(t, os.WriteFile(outside, []byte("original\n"), 0o644))
	return workdir, outside, &confinedTestPermissions{dir: workdir}
}

// TestWriteTool_ConfinedWorkspaceRefusesAnAbsolutePathOutside is the
// regression test for work escaping a thread. Two threads each spent an
// hour producing changes that landed in the main checkout instead of their
// own worktree: their branches stayed empty, and the main tree was left
// holding half-finished work from two unrelated tasks at once.
//
// An absolute path is the whole mechanism — the file tools join a relative
// path onto the working directory but pass an absolute one straight
// through. Permission was never the barrier here: threads inherit yolo, so
// everything is granted, which is exactly what this stub reproduces.
func TestWriteTool_ConfinedWorkspaceRefusesAnAbsolutePathOutside(t *testing.T) {
	t.Parallel()

	workdir, outside, perms := writeOutsideAttempt(t)
	tool := NewWriteTool(nil, perms, &mockHistoryService{}, mockFileTrackerService{}, workdir)

	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  WriteToolName,
		Input: mustJSONInput(t, WriteParams{FilePath: outside, Content: "overwritten\n"}),
	})
	require.NoError(t, err)
	require.True(t, resp.IsError, "the write must be refused, not performed")
	require.Contains(t, resp.Content, "outside this workspace")

	onDisk, err := os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, "original\n", string(onDisk), "the file outside must be untouched")
}

// TestEditTool_ConfinedWorkspaceRefusesAnAbsolutePathOutside: same for
// edits, which is how most of the escaped work was actually written.
func TestEditTool_ConfinedWorkspaceRefusesAnAbsolutePathOutside(t *testing.T) {
	t.Parallel()

	workdir, outside, perms := writeOutsideAttempt(t)
	tool := NewEditTool(nil, perms, &mockHistoryService{}, mockFileTrackerService{}, workdir)

	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  EditToolName,
		Input: mustJSONInput(t, EditParams{FilePath: outside, OldString: "original", NewString: "edited"}),
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "outside this workspace")

	onDisk, err := os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, "original\n", string(onDisk))
}

// TestMultiEditTool_ConfinedWorkspaceRefusesAnAbsolutePathOutside covers
// the third writing tool, so the boundary is not one a model can walk
// around by choosing a different tool.
func TestMultiEditTool_ConfinedWorkspaceRefusesAnAbsolutePathOutside(t *testing.T) {
	t.Parallel()

	workdir, outside, perms := writeOutsideAttempt(t)
	tool := NewMultiEditTool(nil, perms, &mockHistoryService{}, mockFileTrackerService{}, workdir)

	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:   "call-1",
		Name: MultiEditToolName,
		Input: mustJSONInput(t, MultiEditParams{
			FilePath: outside,
			Edits:    []MultiEditOperation{{OldString: "original", NewString: "edited"}},
		}),
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "outside this workspace")

	onDisk, err := os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, "original\n", string(onDisk))
}

// TestConfinedWorkspaceStillWritesInsideItself: the boundary must not cost
// a thread the ability to do its job. An absolute path into its own
// worktree is ordinary and stays allowed.
func TestConfinedWorkspaceStillWritesInsideItself(t *testing.T) {
	t.Parallel()

	workdir, _, perms := writeOutsideAttempt(t)
	target := filepath.Join(workdir, "internal", "thing.go")
	tool := NewWriteTool(nil, perms, &mockHistoryService{}, mockFileTrackerService{}, workdir)

	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  WriteToolName,
		Input: mustJSONInput(t, WriteParams{FilePath: target, Content: "package thing\n"}),
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, "writing inside the workspace must still work: %s", resp.Content)
	require.FileExists(t, target)
}

// TestUnconfinedWorkspaceIsUnaffected: the main workspace is not a thread.
// Reaching outside its working directory is a real, permissioned
// capability there, and this change must not quietly take it away.
func TestUnconfinedWorkspaceIsUnaffected(t *testing.T) {
	t.Parallel()

	workdir, outside, perms := writeOutsideAttempt(t)
	perms.dir = "" // not confined
	tool := NewWriteTool(nil, perms, &mockHistoryService{}, mockFileTrackerService{}, workdir)

	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  WriteToolName,
		Input: mustJSONInput(t, WriteParams{FilePath: outside, Content: "allowed\n"}),
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, "an unconfined workspace may still write outside once permitted: %s", resp.Content)

	onDisk, err := os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, "allowed\n", string(onDisk))
}

// TestConfinementRefusal_NamesWhereItShouldHaveGone: the refusal is also a
// correction. A model that has just been told "no" needs to know the file
// it wants exists inside its own worktree, or it will try the same path
// again by another route.
func TestConfinementRefusal_NamesWhereItShouldHaveGone(t *testing.T) {
	t.Parallel()

	workdir, outside, perms := writeOutsideAttempt(t)
	msg, refused := confinementRefusal(perms, outside)
	require.True(t, refused)
	require.Contains(t, msg, outside, "the refused path")
	require.Contains(t, msg, workdir, "and where the write belongs")
	require.True(t, strings.Contains(msg, "isolated"))
}

// TestConfinedWorkspace_PathWithBackslashSurvivesJSONEncoding reproduces the
// Windows JSON-escaping bug on Linux, deterministically: a path containing
// `\U` — exactly what a Windows "C:\Users\..." path contributes — is not a
// legal JSON escape when spliced into a hand-built `"file_path":"..."`
// string. json.Marshal (via mustJSONInput/WriteParams) must escape the
// backslash so the tool decodes the path byte-for-byte and still applies
// the confinement check; naive string concatenation instead breaks
// unmarshalling and never reaches that check at all.
func TestConfinedWorkspace_PathWithBackslashSurvivesJSONEncoding(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workdir := filepath.Join(root, "worktree")
	require.NoError(t, os.MkdirAll(workdir, 0o755))
	elsewhere := filepath.Join(root, "main-checkout")
	require.NoError(t, os.MkdirAll(elsewhere, 0o755))
	// A literal backslash is a normal filename byte on Linux/macOS, but
	// "\U" is exactly the invalid JSON escape a bare "C:\Users\..." path
	// produces when concatenated into a JSON string by hand.
	//
	// On Windows the backslash *is* the path separator, so this same
	// literal names a file "Users.go" inside a "leaked" subdirectory —
	// create that subdirectory too, or the seed write below fails before
	// the JSON round trip this test exists to check is ever exercised.
	require.NoError(t, os.MkdirAll(filepath.Join(elsewhere, "leaked"), 0o755))
	outside := filepath.Join(elsewhere, `leaked\Users.go`)
	require.NoError(t, os.WriteFile(outside, []byte("original\n"), 0o644))
	perms := &confinedTestPermissions{dir: workdir}

	tool := NewWriteTool(nil, perms, &mockHistoryService{}, mockFileTrackerService{}, workdir)
	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  WriteToolName,
		Input: mustJSONInput(t, WriteParams{FilePath: outside, Content: "overwritten\n"}),
	})
	require.NoError(t, err, "a properly JSON-encoded path must parse, not fail before the confinement check runs")
	require.True(t, resp.IsError, "the write must be refused, not performed")
	require.Contains(t, resp.Content, "outside this workspace")
	require.Contains(t, resp.Content, outside, "the backslash in the path must survive the JSON round trip intact")
}

// TestDownloadTool_ConfinedWorkspaceRefusesAnAbsolutePathOutside: download
// writes a file too, so the same boundary applies — otherwise a thread
// could route its escape through a URL instead of write/edit. The refusal
// happens before any network I/O, so no server is needed here.
func TestDownloadTool_ConfinedWorkspaceRefusesAnAbsolutePathOutside(t *testing.T) {
	t.Parallel()

	workdir, outside, perms := writeOutsideAttempt(t)
	tool := NewDownloadTool(perms, workdir, nil)

	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  DownloadToolName,
		Input: mustJSONInput(t, DownloadParams{URL: "https://example.invalid/x", FilePath: outside}),
	})
	require.NoError(t, err)
	require.True(t, resp.IsError, "the download must be refused, not performed")
	require.Contains(t, resp.Content, "outside this workspace")

	onDisk, err := os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, "original\n", string(onDisk), "the file outside must be untouched")
}

// TestBashTool_ConfinedWorkspaceRefusesAWorkingDirOutside: bash cannot be
// confined in what a command touches, but a confined workspace at least
// refuses to root a command outside itself.
func TestBashTool_ConfinedWorkspaceRefusesAWorkingDirOutside(t *testing.T) {
	t.Parallel()

	workdir, outside, perms := writeOutsideAttempt(t)
	tool := NewBashTool(perms, workdir, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test-model", shell.NewBackgroundShellManager())

	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  BashToolName,
		Input: mustJSONInput(t, BashParams{Command: "pwd", WorkingDir: filepath.Dir(outside)}),
	})
	require.NoError(t, err)
	require.True(t, resp.IsError, "running outside the workspace must be refused")
	require.Contains(t, resp.Content, "outside this workspace")
}

// TestBashTool_ConfinedWorkspaceRefusesAnAbsoluteArgumentOutside closes the
// gap the previous test's own doc comment named: a command rooted inside
// the boundary can still name an absolute path outside it as a plain
// argument, e.g. a destination for `cp`.
func TestBashTool_ConfinedWorkspaceRefusesAnAbsoluteArgumentOutside(t *testing.T) {
	t.Parallel()

	workdir, outside, perms := writeOutsideAttempt(t)
	tool := NewBashTool(perms, workdir, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test-model", shell.NewBackgroundShellManager())

	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  BashToolName,
		Input: mustJSONInput(t, BashParams{Command: "cp x " + outside}),
	})
	require.NoError(t, err)
	require.True(t, resp.IsError, "an absolute-path argument outside the workspace must be refused")
	require.Contains(t, resp.Content, "outside this workspace")
	require.Contains(t, resp.Content, outside, "the refusal must name the offending path")
}

// TestBashTool_ConfinedWorkspaceRefusesAnAbsoluteRedirectOutside: the other
// half of the same gap — a redirect target, not a bare argument.
func TestBashTool_ConfinedWorkspaceRefusesAnAbsoluteRedirectOutside(t *testing.T) {
	t.Parallel()

	workdir, outside, perms := writeOutsideAttempt(t)
	tool := NewBashTool(perms, workdir, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test-model", shell.NewBackgroundShellManager())

	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  BashToolName,
		Input: mustJSONInput(t, BashParams{Command: "echo overwritten > " + outside}),
	})
	require.NoError(t, err)
	require.True(t, resp.IsError, "an absolute redirect target outside the workspace must be refused")
	require.Contains(t, resp.Content, "outside this workspace")
	require.Contains(t, resp.Content, outside)

	onDisk, err := os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, "original\n", string(onDisk), "the redirect must never have run")
}

// TestBashTool_ConfinedWorkspaceAllowsLegitimateCommandInsideBoundary is
// the false-positive check: a command that only ever touches paths inside
// the workspace — including an absolute one that happens to resolve
// inside it, and a command invoked by its absolute binary path — must run
// exactly as it would unconfined. Refusing this would make the confined
// workspace unusable for real work.
func TestBashTool_ConfinedWorkspaceAllowsLegitimateCommandInsideBoundary(t *testing.T) {
	t.Parallel()

	workdir, _, perms := writeOutsideAttempt(t)
	target := filepath.Join(workdir, "out.txt")
	tool := NewBashTool(perms, workdir, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test-model", shell.NewBackgroundShellManager())

	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  BashToolName,
		Input: mustJSONInput(t, BashParams{Command: "echo hello > " + target}),
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, "a command writing only inside the workspace must not be refused: %s", resp.Content)

	onDisk, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "hello\n", string(onDisk))
}

func TestBashTool_ConfinedWorkspaceDynamicPathsRequirePermission(t *testing.T) {
	t.Parallel()

	workdir, outside, basePermissions := writeOutsideAttempt(t)
	permissions := &recordingConfinedPermissions{confinedTestPermissions: basePermissions}
	tool := NewBashTool(permissions, workdir, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test-model", shell.NewBackgroundShellManager())

	commands := []string{
		"echo $OUT",
		"echo ${OUT}",
		"echo $(pwd)",
		"echo `pwd`",
		"echo *.go",
		"echo value > $OUT",
		"echo value > *.txt",
		"echo value > \"$(pwd)/out.txt\"",
		"echo value 3>$OUT",
	}
	for index, command := range commands {
		resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
			ID:    fmt.Sprintf("call-%d", index),
			Name:  BashToolName,
			Input: mustJSONInput(t, BashParams{Command: command}),
		})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		require.NotContains(t, resp.Content, "outside this workspace")
	}

	require.Len(t, permissions.requests, len(commands))
	for _, request := range permissions.requests {
		require.Contains(t, request.Description, "best-effort")
	}
	onDisk, err := os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, "original\n", string(onDisk))
}

func TestBashTool_ConfinedWorkspaceLiteralGlobCharactersDoNotRequirePermission(t *testing.T) {
	t.Parallel()

	workdir, _, basePermissions := writeOutsideAttempt(t)
	permissions := &recordingConfinedPermissions{confinedTestPermissions: basePermissions}
	tool := NewBashTool(permissions, workdir, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test-model", shell.NewBackgroundShellManager())

	for index, command := range []string{"echo '*'", `echo "?"`, `echo \*`, `echo "["`, `echo "prefix"suffix`} {
		resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
			ID:    fmt.Sprintf("call-%d", index),
			Name:  BashToolName,
			Input: mustJSONInput(t, BashParams{Command: command}),
		})
		require.NoError(t, err)
		require.False(t, resp.IsError)
	}

	require.Empty(t, permissions.requests)
}

// TestBashTool_ConfinedWorkspaceAllowsReadOnlyCommandsToReadOutside: the
// boundary keeps changes in, it does not keep the thread from looking out
// — view/grep already read anywhere, and bash refusing `cat /etc/hosts` or
// `git diff --no-index /tmp/x f` (the model's way of showing a new file
// whole) only made the thread route around the refusal. Arguments to a
// known read-only command are not checked; /dev/null is never outside.
// callArgs parses command's first statement as a simple command and
// returns its args, for tests that exercise readsOnlyFromArgs directly.
func callArgs(t *testing.T, command string) []*syntax.Word {
	t.Helper()
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	require.NoError(t, err)
	require.Len(t, file.Stmts, 1)
	call, ok := file.Stmts[0].Cmd.(*syntax.CallExpr)
	require.True(t, ok)
	return call.Args
}

// TestReadsOnlyFromArgs_OutputFlagsDisqualify is the regression test for a
// git invocation whose subcommand only inspects the repository but whose
// arguments name a write target or a program to run: `git diff
// --output=path` truncates and writes path, and `git grep -O cmd` runs cmd
// on every matched file. Neither is read-only, so readsOnlyFromArgs must
// not wave either past the confinement check.
func TestReadsOnlyFromArgs_OutputFlagsDisqualify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"git diff plain", "git diff --stat HEAD", true},
		{"git grep plain", "git grep -n pat", true},
		{"git diff --output", "git diff --output=../x HEAD", false},
		{"git log --output", "git log --output=../x -1", false},
		{"git show --output", "git show --output=x HEAD", false},
		{"git grep -O", "git grep -O cmd pat", false},
		{"git grep --open-files-in-pager", "git grep --open-files-in-pager=cmd pat", false},
		// A non-literal argument (a variable here) might itself expand
		// to "--output=..." or "-O": this must fail closed, not be
		// skipped as if it were harmless.
		{"git diff with non-literal argument", "git diff --stat $FLAG HEAD", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := readsOnlyFromArgs(callArgs(t, tt.command))
			assert.Equal(t, tt.want, got, "readsOnlyFromArgs(%q)", tt.command)
		})
	}
}

// TestReadsOnlyFromArgs_YqAndTreeCanWrite is the regression test for two
// commands that used to sit in readOnlyCommands despite having a writing
// mode. yq is removed outright, the same treatment sed -i and sort -o
// already get, since it has no ordinary read-only-heavy use worth a gate:
// even a plain `yq .` is no longer treated as read-only. tree keeps its
// entry — plain `tree` is common enough to be worth keeping read-only —
// but `tree -o` writes its listing to a file, so that one flag is gated
// via writeDisqualifyingFlags. jq, which has no in-place flag, is
// unaffected either way.
func TestReadsOnlyFromArgs_YqAndTreeCanWrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"yq -i writes in place", "yq -i .a=1 /outside/f.yaml", false},
		{"yq plain read is no longer read-only", "yq . f.yaml", false},
		{"tree -o writes output", "tree -o /outside/f", false},
		{"tree plain read", "tree", true},
		{"jq plain read stays read-only", "jq . f.json", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := readsOnlyFromArgs(callArgs(t, tt.command))
			assert.Equal(t, tt.want, got, "readsOnlyFromArgs(%q)", tt.command)
		})
	}
}

func TestBashTool_ConfinedWorkspaceAllowsReadOnlyCommandsToReadOutside(t *testing.T) {
	t.Parallel()

	workdir, outside, perms := writeOutsideAttempt(t)
	inside := filepath.Join(workdir, "f.txt")
	require.NoError(t, os.WriteFile(inside, []byte("inside\n"), 0o644))
	tool := NewBashTool(perms, workdir, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test-model", shell.NewBackgroundShellManager())

	for _, command := range []string{
		"cat " + outside,
		"diff " + outside + " " + inside + " || true",
		"git --no-pager diff --no-index -- " + outside + " " + inside + " || true",
		"git diff --no-index /dev/null " + inside + " || true",
		"diff /dev/null " + inside + " || true",
		"cat " + inside + " > /dev/null",
		"/usr/bin/wc -l " + outside,
	} {
		resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
			ID:    "call-1",
			Name:  BashToolName,
			Input: mustJSONInput(t, BashParams{Command: command}),
		})
		require.NoError(t, err)
		require.False(t, resp.IsError, "%q only reads and must not be refused: %s", command, resp.Content)
	}

	onDisk, err := os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, "original\n", string(onDisk))
}

// TestBashTool_ConfinedWorkspaceReadOnlyAllowanceIsNarrow pins the edges of
// that allowance: a redirect out of a read-only command still writes and
// is still refused; a git invocation repointed at another repository (-C)
// or a writing subcommand gets no allowance; and the refusal now says what
// is refused and what to do instead.
func TestBashTool_ConfinedWorkspaceReadOnlyAllowanceIsNarrow(t *testing.T) {
	t.Parallel()

	workdir, outside, perms := writeOutsideAttempt(t)
	tool := NewBashTool(perms, workdir, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test-model", shell.NewBackgroundShellManager())

	for _, command := range []string{
		"cat x > " + outside,
		"git -C " + filepath.Dir(outside) + " diff",
		"git worktree add " + outside,
		"cp x " + outside,
	} {
		resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
			ID:    "call-1",
			Name:  BashToolName,
			Input: mustJSONInput(t, BashParams{Command: command}),
		})
		require.NoError(t, err)
		require.True(t, resp.IsError, "%q can write outside and must be refused", command)
		require.Contains(t, resp.Content, "outside this workspace")
		require.Contains(t, resp.Content, "git diff --no-index /dev/null", "the refusal must say what to do instead")
	}

	onDisk, err := os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, "original\n", string(onDisk))
}
