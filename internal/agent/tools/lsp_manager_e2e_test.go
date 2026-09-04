package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/stretchr/testify/require"
)

const lspToolHelperProcess = "GO_WANT_HELPER_PROCESS"

func TestLSPToolHelperProcess(t *testing.T) {
	if os.Getenv(lspToolHelperProcess) != "1" {
		return
	}
	runLSPToolHelper()
	os.Exit(0)
}

func runLSPToolHelper() {
	r := bufio.NewReader(os.Stdin)
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
		scenario := os.Getenv("SENNIT_LSP_TOOL_SCENARIO")
		if len(request.ID) == 0 {
			// Notification (no id expecting a reply). The only one this
			// helper reacts to is didOpen, and only for the "diagnostics"
			// scenario: reply with a publishDiagnostics notification for
			// the just-opened file, mimicking a real server that found a
			// problem in it.
			if scenario == "diagnostics" && request.Method == "textDocument/didOpen" {
				var params struct {
					TextDocument struct {
						URI string `json:"uri"`
					} `json:"textDocument"`
				}
				// Only the user's own file gets a diagnostic — root-marker
				// files like go.mod are opened too (Start opens root
				// markers), and a diagnostic on those would land in the
				// project bucket and confuse what this test is checking.
				if json.Unmarshal(request.Params, &params) == nil && strings.HasSuffix(params.TextDocument.URI, "a.go") {
					notification := []byte(fmt.Sprintf(
						`{"jsonrpc":"2.0","method":"textDocument/publishDiagnostics","params":{"uri":%q,"diagnostics":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"severity":1,"message":"boom"}]}}`,
						params.TextDocument.URI))
					fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(notification), notification)
				}
			}
			continue
		}
		result := "null"
		switch request.Method {
		case "initialize":
			switch scenario {
			case "no-capabilities":
				result = `{"capabilities":{}}`
			case "rename", "rename-edit", "rename-outside":
				result = `{"capabilities":{"definitionProvider":true,"renameProvider":true}}`
			case "replace-symbol":
				result = `{"capabilities":{"definitionProvider":true,"documentSymbolProvider":true}}`
			default:
				result = `{"capabilities":{"hoverProvider":true,"workspaceSymbolProvider":true}}`
			}
		case "textDocument/definition":
			root := filepath.ToSlash(os.Getenv("SENNIT_LSP_TOOL_ROOT"))
			uri := "file://" + filepath.ToSlash(filepath.Join(root, "a.go"))
			result = fmt.Sprintf(`[{"uri":%q,"range":{"start":{"line":1,"character":0},"end":{"line":1,"character":5}}}]`, uri)
		case "textDocument/rename":
			root := filepath.ToSlash(os.Getenv("SENNIT_LSP_TOOL_ROOT"))
			switch scenario {
			case "rename-edit":
				// Two files, one changed line apiece: a real rename, not the
				// empty-edit stand-in the default case uses. "Exact" sits at
				// the same column in both fixtures (see
				// newLSPToolRenameEditWorktree).
				aURI := "file://" + filepath.ToSlash(filepath.Join(root, "a.go"))
				bURI := "file://" + filepath.ToSlash(filepath.Join(root, "b.go"))
				result = fmt.Sprintf(`{"changes":{%q:[{"range":{"start":{"line":2,"character":5},"end":{"line":2,"character":10}},"newText":"Renamed"}],%q:[{"range":{"start":{"line":2,"character":25},"end":{"line":2,"character":30}},"newText":"Renamed"}]}}`, aURI, bURI)
			case "rename-outside":
				// Names a file outside the confined root, so the confinement
				// check has something to refuse.
				outsideURI := "file://" + filepath.ToSlash(filepath.Join(filepath.Dir(root), "outside.go"))
				result = fmt.Sprintf(`{"changes":{%q:[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"newText":"x"}]}}`, outsideURI)
			default:
				// An empty edit is still a non-nil WorkspaceEdit, which drives
				// RenameTool through its permission gate without changing fixtures.
				result = `{"changes":{}}`
			}
		case "textDocument/documentSymbol":
			if scenario == "replace-symbol" {
				// A single-line "Exact" function symbol at line 2 (0-based),
				// matching newLSPToolReplaceSymbolWorktree's fixture.
				result = `[{"name":"Exact","kind":12,"range":{"start":{"line":2,"character":0},"end":{"line":2,"character":36}},"selectionRange":{"start":{"line":2,"character":5},"end":{"line":2,"character":10}}}]`
			}
		case "workspace/symbol":
			root := filepath.ToSlash(os.Getenv("SENNIT_LSP_TOOL_ROOT"))
			uri := "file://" + filepath.ToSlash(filepath.Join(root, "a.go"))
			if scenario == "ambiguous" {
				result = fmt.Sprintf(`[{"name":"Exact","kind":12,"location":{"uri":%q,"range":{"start":{"line":1,"character":0},"end":{"line":1,"character":5}}}},{"name":"Exact","kind":12,"location":{"uri":%q,"range":{"start":{"line":2,"character":0},"end":{"line":2,"character":5}}}}]`, uri, uri)
			} else if scenario != "no-capabilities" {
				result = fmt.Sprintf(`[{"name":"Exact","kind":12,"location":{"uri":%q,"range":{"start":{"line":1,"character":0},"end":{"line":1,"character":5}}}},{"name":"Other","kind":12,"location":{"uri":%q,"range":{"start":{"line":2,"character":0},"end":{"line":2,"character":5}}}}]`, uri, uri)
			}
		case "textDocument/hover":
			result = `{"contents":{"kind":"markdown","value":"Exact() string"}}`
			// TestLSPHoverThroughManagerConvertsPosition checks this log to
			// confirm the position that reaches the server is 0-based, not
			// the tool's 1-based file_path/line/character input.
			if logPath := os.Getenv("SENNIT_LSP_TOOL_LOG"); logPath != "" {
				var params struct {
					Position struct {
						Line      uint32 `json:"line"`
						Character uint32 `json:"character"`
					} `json:"position"`
				}
				if json.Unmarshal(request.Params, &params) == nil {
					if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
						fmt.Fprintf(f, "line=%d character=%d\n", params.Position.Line, params.Position.Character)
						f.Close()
					}
				}
			}
		}
		response := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, result))
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(response), response)
	}
}

func readLSPToolFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if value, ok := strings.CutPrefix(line, "Content-Length: "); ok {
			length, err = strconv.Atoi(value)
			if err != nil {
				return nil, err
			}
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("missing Content-Length")
	}
	body := make([]byte, length)
	_, err := io.ReadFull(r, body)
	return body, err
}

func newLSPToolE2EManager(t *testing.T, root, scenario string) *lsp.Manager {
	t.Helper()
	return newLSPToolE2EManagerWithEnv(t, root, scenario, nil)
}

func newLSPToolE2EManagerWithEnv(t *testing.T, root, scenario string, extraEnv map[string]string) *lsp.Manager {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	autoLSP := false
	env := map[string]string{lspToolHelperProcess: "1", "SENNIT_LSP_TOOL_SCENARIO": scenario, "SENNIT_LSP_TOOL_ROOT": root}
	for k, v := range extraEnv {
		env[k] = v
	}
	store := configtest.NewStore(t, &config.Config{
		Options: &config.Options{AutoLSP: &autoLSP},
		LSP: config.LSPs{"gopls": {
			Command:     exe,
			Args:        []string{"-test.run=^TestLSPToolHelperProcess$"},
			Env:         env,
			FileTypes:   []string{"go"},
			RootMarkers: []string{"go.mod"},
			Timeout:     5,
		}},
	}, configtest.WithWorkingDir(root))
	manager := lsp.NewManager(store)
	t.Cleanup(func() { manager.StopAll(t.Context()) })
	return manager
}

func newLSPToolWorktree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/e2e\n\ngo 1.24\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package e2e\n\nfunc Exact() string { return \"ok\" }\nfunc Other() {}\n"), 0o644))
	return root
}

func TestLSPToolsThroughManagerAndProcess(t *testing.T) {
	root := newLSPToolWorktree(t)
	manager := newLSPToolE2EManager(t, root, "symbols")
	symbols := NewWorkspaceSymbolsTool(manager, root)

	first := runToolWith(t, symbols, t.Context(), WorkspaceSymbolsToolName, WorkspaceSymbolsParams{Query: "Exact", Limit: 1})
	require.False(t, first.IsError)
	require.Contains(t, first.Content, "Exact")
	require.Contains(t, first.Content, "a.go:2")
	require.NotContains(t, first.Content, filepath.ToSlash(root))
	firstMeta := responseMetadata[WorkspaceSymbolsMetadata](t, first.Metadata)
	require.True(t, firstMeta.Truncated)
	require.NotEmpty(t, firstMeta.Cursor)

	second := runToolWith(t, symbols, t.Context(), WorkspaceSymbolsToolName, WorkspaceSymbolsParams{Query: "Exact", Limit: 1, Cursor: firstMeta.Cursor})
	require.False(t, second.IsError)
	require.Contains(t, second.Content, "Other")
	require.NotContains(t, second.Content, "Exact")
	require.Contains(t, second.Content, "a.go:3")

	hover := runToolWith(t, NewHoverTool(manager, root), t.Context(), HoverToolName, HoverParams{Symbol: "Exact"})
	require.False(t, hover.IsError)
	require.Equal(t, "Exact() string", hover.Content)
}

func TestLSPRenameThroughManagerRequestsOperationScopedPermission(t *testing.T) {
	root := newLSPToolWorktree(t)
	manager := newLSPToolE2EManager(t, root, "rename")
	perms := permission.NewPermissionService(root, false, nil)
	events := perms.Subscribe(t.Context())
	tool := NewRenameTool(manager, perms, nil, nil, root)
	params := RenameParams{Symbol: "Exact", NewName: "Renamed", Path: "."}
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "rename-session")

	done := make(chan fantasy.ToolResponse, 1)
	go func() { done <- runToolWith(t, tool, ctx, RenameToolName, params) }()
	var req permission.PermissionRequest
	select {
	case request := <-events:
		req = request.Payload
	case response := <-done:
		t.Fatalf("rename did not request permission: %s", response.Content)
	case <-time.After(5 * time.Second):
		t.Fatal("rename did not reach the permission request")
	}
	require.Equal(t, "rename", req.Action)
	require.Equal(t, root, req.Path)
	require.Equal(t, params, req.Params)
	require.True(t, perms.GrantPersistent(req))
	response := <-done
	require.False(t, response.IsError, response.Content)

	// The persisted grant is keyed by the actual operation parameters, not
	// merely by tool/action/path. A different rename must reach the dialog.
	other := RenameParams{Symbol: "Exact", NewName: "OtherName", Path: "."}
	otherDone := make(chan fantasy.ToolResponse, 1)
	go func() { otherDone <- runToolWith(t, tool, ctx, RenameToolName, other) }()
	otherRequest := <-events
	require.Equal(t, other, otherRequest.Payload.Params)
	require.True(t, perms.Deny(otherRequest.Payload))
	require.True(t, (<-otherDone).IsError)
}

func TestLSPHoverThroughManagerRejectsAmbiguousSymbol(t *testing.T) {
	root := newLSPToolWorktree(t)
	manager := newLSPToolE2EManager(t, root, "ambiguous")
	response := runToolWith(t, NewHoverTool(manager, root), t.Context(), HoverToolName, HoverParams{Symbol: "Exact"})
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "ambiguous")
}

func TestLSPWorkspaceSymbolsThroughManagerRequiresCapability(t *testing.T) {
	root := newLSPToolWorktree(t)
	manager := newLSPToolE2EManager(t, root, "no-capabilities")
	response := runToolWith(t, NewWorkspaceSymbolsTool(manager, root), t.Context(), WorkspaceSymbolsToolName, WorkspaceSymbolsParams{Query: "Exact"})
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "no workspace-symbol-capable LSP client")
}

// TestDiagnosticsToolAttributesRelativeFilePathToCurrentFile reproduces
// defect 1: a model naturally passes the relative path it saw from
// read/grep. Before the fix, lsp_diagnostics compared that raw relative
// path against the absolute paths reported by the LSP client, so the
// file's own diagnostic never matched path == filePath and landed in the
// project bucket instead of "Current file".
func TestDiagnosticsToolAttributesRelativeFilePathToCurrentFile(t *testing.T) {
	root := newLSPToolWorktree(t)
	manager := newLSPToolE2EManager(t, root, "diagnostics")

	response := runToolWith(t, NewDiagnosticsTool(manager, root), t.Context(), DiagnosticsToolName, DiagnosticsParams{FilePath: "a.go"})
	require.False(t, response.IsError)
	require.Contains(t, response.Content, "<file_diagnostics>")
	require.Contains(t, response.Content, "boom")
	require.NotContains(t, response.Content, "<project_diagnostics>")
	require.Contains(t, response.Content, "Current file: 1 errors, 0 warnings")
	require.Contains(t, response.Content, "Project: 0 errors, 0 warnings")
}

// TestDiagnosticsToolDistinguishesCleanFromNoLSPRunning is the regression
// test for finding B: getDiagnostics returned "" both when a real client
// ran and found nothing, and when no client was running at all (LSP
// unconfigured, or auto-start off) — a model reading an empty response
// could not tell "checked, clean" from "never checked".
func TestDiagnosticsToolDistinguishesCleanFromNoLSPRunning(t *testing.T) {
	root := newLSPToolWorktree(t)

	t.Run("no LSP client running at all", func(t *testing.T) {
		autoLSP := false
		store := configtest.NewStore(t, &config.Config{
			Options: &config.Options{AutoLSP: &autoLSP},
		}, configtest.WithWorkingDir(root))
		manager := lsp.NewManager(store)
		t.Cleanup(func() { manager.StopAll(t.Context()) })
		require.Zero(t, manager.Clients().Len(), "no LSP is configured and auto-start is off")

		response := runToolWith(t, NewDiagnosticsTool(manager, root), t.Context(), DiagnosticsToolName, DiagnosticsParams{FilePath: "a.go"})
		require.False(t, response.IsError)
		require.Equal(t, noLSPRunningMessage, response.Content)
	})

	t.Run("client running and genuinely clean", func(t *testing.T) {
		manager := newLSPToolE2EManager(t, root, "symbols")

		response := runToolWith(t, NewDiagnosticsTool(manager, root), t.Context(), DiagnosticsToolName, DiagnosticsParams{FilePath: "a.go"})
		require.False(t, response.IsError)
		require.Equal(t, noDiagnosticsFoundMessage, response.Content,
			"a client that ran and found nothing must not read the same as no client running")
	})
}

// TestResolveSymbolResultsStartsOnMatchedFileNotWorkingDir reproduces
// defect 2: resolveSymbolResults used to call lspManager.Start on the
// workspace directory, which handlesFiletype always rejects (a directory
// has no file extension), so no server was ever started on a session's
// first LSP call. This manager is fresh — no read/edit has opened
// anything yet — so a client only appears in Clients() if
// resolveSymbolResults itself starts one, on the matched file.
func TestResolveSymbolResultsStartsOnMatchedFileNotWorkingDir(t *testing.T) {
	root := newLSPToolWorktree(t)
	manager := newLSPToolE2EManager(t, root, "symbols")
	require.Zero(t, manager.Clients().Len(), "fixture must start with no running LSP client")

	results, _, err := resolveSymbolResults(t.Context(), manager, "Exact", root)
	require.NoError(t, err)
	require.NotEmpty(t, results)
	require.Positive(t, manager.Clients().Len(), "resolveSymbolResults must start a client for the matched file")
}
