package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/permission"
	"github.com/stretchr/testify/require"
)

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
		Input: `{"file_path":"` + outside + `","content":"overwritten\n"}`,
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
		Input: `{"file_path":"` + outside + `","old_string":"original","new_string":"edited"}`,
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
		ID:    "call-1",
		Name:  MultiEditToolName,
		Input: `{"file_path":"` + outside + `","edits":[{"old_string":"original","new_string":"edited"}]}`,
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
		Input: `{"file_path":"` + target + `","content":"package thing\n"}`,
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
		Input: `{"file_path":"` + outside + `","content":"allowed\n"}`,
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
