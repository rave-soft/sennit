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

// referencesSyncHelperProcessEnv gates referencesSyncLSPHelper below: a fake
// LSP server whose textDocument/references answer depends on whatever
// content the client most recently sent via didOpen/didChange, the same
// technique lsp_definition_test.go uses, needed because a fixed-location
// canned response cannot distinguish a stale overlay from a resynced one.
const referencesSyncHelperProcessEnv = "SENNIT_REFERENCES_SYNC_HELPER"

// TestReferencesSyncLSPHelperProcess reports a reference only when the
// requested line, in whatever content it was last told about, actually
// names the target identifier.
func TestReferencesSyncLSPHelperProcess(t *testing.T) {
	if os.Getenv(referencesSyncHelperProcessEnv) != "1" {
		return
	}
	runReferencesSyncLSPHelper()
	os.Exit(0)
}

func runReferencesSyncLSPHelper() {
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
			result = `{"capabilities":{"referencesProvider":true}}`
		case "textDocument/references":
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

func newReferencesSyncE2EManager(t *testing.T, root string) *lsp.Manager {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	autoLSP := false
	store := configtest.NewStore(t, &config.Config{
		Options: &config.Options{AutoLSP: &autoLSP},
		LSP: config.LSPs{"gopls": {
			Command:     exe,
			Args:        []string{"-test.run=^TestReferencesSyncLSPHelperProcess$"},
			Env:         map[string]string{referencesSyncHelperProcessEnv: "1"},
			FileTypes:   []string{"go"},
			RootMarkers: []string{"go.mod"},
			Timeout:     5,
		}},
	}, configtest.WithWorkingDir(root))
	manager := lsp.NewManager(store)
	t.Cleanup(func() { manager.StopAll(t.Context()) })
	return manager
}

// TestLSPReferencesResyncsOverlayBeforeQuerying is the regression test for
// finding 1.5's references surface: unlike resolveSymbol (used by
// definition/rename/call-hierarchy), lsp_references calls FindReferences
// directly on every resolveSymbolResults candidate rather than going
// through firstSymbolWithDefinition, so it needed its own syncOverlay call
// in references.go's loop. Same fixture and staleness setup as
// TestLSPDefinitionResyncsOverlayBeforeCheckingViability: "Exact" shifts
// from line 2 to line 3 on disk without notifying the already-open client.
func TestLSPReferencesResyncsOverlayBeforeQuerying(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/e2e\n\ngo 1.24\n"), 0o644))
	original := "package e2e\n\nfunc Exact() string { return \"old\" }\n"
	file := filepath.Join(root, "a.go")
	require.NoError(t, os.WriteFile(file, []byte(original), 0o644))

	manager := newReferencesSyncE2EManager(t, root)

	manager.Start(t.Context(), file)
	client := findLSPClient(manager, file)
	require.NotNil(t, client)
	require.NoError(t, client.OpenFileOnDemand(t.Context(), file))

	shifted := "// Package e2e is a fixture.\n" + original
	require.NoError(t, os.WriteFile(file, []byte(shifted), 0o644))

	tool := NewReferencesTool(manager, root)
	resp := runToolWith(t, tool, t.Context(), ReferencesToolName, ReferencesParams{Symbol: "Exact", Path: "."})

	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "Found 1 reference(s)",
		"a resynced overlay must let the fake server confirm the identifier at its post-shift line")
	require.NotContains(t, resp.Content, "No references found",
		"a stale overlay must not make a genuine symbol read back as having no references")
}
