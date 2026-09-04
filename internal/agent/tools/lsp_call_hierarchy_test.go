package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/stretchr/testify/require"
)

// callHierarchyHelperProcessEnv gates callHierarchyLSPHelper below: it is a
// dedicated fake LSP server for this file only, distinct from
// runLSPToolHelper in lsp_manager_e2e_test.go, because that shared harness
// has no scenario that can hold a request open — every response it sends is
// synchronous and immediate, which cannot exercise a context canceled while
// a request is genuinely in flight.
const callHierarchyHelperProcessEnv = "SENNIT_CALL_HIERARCHY_HELPER"

// TestCallHierarchyLSPHelperProcess is a fake LSP server that answers
// initialize and textDocument/definition immediately — enough for
// resolveSymbol to succeed — but never responds to
// textDocument/prepareCallHierarchy, so a caller waiting on that request is
// the one left to notice its context was canceled.
func TestCallHierarchyLSPHelperProcess(t *testing.T) {
	if os.Getenv(callHierarchyHelperProcessEnv) != "1" {
		return
	}
	runCallHierarchyLSPHelper()
	os.Exit(0)
}

func runCallHierarchyLSPHelper() {
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
			continue // Not a request awaiting a reply (or unparseable).
		}

		var result string
		switch request.Method {
		case "initialize":
			result = `{"capabilities":{"definitionProvider":true,"callHierarchyProvider":true}}`
		case "textDocument/definition":
			root := filepath.ToSlash(os.Getenv("SENNIT_LSP_TOOL_ROOT"))
			uri := "file://" + filepath.ToSlash(filepath.Join(root, "a.go"))
			result = fmt.Sprintf(`[{"uri":%q,"range":{"start":{"line":2,"character":5},"end":{"line":2,"character":10}}}]`, uri)
		case "textDocument/prepareCallHierarchy":
			// Signal the request arrived, then never answer it — see the
			// doc comment above. The marker lets the test cancel its
			// context exactly once this request is in flight, rather than
			// guessing at a delay that could race resolveSymbol's own
			// (already correct) cancellation handling instead.
			if marker := os.Getenv("SENNIT_CALL_HIERARCHY_MARKER"); marker != "" {
				_ = os.WriteFile(marker, []byte("1"), 0o644)
			}
			continue
		default:
			result = "null"
		}
		response := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, result))
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(response), response)
	}
}

func newCallHierarchyE2EManager(t *testing.T, root, marker string) *lsp.Manager {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	autoLSP := false
	store := configtest.NewStore(t, &config.Config{
		Options: &config.Options{AutoLSP: &autoLSP},
		LSP: config.LSPs{"gopls": {
			Command: exe,
			Args:    []string{"-test.run=^TestCallHierarchyLSPHelperProcess$"},
			Env: map[string]string{
				callHierarchyHelperProcessEnv: "1", "SENNIT_LSP_TOOL_ROOT": root,
				"SENNIT_CALL_HIERARCHY_MARKER": marker,
			},
			FileTypes:   []string{"go"},
			RootMarkers: []string{"go.mod"},
			Timeout:     5,
		}},
	}, configtest.WithWorkingDir(root))
	manager := lsp.NewManager(store)
	t.Cleanup(func() { manager.StopAll(t.Context()) })
	return manager
}

// TestCallHierarchyTool_ContextCanceledIsGoErrorNotTextResponse is the
// regression test for D5: PrepareCallHierarchy's (and, by the same
// pattern, IncomingCalls'/OutgoingCalls') error used to be wrapped in
// fantasy.NewTextErrorResponse unconditionally, so canceling the context
// (e.g. the user hitting Esc) while a call hierarchy request is genuinely
// in flight came back to the model as an ordinary tool result reading
// "failed to prepare call hierarchy: context canceled" instead of
// aborting the tool-call batch the way every other infrastructure failure
// does. The fake server here never answers prepareCallHierarchy, so the
// only way this call ends is the context being canceled while it waits.
func TestCallHierarchyTool_ContextCanceledIsGoErrorNotTextResponse(t *testing.T) {
	root := newLSPToolWorktree(t)
	marker := filepath.Join(t.TempDir(), "prepared")
	manager := newCallHierarchyE2EManager(t, root, marker)
	tool := NewCallHierarchyTool(manager, root)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		// Cancel exactly once the fake server confirms it received
		// prepareCallHierarchy, so this never races resolveSymbol's own
		// (separately correct) request ahead of it.
		for {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()

	input, err := json.Marshal(CallHierarchyParams{Symbol: "Exact", Direction: "incoming"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{Input: string(input)})
	require.Error(t, err, "a canceled context must abort as a Go error, not a text response")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, fantasy.ToolResponse{}, resp)
}

// TestCallHierarchyNotFoundMentionsTruncation is the regression test for
// finding 3: lsp_call_hierarchy used to discard resolveSymbol's truncated
// flag, so a capped grep whose matched candidates all lacked an LSP
// client read back identically to a symbol that genuinely does not exist
// anywhere in the tree.
func TestCallHierarchyNotFoundMentionsTruncation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeManySymbolMatches(t, root)

	manager := newNoLSPManager(t)
	tool := NewCallHierarchyTool(manager, root)
	resp := runToolWith(t, tool, t.Context(), CallHierarchyToolName, CallHierarchyParams{Symbol: "count", Direction: "incoming", Path: "."})

	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "Symbol 'count' not found")
	require.Contains(t, resp.Content, "match limit",
		"a capped grep must say so instead of reading back exactly like a genuine miss")
}
