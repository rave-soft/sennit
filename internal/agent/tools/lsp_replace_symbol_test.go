package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/stretchr/testify/require"
)

// TestSymbolRangeEndLine is the regression test for eating one line too
// many (or, for add_after, inserting one line too late) when an LSP range
// ends at column 0: per protocol.Range's own doc comment, a range spanning
// through the end of line 11 (0-indexed) is reported as
// End:{Line:12, Character:0} - the start of the FOLLOWING line, not a
// position on line 12 itself.
func TestSymbolRangeEndLine(t *testing.T) {
	t.Parallel()

	t.Run("end at column 0 on a later line backs off to the true last line", func(t *testing.T) {
		t.Parallel()
		rng := protocol.Range{
			Start: protocol.Position{Line: 5, Character: 0},
			End:   protocol.Position{Line: 12, Character: 0},
		}
		require.Equal(t, 11, symbolRangeEndLine(rng))
	})

	t.Run("end mid-line is unaffected", func(t *testing.T) {
		t.Parallel()
		rng := protocol.Range{
			Start: protocol.Position{Line: 5, Character: 0},
			End:   protocol.Position{Line: 12, Character: 4},
		}
		require.Equal(t, 12, symbolRangeEndLine(rng))
	})

	t.Run("a single-line range ending at column 0 is not adjusted", func(t *testing.T) {
		t.Parallel()
		// Start == End.Line guards against a genuinely empty/zero-width
		// range being pushed to a negative line.
		rng := protocol.Range{
			Start: protocol.Position{Line: 5, Character: 0},
			End:   protocol.Position{Line: 5, Character: 0},
		}
		require.Equal(t, 5, symbolRangeEndLine(rng))
	})
}

// TestReplaceSymbolThroughManagerRecordsOnlyTheEditedSpan is the regression
// test for defect 1: replace_symbol used to set wholeFileRead: true, so
// replacing one function retroactively marked the entire file as read for
// the session — after which edit/write would accept a blind change
// anywhere else in it. The fix records only the span replace_symbol
// actually touched, same as edit/multiedit/write already do for a partial
// change.
//
// The tracker is seeded with a lastRead well after the fixture was
// written: this test predates the G2 freshness check added later
// (checkFileFreshness, guarding against exactly this "never read"
// mockEditFileTracker zero value), so without seeding it here the tool
// would now refuse before ever reaching the coverage behavior this test
// exists to pin — see TestReplaceSymbolRefusesWithoutPriorRead for that
// refusal on its own.
func TestReplaceSymbolThroughManagerRecordsOnlyTheEditedSpan(t *testing.T) {
	root := newLSPToolWorktree(t)
	manager := newLSPToolE2EManager(t, root, "replace-symbol")
	tracker := &mockEditFileTracker{lastRead: time.Now().Add(time.Hour)}
	tool := NewReplaceSymbolTool(manager, &mockPermissionService{}, &mockHistoryService{}, tracker, root)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "replace-session")

	resp := runToolWith(t, tool, ctx, ReplaceSymbolToolName, ReplaceSymbolParams{
		Symbol:      "Exact",
		FilePath:    "a.go",
		Replacement: `func Exact() string { return "changed" }`,
	})
	require.False(t, resp.IsError, resp.Content)

	content, err := os.ReadFile(filepath.Join(root, "a.go"))
	require.NoError(t, err)
	require.Contains(t, string(content), "changed")

	// The tool never asked to record a whole-file read...
	require.Empty(t, tracker.reads)
	coverage := tracker.ReadCoverage(ctx, "replace-session", filepath.Join(root, "a.go"))
	require.False(t, coverage.Full, "replacing one symbol must not grant full-file coverage")
	// ...and coverage does not reach the untouched "Other" function three
	// lines below the one that was actually replaced.
	require.False(t, coverage.Covers(4, 4), "coverage must not reach lines the edit never touched")
	require.True(t, coverage.Covers(3, 3), "coverage must include the line that was actually replaced")
}

// TestReplaceSymbolRefusesWithoutPriorRead is the regression test for half
// of G2: replace_symbol used to cut the file at LSP-reported ranges with no
// freshness check at all, so it would happily rewrite a symbol in a file
// the session had never read (or one changed on disk since). The tracker's
// zero value ("never read") is left as-is here, unlike the test above.
func TestReplaceSymbolRefusesWithoutPriorRead(t *testing.T) {
	root := newLSPToolWorktree(t)
	manager := newLSPToolE2EManager(t, root, "replace-symbol")
	tracker := &mockEditFileTracker{}
	tool := NewReplaceSymbolTool(manager, &mockPermissionService{}, &mockHistoryService{}, tracker, root)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "replace-session")

	resp := runToolWith(t, tool, ctx, ReplaceSymbolToolName, ReplaceSymbolParams{
		Symbol:      "Exact",
		FilePath:    "a.go",
		Replacement: `func Exact() string { return "changed" }`,
	})
	require.True(t, resp.IsError, "replacing a symbol in an unread file must be refused")
	require.Contains(t, resp.Content, "has not been read in this session")

	content, err := os.ReadFile(filepath.Join(root, "a.go"))
	require.NoError(t, err)
	require.NotContains(t, string(content), "changed", "a refused replacement must not land on disk")
}

// replaceSymbolSyncHelperProcessEnv gates replaceSymbolSyncLSPHelper below:
// a dedicated fake LSP server for this file only, distinct from
// runLSPToolHelper in lsp_manager_e2e_test.go, whose "replace-symbol"
// scenario answers documentSymbol with a fixed range regardless of the
// document's actual (possibly just-changed) content — sufficient for the
// range-math tests above, but not for proving the overlay gets resynced
// before that range is trusted.
const replaceSymbolSyncHelperProcessEnv = "SENNIT_REPLACE_SYMBOL_SYNC_HELPER"

// TestReplaceSymbolSyncLSPHelperProcess is a fake LSP server whose
// documentSymbol response is computed from whatever content the client
// most recently sent it (via didOpen or didChange), rather than a static
// fixture — so a stale overlay and a resynced one report different symbol
// ranges for the same on-disk file, the way a real gopls would after the
// file changed underneath it.
func TestReplaceSymbolSyncLSPHelperProcess(t *testing.T) {
	if os.Getenv(replaceSymbolSyncHelperProcessEnv) != "1" {
		return
	}
	runReplaceSymbolSyncLSPHelper()
	os.Exit(0)
}

func runReplaceSymbolSyncLSPHelper() {
	r := bufio.NewReader(os.Stdin)
	var docText string
	for {
		body, err := readLSPToolFrame(r)
		if err != nil {
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &request) != nil {
			continue
		}
		if len(request.ID) == 0 {
			// Notifications: track the document text didOpen/didChange
			// deliver, so documentSymbol below can answer from it.
			switch request.Method {
			case "textDocument/didOpen":
				var params struct {
					TextDocument struct {
						Text string `json:"text"`
					} `json:"textDocument"`
				}
				if json.Unmarshal(request.Params, &params) == nil {
					docText = params.TextDocument.Text
				}
			case "textDocument/didChange":
				var params struct {
					ContentChanges []struct {
						Text string `json:"text"`
					} `json:"contentChanges"`
				}
				if json.Unmarshal(request.Params, &params) == nil && len(params.ContentChanges) > 0 {
					docText = params.ContentChanges[0].Text
				}
			}
			continue
		}

		var result string
		switch request.Method {
		case "initialize":
			result = `{"capabilities":{"documentSymbolProvider":true}}`
		case "textDocument/documentSymbol":
			line := -1
			for i, l := range strings.Split(docText, "\n") {
				if strings.HasPrefix(l, "func") {
					line = i
					break
				}
			}
			if line < 0 {
				result = "[]"
			} else {
				result = fmt.Sprintf(`[{"name":"Exact","kind":12,"range":{"start":{"line":%d,"character":0},"end":{"line":%d,"character":0}},"selectionRange":{"start":{"line":%d,"character":5},"end":{"line":%d,"character":10}}}]`, line, line+1, line, line)
			}
		default:
			result = "null"
		}
		response := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, result))
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(response), response)
	}
}

func newReplaceSymbolSyncE2EManager(t *testing.T, root string) *lsp.Manager {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	autoLSP := false
	store := configtest.NewStore(t, &config.Config{
		Options: &config.Options{AutoLSP: &autoLSP},
		LSP: config.LSPs{"gopls": {
			Command:     exe,
			Args:        []string{"-test.run=^TestReplaceSymbolSyncLSPHelperProcess$"},
			Env:         map[string]string{replaceSymbolSyncHelperProcessEnv: "1"},
			FileTypes:   []string{"go"},
			RootMarkers: []string{"go.mod"},
			Timeout:     5,
		}},
	}, configtest.WithWorkingDir(root))
	manager := lsp.NewManager(store)
	t.Cleanup(func() { manager.StopAll(t.Context()) })
	return manager
}

// TestReplaceSymbolResyncsOverlayBeforeReadingRanges is the main regression
// test for G2: DocumentSymbols used to be read straight from the LSP
// overlay while applyFileMutation cuts the file it reads fresh from disk,
// so once those two disagreed about line numbers, replace_symbol cut the
// wrong span.
//
// The fixture starts with "Exact" on line 2 (0-indexed) and is opened by
// the LSP in that state. The disk file is then rewritten with a comment
// line inserted above the package clause, shifting "Exact" down to line 3
// — without ever notifying the LSP through the normal didOpen/didChange
// path a real editor would use. A stale overlay would still answer
// documentSymbol with line 2 (now a blank line in the shifted file); only
// resyncing (OpenFileOnDemand + NotifyChange) before DocumentSymbols makes
// the fake server recompute from the new content and report line 3, the
// line the replacement must actually land on.
func TestReplaceSymbolResyncsOverlayBeforeReadingRanges(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/e2e\n\ngo 1.24\n"), 0o644))
	original := "package e2e\n\nfunc Exact() string { return \"old\" }\nfunc Other() { return }\n"
	file := filepath.Join(root, "a.go")
	require.NoError(t, os.WriteFile(file, []byte(original), 0o644))

	manager := newReplaceSymbolSyncE2EManager(t, root)

	// Warm the LSP so it opens the file and caches the *original* content
	// — this is the stale overlay the fix has to resync away from.
	manager.Start(t.Context(), file)
	client := findLSPClient(manager, file)
	require.NotNil(t, client)
	require.NoError(t, client.OpenFileOnDemand(t.Context(), file))

	// Now the disk file changes underneath the LSP, without going through
	// NotifyChange: a comment line lands above "package e2e", shifting
	// "func Exact" from line 2 down to line 3.
	shifted := "// Package e2e is a fixture.\n" + original
	require.NoError(t, os.WriteFile(file, []byte(shifted), 0o644))

	tracker := &mockEditFileTracker{lastRead: time.Now().Add(time.Hour)}
	tool := NewReplaceSymbolTool(manager, &mockPermissionService{}, &mockHistoryService{}, tracker, root)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "replace-session")

	resp := runToolWith(t, tool, ctx, ReplaceSymbolToolName, ReplaceSymbolParams{
		Symbol:      "Exact",
		FilePath:    "a.go",
		Replacement: `func Exact() string { return "new" }`,
	})
	require.False(t, resp.IsError, resp.Content)

	content, err := os.ReadFile(file)
	require.NoError(t, err)
	got := string(content)

	require.Contains(t, got, "// Package e2e is a fixture.", "the unrelated leading comment must survive untouched")
	require.Contains(t, got, `func Exact() string { return "new" }`, "the replacement must land")
	require.NotContains(t, got, `return "old"`, "the old symbol body must be gone, not duplicated")
	require.Contains(t, got, "func Other() { return }", "the untouched sibling function must survive intact")
}
