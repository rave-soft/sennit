package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/stretchr/testify/require"
)

func newMultiReadToolForTest(dir string) fantasy.AgentTool {
	return NewMultiReadTool(&mockReadPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}, mockFileTracker{}, dir)
}

func runMultiRead(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params MultiReadParams) (fantasy.ToolResponse, MultiReadResponse) {
	t.Helper()
	response, err := tool.Run(ctx, fantasy.ToolCall{ID: "multi-read-test", Name: MultiReadToolName, Input: mustJSONInput(t, params)})
	require.NoError(t, err)
	var metadata MultiReadResponse
	if response.Metadata != "" {
		require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
	}
	return response, metadata
}

func multiReadContext() context.Context {
	return context.WithValue(context.Background(), SessionIDContextKey, "session")
}

func TestMultiReadBudgetContinuationPreservesFileSequence(t *testing.T) {
	dir := t.TempDir()
	large := filepath.Join(dir, "large.txt")
	next := filepath.Join(dir, "next.txt")
	lines := make([]string, 12)
	for i := range lines {
		lines[i] = strings.Repeat(string(rune('a'+i)), 12)
	}
	require.NoError(t, os.WriteFile(large, []byte(strings.Join(lines, "\n")), 0o644))
	require.NoError(t, os.WriteFile(next, []byte("next-file"), 0o644))

	tool := newMultiReadToolForTest(dir)
	// Relative paths, resolved against the tool's working directory.
	// The budget below has to leave room for the per-file framing, and
	// that framing embeds the path *as passed* — so absolute t.TempDir()
	// paths made the test's arithmetic depend on how long the platform's
	// temp directory happens to be. It fit under Linux's /tmp/... and did
	// not under macOS's /var/folders/... or Windows' AppData path, where
	// the framing alone overran 150 bytes and multi_read correctly
	// refused the whole call with "budget too small for one line".
	params := MultiReadParams{Files: []MultiReadItem{{FilePath: "large.txt"}, {FilePath: "next.txt"}}, MaxBytes: 150}
	var all strings.Builder
	for page := 0; ; page++ {
		response, metadata := runMultiRead(t, tool, multiReadContext(), params)
		require.False(t, response.IsError)
		require.LessOrEqual(t, len(response.Content), params.MaxBytes)
		all.WriteString(response.Content)
		if !metadata.Truncated {
			break
		}
		require.NotEmpty(t, metadata.Cursor)
		if page == 0 {
			require.Equal(t, 0, metadata.NextIndex, "budget continuation remains on the large file")
		}
		params.Cursor = metadata.Cursor
	}
	for _, line := range lines {
		require.Equal(t, 1, strings.Count(all.String(), line), "line must occur exactly once")
	}
	require.Equal(t, 1, strings.Count(all.String(), "next-file"))
}

func TestMultiReadItemLimitContinuesToFollowingFiles(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first.txt"), filepath.Join(dir, "second.txt")
	require.NoError(t, os.WriteFile(first, []byte("one\ntwo\nthree"), 0o644))
	require.NoError(t, os.WriteFile(second, []byte("second"), 0o644))
	response, metadata := runMultiRead(t, newMultiReadToolForTest(dir), multiReadContext(), MultiReadParams{Files: []MultiReadItem{{FilePath: first, Limit: 1}, {FilePath: second}}, MaxBytes: 1000})
	require.False(t, response.IsError)
	require.Len(t, metadata.Files, 2)
	require.True(t, metadata.Files[0].Truncated)
	require.False(t, metadata.Truncated)
	require.Contains(t, response.Content, "second")
}

func TestMultiReadMixedMissingAndSuccess(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present.txt")
	require.NoError(t, os.WriteFile(present, []byte("available"), 0o644))
	response, metadata := runMultiRead(t, newMultiReadToolForTest(dir), multiReadContext(), MultiReadParams{Files: []MultiReadItem{{FilePath: "missing.txt"}, {FilePath: present}}})
	require.False(t, response.IsError)
	require.Equal(t, []string{"error", "ok"}, []string{metadata.Files[0].Status, metadata.Files[1].Status})
	require.Contains(t, metadata.Files[0].Error, "File not found")
	require.Contains(t, response.Content, "available")
}

type denyingMultiReadPermissions struct {
	*mockReadPermissionService
	mu       sync.Mutex
	requests []permission.CreatePermissionRequest
}

func (p *denyingMultiReadPermissions) Request(_ context.Context, request permission.CreatePermissionRequest) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, request)
	return false, nil
}

func TestMultiReadPermissionDenialStopsBeforeNextRead(t *testing.T) {
	root := t.TempDir()
	workingDir := filepath.Join(root, "work")
	require.NoError(t, os.Mkdir(workingDir, 0o755))
	outside := filepath.Join(root, "outside.txt")
	second := filepath.Join(workingDir, "second.txt")
	require.NoError(t, os.WriteFile(outside, []byte("denied"), 0o644))
	require.NoError(t, os.WriteFile(second, []byte("must-not-read"), 0o644))
	permissions := &denyingMultiReadPermissions{mockReadPermissionService: &mockReadPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}}
	tracker := &recordingReadTracker{}
	tool := NewMultiReadTool(permissions, tracker, workingDir)

	response, _ := runMultiRead(t, tool, multiReadContext(), MultiReadParams{Files: []MultiReadItem{{FilePath: outside}, {FilePath: second}}})
	require.True(t, response.IsError)
	require.True(t, response.StopTurn)
	require.Len(t, permissions.requests, 1)
	require.Equal(t, MultiReadToolName, permissions.requests[0].ToolName)
	require.Empty(t, tracker.ranges)
	require.Empty(t, tracker.full)
}

type trackedRange struct {
	path       string
	start, end int
}

type recordingReadTracker struct {
	mu     sync.Mutex
	full   []string
	ranges []trackedRange
}

func (t *recordingReadTracker) RecordRead(_ context.Context, _ string, path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.full = append(t.full, path)
}

func (*recordingReadTracker) LastReadTime(context.Context, string, string) time.Time {
	return time.Time{}
}

func (*recordingReadTracker) ListReadFiles(context.Context, string) ([]string, error) {
	return nil, nil
}

func (t *recordingReadTracker) RecordPartialRead(_ context.Context, _ string, path string, start, end int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ranges = append(t.ranges, trackedRange{path, start, end})
}
func (*recordingReadTracker) RecordEdit(context.Context, string, string, int, int, int) {}
func (*recordingReadTracker) ReadCoverage(context.Context, string, string) FileCoverage {
	return FileCoverage{}
}

func TestMultiReadFileTrackerRecordsExactRangesAcrossPages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "paged.txt")
	require.NoError(t, os.WriteFile(path, []byte("1111111111\n2222222222\n3333333333\n4444444444"), 0o644))
	tracker := &recordingReadTracker{}
	tool := NewMultiReadTool(&mockReadPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}, tracker, dir)
	params := MultiReadParams{Files: []MultiReadItem{{FilePath: "paged.txt"}}, MaxBytes: 75}
	for {
		_, metadata := runMultiRead(t, tool, multiReadContext(), params)
		if !metadata.Truncated {
			break
		}
		params.Cursor = metadata.Cursor
	}
	require.Equal(t, []trackedRange{{path, 1, 3}, {path, 4, 4}}, tracker.ranges)
	require.Empty(t, tracker.full)
}

func TestMultiReadCursorValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("1111111111\n2222222222\n3333333333\n4444444444"), 0o644))
	tool := newMultiReadToolForTest(dir)
	params := MultiReadParams{Files: []MultiReadItem{{FilePath: "file.txt"}}, MaxBytes: 65}
	_, first := runMultiRead(t, tool, multiReadContext(), params)
	require.True(t, first.Truncated)

	t.Run("tampered", func(t *testing.T) {
		tamperedBytes := []byte(first.Cursor)
		if tamperedBytes[len(tamperedBytes)/2] == 'A' {
			tamperedBytes[len(tamperedBytes)/2] = 'B'
		} else {
			tamperedBytes[len(tamperedBytes)/2] = 'A'
		}
		tampered := string(tamperedBytes)
		response, _ := runMultiRead(t, tool, multiReadContext(), MultiReadParams{Files: params.Files, MaxBytes: params.MaxBytes, Cursor: tampered})
		require.True(t, response.IsError)
		require.Contains(t, response.Content, "invalid multi_read cursor")
	})
	t.Run("request mismatch", func(t *testing.T) {
		response, _ := runMultiRead(t, tool, multiReadContext(), MultiReadParams{Files: params.Files, MaxBytes: params.MaxBytes + 1, Cursor: first.Cursor})
		require.True(t, response.IsError)
		require.Contains(t, response.Content, "invalid multi_read cursor")
	})
	t.Run("stale read", func(t *testing.T) {
		require.NoError(t, os.WriteFile(path, []byte("changed\ncontent"), 0o644))
		response, metadata := runMultiRead(t, tool, multiReadContext(), MultiReadParams{Files: params.Files, MaxBytes: params.MaxBytes, Cursor: first.Cursor})
		require.False(t, response.IsError)
		require.Equal(t, "error", metadata.Files[0].Status)
		require.Contains(t, metadata.Files[0].Error, "stale cursor")
	})
}

func TestMultiReadBudgets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("content\n", 40)), 0o644))
	tool := newMultiReadToolForTest(dir)
	for _, test := range []struct {
		name       string
		maxBytes   int
		maxTokens  int
		wantBudget int
		wantError  bool
	}{
		{name: "tokens only uses one byte per token", maxTokens: 30, wantBudget: 30, wantError: true},
		{name: "combined uses token minimum", maxBytes: 90, maxTokens: 30, wantBudget: 30, wantError: true},
		{name: "combined uses byte minimum", maxBytes: 60, maxTokens: 90, wantBudget: 60},
	} {
		t.Run(test.name, func(t *testing.T) {
			response, metadata := runMultiRead(t, tool, multiReadContext(), MultiReadParams{Files: []MultiReadItem{{FilePath: "file.txt"}}, MaxBytes: test.maxBytes, MaxTokens: test.maxTokens})
			require.Equal(t, test.wantError, response.IsError)
			require.LessOrEqual(t, len(response.Content), test.wantBudget)
			if test.wantError {
				require.Contains(t, response.Content, "too small for one line")
				require.Zero(t, metadata.Bytes)
			} else {
				require.Equal(t, len(response.Content), metadata.Bytes)
			}
		})
	}
	response, _ := runMultiRead(t, tool, multiReadContext(), MultiReadParams{Files: []MultiReadItem{{FilePath: path}}, MaxBytes: 1})
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "too small for one line")
}

func TestMultiReadRejectsUnsupportedResources(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "image.png")
	require.NoError(t, os.WriteFile(image, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, 0o644))
	skillDir := filepath.Join(dir, "skills")
	require.NoError(t, os.Mkdir(skillDir, 0o755))
	skillPath := filepath.Join(skillDir, skills.SkillFileName)
	require.NoError(t, os.WriteFile(skillPath, []byte("skill"), 0o644))
	tool := NewMultiReadTool(&mockReadPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}, mockFileTracker{}, dir, skillDir)
	ctx := context.WithValue(multiReadContext(), SupportsImagesContextKey, true)
	response, metadata := runMultiRead(t, tool, ctx, MultiReadParams{Files: []MultiReadItem{{FilePath: image}, {FilePath: skills.BuiltinPrefix + "sennit-config/" + skills.SkillFileName}, {FilePath: skillPath}}})
	require.False(t, response.IsError)
	require.Equal(t, []string{"unsupported", "error", "error"}, []string{metadata.Files[0].Status, metadata.Files[1].Status, metadata.Files[2].Status})
	require.Contains(t, metadata.Files[0].Error, "text files only")
	require.Contains(t, metadata.Files[1].Error, "skill resources")
	require.Contains(t, metadata.Files[2].Error, "skill files")
}

var (
	_ FileTracking       = (*recordingReadTracker)(nil)
	_ permission.Service = (*denyingMultiReadPermissions)(nil)
)
