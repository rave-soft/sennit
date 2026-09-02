package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

type mockEditFileTracker struct {
	lastRead time.Time
	reads    []string
	partial  FileCoverage
}

func (m *mockEditFileTracker) RecordRead(ctx context.Context, sessionID, path string) {
	m.reads = append(m.reads, path)
}

func (m *mockEditFileTracker) LastReadTime(ctx context.Context, sessionID, path string) time.Time {
	return m.lastRead
}

func (m *mockEditFileTracker) ListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	return m.reads, nil
}

func (m *mockEditFileTracker) RecordPartialRead(ctx context.Context, sessionID, path string, start, end int) {
	m.partial = m.partial.Add(FileLineRange{Start: start, End: end})
}

func (m *mockEditFileTracker) RecordEdit(ctx context.Context, sessionID, path string, start, end, newEnd int) {
	m.partial = m.partial.Shift(start, end, newEnd-end).Add(FileLineRange{Start: start, End: newEnd})
}

// ReadCoverage reports whatever coverage the test set up; the zero value
// is "fully read", which is what every test predating range tracking
// assumes.
func (m *mockEditFileTracker) ReadCoverage(ctx context.Context, sessionID, path string) FileCoverage {
	if m.partial.Empty() {
		return FileCoverage{Full: true}
	}
	return m.partial
}

func TestReplaceContentPreservesCRLFAndMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("alpha\r\nbeta\r\n"), 0o644))

	tracker := &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}
	edit := editContext{
		ctx:         context.WithValue(t.Context(), SessionIDContextKey, "session"),
		permissions: &mockPermissionService{},
		files:       &mockHistoryService{},
		filetracker: tracker,
		workingDir:  dir,
	}

	resp, err := replaceContent(edit, filePath, "beta", "BETA", false, fantasy.ToolCall{ID: "call"})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, "Content replaced in file: "+filePath, resp.Content)

	content, err := os.ReadFile(filePath)
	require.NoError(t, err)
	require.Equal(t, "alpha\r\nBETA\r\n", string(content))
	// An edit records the span it touched, not a whole-file read — see
	// recordEditedSpan.
	require.Empty(t, tracker.reads)
	require.True(t, tracker.ReadCoverage(t.Context(), "session", filePath).Covers(2, 2))

	var meta EditResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, "alpha\nbeta\n", meta.OldContent)
	require.Equal(t, "alpha\r\nBETA\r\n", meta.NewContent)
}

func TestDeleteContentRejectsMultipleMatchesWithoutReplaceAll(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("alpha\nbeta\nalpha\n"), 0o644))

	edit := editContext{
		ctx:         context.WithValue(t.Context(), SessionIDContextKey, "session"),
		permissions: &mockPermissionService{},
		files:       &mockHistoryService{},
		filetracker: &mockEditFileTracker{lastRead: time.Now().Add(time.Second)},
		workingDir:  dir,
	}

	resp, err := deleteContent(edit, filePath, "alpha\n", false, fantasy.ToolCall{ID: "call"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "old_string appears multiple times")

	content, err := os.ReadFile(filePath)
	require.NoError(t, err)
	require.Equal(t, "alpha\nbeta\nalpha\n", string(content))
}

// TestChangedLineSpan pins the span an edit is checked against. It is
// derived from before/after content rather than from old_string, so it
// holds for every path that produces an edit.
func TestChangedLineSpan(t *testing.T) {
	t.Parallel()

	const before = "one\ntwo\nthree\nfour\nfive\n"

	start, end, ok := changedLineSpan(before, "one\ntwo\nTHREE\nfour\nfive\n")
	require.True(t, ok)
	require.Equal(t, 3, start)
	require.Equal(t, 3, end)

	start, end, ok = changedLineSpan(before, "one\nTWO\nTHREE\nfour\nfive\n")
	require.True(t, ok)
	require.Equal(t, 2, start)
	require.Equal(t, 3, end)

	// A deletion spans the lines that disappeared.
	start, end, ok = changedLineSpan(before, "one\nfour\nfive\n")
	require.True(t, ok)
	require.Equal(t, 2, start)
	require.Equal(t, 3, end)

	// A pure insertion anchors on the line it lands against, so it still
	// requires that neighborhood to have been read.
	start, end, ok = changedLineSpan(before, "one\ntwo\ninserted\nthree\nfour\nfive\n")
	require.True(t, ok)
	require.Equal(t, 3, start)
	require.Equal(t, 3, end)

	_, _, ok = changedLineSpan(before, before)
	require.False(t, ok, "identical content is not a change")
}

// TestReplaceContentRejectsEditOutsideReadWindow is the hole this closes:
// reading the head of a long file used to authorize an edit anywhere in
// it, because the tracker only recorded that the file had been read at
// all.
func TestReplaceContentRejectsEditOutsideReadWindow(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "long.txt")
	lines := make([]string, 200)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	tracker := &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}
	tracker.RecordPartialRead(t.Context(), "session", filePath, 1, 50)
	edit := editContext{
		ctx:         context.WithValue(t.Context(), SessionIDContextKey, "session"),
		permissions: &mockPermissionService{},
		files:       &mockHistoryService{},
		filetracker: tracker,
		workingDir:  dir,
	}

	resp, err := replaceContent(edit, filePath, "line 190\n", "LINE 190\n", false, fantasy.ToolCall{ID: "call"})
	require.NoError(t, err)
	require.True(t, resp.IsError, "an edit outside the read window must be refused")
	require.Contains(t, resp.Content, "lines 190-190")
	require.Contains(t, resp.Content, "1-50", "the message names what was actually read")

	// The file is untouched.
	content, err := os.ReadFile(filePath)
	require.NoError(t, err)
	require.Contains(t, string(content), "line 190\n")
}

// TestReplaceContentAllowsEditInsideReadWindow is the other half: an edit
// within a window that was served goes through, so partial reads remain a
// usable way to work on a large file.
func TestReplaceContentAllowsEditInsideReadWindow(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "long.txt")
	lines := make([]string, 200)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	tracker := &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}
	tracker.RecordPartialRead(t.Context(), "session", filePath, 150, 200)
	edit := editContext{
		ctx:         context.WithValue(t.Context(), SessionIDContextKey, "session"),
		permissions: &mockPermissionService{},
		files:       &mockHistoryService{},
		filetracker: tracker,
		workingDir:  dir,
	}

	resp, err := replaceContent(edit, filePath, "line 190\n", "LINE 190\n", false, fantasy.ToolCall{ID: "call"})
	require.NoError(t, err)
	require.False(t, resp.IsError, "content: %s", resp.Content)

	content, err := os.ReadFile(filePath)
	require.NoError(t, err)
	require.Contains(t, string(content), "LINE 190\n")
}

// TestEditDoesNotWidenCoverageToWholeFile guards the trap in recording
// coverage on write: an edit is not a read. Writing the file used to mark
// it fully read, so reading fifty lines and editing one of them handed
// back the whole file — the exact hole range tracking closes. Only the
// edited span becomes covered.
func TestEditDoesNotWidenCoverageToWholeFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "long.txt")
	lines := make([]string, 200)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	tracker := &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}
	tracker.RecordPartialRead(t.Context(), "session", filePath, 1, 50)
	edit := editContext{
		ctx:         context.WithValue(t.Context(), SessionIDContextKey, "session"),
		permissions: &mockPermissionService{},
		files:       &mockHistoryService{},
		filetracker: tracker,
		workingDir:  dir,
	}

	resp, err := replaceContent(edit, filePath, "line 10\n", "LINE 10\n", false, fantasy.ToolCall{ID: "call"})
	require.NoError(t, err)
	require.False(t, resp.IsError, "content: %s", resp.Content)

	coverage := tracker.ReadCoverage(t.Context(), "session", filePath)
	require.False(t, coverage.Full, "an edit must not mark the whole file read")
	require.True(t, coverage.Covers(1, 50), "what was read stays read")
	require.False(t, coverage.Covers(190, 190), "what was never read stays unread")

	// And the follow-up blind edit is still refused.
	resp, err = replaceContent(edit, filePath, "line 190\n", "LINE 190\n", false, fantasy.ToolCall{ID: "call2"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "has not been read in this session")
}

// TestEditShiftsCoverageBelowIt proves coverage follows the text it stands
// for: an edit that adds lines near the top moves every range recorded
// below it, so a later edit far down is judged against the right lines.
func TestEditShiftsCoverageBelowIt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "long.txt")
	lines := make([]string, 200)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	tracker := &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}
	tracker.RecordPartialRead(t.Context(), "session", filePath, 1, 20)
	tracker.RecordPartialRead(t.Context(), "session", filePath, 100, 120)
	edit := editContext{
		ctx:         context.WithValue(t.Context(), SessionIDContextKey, "session"),
		permissions: &mockPermissionService{},
		files:       &mockHistoryService{},
		filetracker: tracker,
		workingDir:  dir,
	}

	// Replace one line with three: everything below moves down by two.
	resp, err := replaceContent(edit, filePath, "line 10\n", "a\nb\nc\n", false, fantasy.ToolCall{ID: "call"})
	require.NoError(t, err)
	require.False(t, resp.IsError, "content: %s", resp.Content)

	coverage := tracker.ReadCoverage(t.Context(), "session", filePath)
	require.True(t, coverage.Covers(102, 122), "the lower window followed its text")
	require.False(t, coverage.Covers(99, 99), "and did not stretch to cover new ground")
}
