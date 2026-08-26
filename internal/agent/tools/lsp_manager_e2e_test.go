package tools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/lsp"
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
		}
		if json.Unmarshal(body, &request) != nil || len(request.ID) == 0 {
			continue
		}
		result := "null"
		scenario := os.Getenv("SENNIT_LSP_TOOL_SCENARIO")
		switch request.Method {
		case "initialize":
			if scenario == "no-capabilities" {
				result = `{"capabilities":{}}`
			} else {
				result = `{"capabilities":{"hoverProvider":true,"workspaceSymbolProvider":true}}`
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
	exe, err := os.Executable()
	require.NoError(t, err)
	autoLSP := false
	store := configtest.NewStore(t, &config.Config{
		Options: &config.Options{AutoLSP: &autoLSP},
		LSP: config.LSPs{"gopls": {
			Command:     exe,
			Args:        []string{"-test.run=^TestLSPToolHelperProcess$"},
			Env:         map[string]string{lspToolHelperProcess: "1", "SENNIT_LSP_TOOL_SCENARIO": scenario, "SENNIT_LSP_TOOL_ROOT": root},
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
