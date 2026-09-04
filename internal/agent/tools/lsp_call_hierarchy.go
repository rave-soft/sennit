package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/lsp"
)

type CallHierarchyParams struct {
	Symbol    string `json:"symbol" description:"The symbol name to show call hierarchy for"`
	Direction string `json:"direction" description:"Either 'incoming' (who calls this) or 'outgoing' (what does this call)"`
	Path      string `json:"path,omitempty" description:"The directory to search in. Defaults to the current working directory."`
}

const CallHierarchyToolName = "lsp_call_hierarchy"

//go:embed lsp_call_hierarchy.md
var callHierarchyDescription string

func NewCallHierarchyTool(lspManager *lsp.Manager, workingDir string) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewAgentTool(
		CallHierarchyToolName,
		callHierarchyDescription,
		func(ctx context.Context, params CallHierarchyParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Symbol == "" {
				return invalidParam("symbol"), nil
			}
			if params.Direction != "incoming" && params.Direction != "outgoing" {
				return fantasy.NewTextErrorResponse("direction must be 'incoming' or 'outgoing'"), nil
			}
			// Resolve against the workspace, not the process cwd. A
			// thread runs its agent in its own worktree while the
			// process stays in the main checkout, so "." — and any
			// relative path the model gives — pointed at the wrong tree
			// entirely: the tools searched the main checkout, or found
			// no LSP client for a file that plainly exists.
			searchDir := filepathext.SmartJoin(workingDir, params.Path)
			resolved, err := resolveSymbol(ctx, lspManager, params.Symbol, searchDir)
			if err != nil {
				if !isGenuineSymbolMiss(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("resolve symbol: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Symbol '%s' not found", params.Symbol)), nil
			}

			// Checked before the round trip, not only after it fails: a
			// canceled context does not reliably make the request return
			// an error - a server that answers first hands back a good
			// result - so testing only the failure path leaves the abort
			// depending on who wins a race. See NewHoverTool.
			if err := ctx.Err(); err != nil {
				return fantasy.ToolResponse{}, err
			}
			items, err := resolved.client.PrepareCallHierarchy(ctx, resolved.path, resolved.line, resolved.char)
			if err != nil {
				// A canceled context (Esc) must abort the tool-call batch
				// like any other infrastructure failure, not read back to
				// the model as text — see lsp_helpers.go's rule, applied
				// here and at the two lookups below.
				if ctx.Err() != nil {
					return fantasy.ToolResponse{}, ctx.Err()
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to prepare call hierarchy: %s", err)), nil
			}
			if len(items) == 0 {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("No call hierarchy information for '%s'", params.Symbol)), nil
			}

			item := items[0]

			var b strings.Builder
			fmt.Fprintf(&b, "Call hierarchy for '%s':\n\n", item.Name)

			if params.Direction == "incoming" {
				// Checked before the round trip as well: see NewHoverTool
				// for why a canceled context that the server happens to
				// answer first must not come back as a normal result.
				if err := ctx.Err(); err != nil {
					return fantasy.ToolResponse{}, err
				}
				calls, err := resolved.client.IncomingCalls(ctx, item)
				if err != nil {
					if ctx.Err() != nil {
						return fantasy.ToolResponse{}, ctx.Err()
					}
					return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to get incoming calls: %s", err)), nil
				}
				if len(calls) == 0 {
					b.WriteString("No incoming calls found.\n")
				} else {
					fmt.Fprintf(&b, "%d caller(s):\n\n", len(calls))
					for _, c := range calls {
						path, _ := c.From.URI.Path()
						line := c.From.Range.Start.Line + 1
						fmt.Fprintf(&b, "  %s:%d — %s\n", path, line, c.From.Name)
					}
				}
			} else {
				// Checked before the round trip as well: see NewHoverTool
				// for why a canceled context that the server happens to
				// answer first must not come back as a normal result.
				if err := ctx.Err(); err != nil {
					return fantasy.ToolResponse{}, err
				}
				calls, err := resolved.client.OutgoingCalls(ctx, item)
				if err != nil {
					if ctx.Err() != nil {
						return fantasy.ToolResponse{}, ctx.Err()
					}
					return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to get outgoing calls: %s", err)), nil
				}
				if len(calls) == 0 {
					b.WriteString("No outgoing calls found.\n")
				} else {
					fmt.Fprintf(&b, "%d callee(s):\n\n", len(calls))
					for _, c := range calls {
						path, _ := c.To.URI.Path()
						line := c.To.Range.Start.Line + 1
						fmt.Fprintf(&b, "  %s:%d — %s\n", path, line, c.To.Name)
					}
				}
			}

			return fantasy.NewTextResponse(b.String()), nil
		},
	), map[string]toolParameterSchema{"symbol": {minLength: intPtr(1)}, "direction": {enum: []string{"incoming", "outgoing"}}})
}
