package tools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/stretchr/testify/require"
)

// definitionSyncHelperProcessEnv gates definitionSyncLSPHelper below: a
// fake LSP server whose textDocument/definition response depends on
// whatever content the client most recently sent via didOpen/didChange,
// unlike runLSPToolHelper's "definition" scenario (lsp_manager_e2e_test.go),
// which answers with a fixed location regardless of the document's actual
// content and so cannot distinguish a stale overlay from a fresh one.
const definitionSyncHelperProcessEnv = "SENNIT_DEFINITION_SYNC_HELPER"

// TestDefinitionSyncLSPHelperProcess is the fake server: it reports a
// definition only when the requested line, in whatever content it was
// last told about, actually names the target identifier. A stale overlay
// (still holding the pre-shift content) and a resynced one therefore
// answer differently for the same on-disk file and the same requested
// line — precisely what proves resyncing happened.
func TestDefinitionSyncLSPHelperProcess(t *testing.T) {
	if os.Getenv(definitionSyncHelperProcessEnv) != "1" {
		return
	}
	runDefinitionSyncLSPHelper()
	os.Exit(0)
}

func runDefinitionSyncLSPHelper() {
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
			result = `{"capabilities":{"definitionProvider":true}}`
		case "textDocument/definition":
			var params struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
				Position struct {
					Line uint32 `json:"line"`
				} `json:"position"`
			}
			result = "null"
			if json.Unmarshal(request.Params, &params) == nil {
				lines := strings.Split(docText, "\n")
				if int(params.Position.Line) < len(lines) && strings.Contains(lines[params.Position.Line], "Exact") {
					result = fmt.Sprintf(`[{"uri":%q,"range":{"start":{"line":%d,"character":0},"end":{"line":%d,"character":5}}}]`,
						params.TextDocument.URI, params.Position.Line, params.Position.Line)
				}
			}
		default:
			result = "null"
		}
		response := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, result))
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(response), response)
	}
}

func newDefinitionSyncE2EManager(t *testing.T, root string) *lsp.Manager {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	autoLSP := false
	store := configtest.NewStore(t, &config.Config{
		Options: &config.Options{AutoLSP: &autoLSP},
		LSP: config.LSPs{"gopls": {
			Command:     exe,
			Args:        []string{"-test.run=^TestDefinitionSyncLSPHelperProcess$"},
			Env:         map[string]string{definitionSyncHelperProcessEnv: "1"},
			FileTypes:   []string{"go"},
			RootMarkers: []string{"go.mod"},
			Timeout:     5,
		}},
	}, configtest.WithWorkingDir(root))
	manager := lsp.NewManager(store)
	t.Cleanup(func() { manager.StopAll(t.Context()) })
	return manager
}

// TestLSPDefinitionResyncsOverlayBeforeCheckingViability is the regression
// test for finding 1.5: resolveSymbol's viability check (firstWithDefinition,
// via firstSymbolWithDefinition) used to call Definition straight off the
// LSP's overlay, which only OpenFileOnDemand (a no-op once already open)
// ever refreshed. A file whose content changed on disk without going
// through edit/write/multiedit — read and bash never send didChange — left
// that overlay stale, so the position grep found on the fresh file missed
// the identifier against the server's stale one, isNoIdentifierError-style
// misses came back as "not found" candidates, and a real symbol was
// reported as absent.
//
// The fixture starts with "Exact" on line 2 (0-indexed) and is opened by
// the LSP in that state. The disk file is then rewritten with a leading
// comment, shifting "func Exact" down to line 3 — without ever notifying
// the LSP. resolveSymbol greps the fresh disk file and finds "Exact" on
// line 3; only resyncing the overlay before asking Definition makes the
// fake server's tracked docText match, so it answers with a location
// instead of null.
func TestLSPDefinitionResyncsOverlayBeforeCheckingViability(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/e2e\n\ngo 1.24\n"), 0o644))
	original := "package e2e\n\nfunc Exact() string { return \"old\" }\n"
	file := filepath.Join(root, "a.go")
	require.NoError(t, os.WriteFile(file, []byte(original), 0o644))

	manager := newDefinitionSyncE2EManager(t, root)

	// Warm the LSP so it opens the file and caches the *original*
	// content — the stale overlay the fix has to resync away from.
	manager.Start(t.Context(), file)
	client := findLSPClient(manager, file)
	require.NotNil(t, client)
	require.NoError(t, client.OpenFileOnDemand(t.Context(), file))

	// The disk file changes underneath the LSP, without going through
	// NotifyChange: a comment line lands above "package e2e", shifting
	// "func Exact" from line 2 down to line 3.
	shifted := "// Package e2e is a fixture.\n" + original
	require.NoError(t, os.WriteFile(file, []byte(shifted), 0o644))

	tool := NewDefinitionTool(manager, root)
	resp := runToolWith(t, tool, t.Context(), DefinitionToolName, DefinitionParams{Symbol: "Exact", Path: "."})

	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "Found 1 definition(s)",
		"a resynced overlay must let the fake server confirm the identifier at its post-shift line")
	require.NotContains(t, resp.Content, "No definition found",
		"a stale overlay must not make a genuine symbol read back as not found")
}

// TestLSPDefinitionNotFoundMentionsTruncation is the regression test for
// finding 3: lsp_definition used to discard resolveSymbol's truncated
// flag, so a capped grep whose matched candidates all lacked an LSP
// client read back identically to a symbol that genuinely does not exist
// anywhere in the tree.
func TestLSPDefinitionNotFoundMentionsTruncation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeManySymbolMatches(t, root)

	manager := newNoLSPManager(t)
	tool := NewDefinitionTool(manager, root)
	resp := runToolWith(t, tool, t.Context(), DefinitionToolName, DefinitionParams{Symbol: "count", Path: "."})

	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "No definition found for symbol 'count'")
	require.Contains(t, resp.Content, "match limit",
		"a capped grep must say so instead of reading back exactly like a genuine miss")
}
