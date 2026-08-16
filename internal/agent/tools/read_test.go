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
	"github.com/rave-soft/sennit/internal/filetracker"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestReadTextFileBoundaryCases(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sample.txt")

	var allLines []string
	for i := range 5 {
		allLines = append(allLines, fmt.Sprintf("line %d", i+1))
	}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(allLines, "\n")), 0o644))

	tests := []struct {
		name        string
		offset      int
		limit       int
		wantContent string
		wantHasMore bool
	}{
		{
			name:        "exactly limit lines remaining",
			offset:      0,
			limit:       5,
			wantContent: "line 1\nline 2\nline 3\nline 4\nline 5",
			wantHasMore: false,
		},
		{
			name:        "limit plus one line remaining",
			offset:      0,
			limit:       4,
			wantContent: "line 1\nline 2\nline 3\nline 4",
			wantHasMore: true,
		},
		{
			name:        "offset at last line",
			offset:      4,
			limit:       3,
			wantContent: "line 5",
			wantHasMore: false,
		},
		{
			name:        "offset beyond eof",
			offset:      10,
			limit:       3,
			wantContent: "",
			wantHasMore: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotContent, gotHasMore, err := readTextFile(filePath, tt.offset, tt.limit, 0)
			require.NoError(t, err)
			require.Equal(t, tt.wantContent, gotContent)
			require.Equal(t, tt.wantHasMore, gotHasMore)
		})
	}
}

func TestReadTextFileTruncatesLongLines(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "longline.txt")

	longLine := strings.Repeat("a", MaxLineLength+10)
	require.NoError(t, os.WriteFile(filePath, []byte(longLine), 0o644))

	content, hasMore, err := readTextFile(filePath, 0, 1, 0)
	require.NoError(t, err)
	require.False(t, hasMore)
	require.Equal(t, strings.Repeat("a", MaxLineLength)+"...", content)
}

func TestReadTextFileLineExceeding1MB(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "huge_line.txt")

	hugeLine := strings.Repeat("A", 2*1024*1024) // 2MB — exceeds bufio.Scanner max
	require.NoError(t, os.WriteFile(filePath, []byte(hugeLine), 0o644))

	content, hasMore, err := readTextFile(filePath, 0, 1, 0)
	require.NoError(t, err)
	require.False(t, hasMore)
	require.Equal(t, strings.Repeat("A", MaxLineLength)+"...", content)
}

func TestReadToolAllowsSmallSectionsOfLargeFiles(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "large.txt")
	lines := []string{strings.Repeat("a", MaxReadSize+1), "target line", "after target"}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(lines, "\n")), 0o644))

	tool := newReadToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	resp := runReadTool(t, tool, ctx, ReadParams{
		FilePath: filePath,
		Offset:   1,
		Limit:    1,
	})

	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "     2|target line")
	require.NotContains(t, resp.Content, "File is too large")

	var meta ReadResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, "target line", meta.Content)
}

func TestReadToolTruncatesOversizedReturnedSections(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "large-section.txt")
	lines := make([]string, DefaultReadLimit)
	for i := range lines {
		lines[i] = strings.Repeat("a", MaxLineLength)
	}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(lines, "\n")), 0o644))

	tool := newReadToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	resp := runReadTool(t, tool, ctx, ReadParams{
		FilePath: filePath,
	})

	// Oversized files are truncated at the cap with continuation
	// guidance rather than rejected outright.
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Use 'offset' parameter")
}

func TestReadToolBlocksOversizedImages(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "large.png")
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Repeat("a", MaxReadSize+1)), 0o644))

	tool := newReadToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	ctx = context.WithValue(ctx, SupportsImagesContextKey, true)
	resp := runReadTool(t, tool, ctx, ReadParams{
		FilePath: filePath,
	})

	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "Image file is too large")
}

func TestReadTextFileEnforcesMaxContentSize(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "oversized.txt")
	lines := []string{
		strings.Repeat("a", MaxLineLength),
		strings.Repeat("b", MaxLineLength),
		"target line",
	}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(lines, "\n")), 0o644))

	// The size cap truncates at a line boundary and advertises
	// continuation instead of failing the read.
	content, hasMore, err := readTextFile(filePath, 0, len(lines), MaxLineLength)
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("a", MaxLineLength), content)
	require.True(t, hasMore)

	content, hasMore, err = readTextFile(filePath, 2, 1, MaxLineLength)
	require.NoError(t, err)
	require.Equal(t, "target line", content)
	require.False(t, hasMore)
}

func TestReadTextFileAllowsExactMaxContentSize(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "exact-size.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("abcd\nefgh"), 0o644))

	content, hasMore, err := readTextFile(filePath, 0, 2, len("abcd\nefgh"))
	require.NoError(t, err)
	require.Equal(t, "abcd\nefgh", content)
	require.False(t, hasMore)
}

type mockReadPermissionService struct {
	*pubsub.Broker[permission.PermissionRequest]
}

func (m *mockReadPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	return true, nil
}

func (m *mockReadPermissionService) Grant(req permission.PermissionRequest) bool { return true }

func (m *mockReadPermissionService) Deny(req permission.PermissionRequest) bool { return true }

func (m *mockReadPermissionService) GrantPersistent(req permission.PermissionRequest) bool {
	return true
}

func (m *mockReadPermissionService) AutoApproveSession(sessionID string) {}

func (m *mockReadPermissionService) SetSkipRequests(skip bool) {}

func (m *mockReadPermissionService) SkipRequests() bool {
	return false
}

func (m *mockReadPermissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[permission.PermissionNotification] {
	return make(<-chan pubsub.Event[permission.PermissionNotification])
}

type mockFileTracker struct{}

func (m mockFileTracker) RecordRead(ctx context.Context, sessionID, path string) {}

func (m mockFileTracker) LastReadTime(ctx context.Context, sessionID, path string) time.Time {
	return time.Time{}
}

func (m mockFileTracker) ListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	return nil, nil
}

func (m mockFileTracker) RecordPartialRead(ctx context.Context, sessionID, path string, start, end int) {
}

func (m mockFileTracker) RecordEdit(ctx context.Context, sessionID, path string, start, end, newEnd int) {
}

func (m mockFileTracker) ReadCoverage(ctx context.Context, sessionID, path string) filetracker.Coverage {
	return filetracker.FullCoverage
}

func newReadToolForTest(workingDir string) fantasy.AgentTool {
	permissions := &mockReadPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	return NewReadTool(nil, permissions, mockFileTracker{}, nil, workingDir)
}

func runReadTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params ReadParams) fantasy.ToolResponse {
	t.Helper()

	input, err := json.Marshal(params)
	require.NoError(t, err)

	call := fantasy.ToolCall{
		ID:    "test-call",
		Name:  ReadToolName,
		Input: string(input),
	}

	resp, err := tool.Run(ctx, call)
	require.NoError(t, err)
	return resp
}

var _ filetracker.Service = mockFileTracker{}

func TestReadBuiltinFile(t *testing.T) {
	t.Parallel()

	t.Run("reads braid-config skill", func(t *testing.T) {
		t.Parallel()

		resp := readBuiltinFile(ReadParams{
			FilePath: "sennit://skills/braid-config/SKILL.md",
		}, nil)
		require.NotEmpty(t, resp.Content)
		require.Contains(t, resp.Content, "Braid Configuration")
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()

		resp := readBuiltinFile(ReadParams{
			FilePath: "sennit://skills/nonexistent/SKILL.md",
		}, nil)
		require.True(t, resp.IsError)
	})

	t.Run("metadata has skill info", func(t *testing.T) {
		t.Parallel()

		resp := readBuiltinFile(ReadParams{
			FilePath: "sennit://skills/braid-config/SKILL.md",
		}, nil)

		var meta ReadResponseMetadata
		require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
		require.Equal(t, ReadResourceSkill, meta.ResourceType)
		require.Equal(t, "braid-config", meta.ResourceName)
		require.NotEmpty(t, meta.ResourceDescription)
	})

	t.Run("respects offset", func(t *testing.T) {
		t.Parallel()

		resp := readBuiltinFile(ReadParams{
			FilePath: "sennit://skills/braid-config/SKILL.md",
			Offset:   5,
		}, nil)
		require.NotContains(t, resp.Content, "     1|")
	})
}

func TestSniffImageMimeType(t *testing.T) {
	t.Parallel()

	jpegMagic := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F'}
	pngMagic := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	gifMagic := []byte("GIF89a")
	// Minimal RIFF/WEBP header.
	webpMagic := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 16)...)
	random := []byte("not an image at all, just text")

	cases := []struct {
		name     string
		data     []byte
		fallback string
		want     string
	}{
		{"jpeg bytes in .png file uses sniffed", jpegMagic, "image/png", "image/jpeg"},
		{"png bytes in .jpg file uses sniffed", pngMagic, "image/jpeg", "image/png"},
		{"gif bytes uses sniffed", gifMagic, "image/png", "image/gif"},
		{"webp bytes uses sniffed", webpMagic, "image/png", "image/webp"},
		{"matching extension and content keeps sniffed", pngMagic, "image/png", "image/png"},
		{"unsniffable content falls back", random, "image/png", "image/png"},
		{"empty content falls back", nil, "image/jpeg", "image/jpeg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, sniffImageMimeType(tc.data, tc.fallback))
		})
	}
}

func (m *mockReadPermissionService) ActiveRequest() (permission.PermissionRequest, bool) {
	return permission.PermissionRequest{}, false
}

func (*mockReadPermissionService) ConfineToWorkingDir() {}
func (*mockReadPermissionService) ConfinedDir() string  { return "" }
