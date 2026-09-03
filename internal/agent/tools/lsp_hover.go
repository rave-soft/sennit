package tools

import (
	"context"
	"fmt"

	"charm.land/fantasy"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/lsp"
)

const HoverToolName = "lsp_hover"

type HoverParams struct {
	FilePath  string `json:"file_path,omitempty"`
	Line      int    `json:"line,omitempty"`
	Character int    `json:"character,omitempty"`
	Symbol    string `json:"symbol,omitempty"`
	Path      string `json:"path,omitempty"`
}

func NewHoverTool(m *lsp.Manager, root string) fantasy.AgentTool {
	return fantasy.NewAgentTool(HoverToolName, "Get type, signature, and documentation at a position or for an unambiguous symbol.", func(ctx context.Context, p HoverParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		var c *lsp.Client
		path, line, char := "", 0, 0
		if p.Symbol != "" {
			matches, err := workspaceSymbolMatches(ctx, m, root, p.Symbol, "", p.Path)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			exact := matches[:0]
			for _, match := range matches {
				if match.symbol.Name == p.Symbol {
					exact = append(exact, match)
				}
			}
			if len(exact) != 1 {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("symbol %q is ambiguous or not found; specify file_path and position", p.Symbol)), nil
			}
			// c.Hover takes 1-based line/character like the other position-based
			// requests (see requests.go), so pass the symbol's position through
			// unadjusted - a compensating -1 here used to double up with the
			// conversion requests.Hover now does, landing one line up and one
			// column left of the symbol.
			c, path, line, char = exact[0].client, exact[0].symbol.Path, exact[0].symbol.Line, exact[0].symbol.Character
		} else {
			if p.FilePath == "" {
				return fantasy.NewTextErrorResponse("provide symbol or file_path, line, and character"), nil
			}
			path = filepathext.SmartJoin(root, p.FilePath)
			if !fsext.HasPrefix(path, root) {
				return fantasy.NewTextErrorResponse("file_path must be inside the workspace"), nil
			}
			// line and character are 1-based (requests.Hover subtracts one
			// to reach the LSP wire position); line's zero value is also
			// its omitempty zero value, so a plain "not provided" and an
			// explicit "line 0" are indistinguishable and both must be
			// rejected here rather than silently underflowing to line
			// 4294967295 once converted.
			if p.Line < 1 || p.Character < 1 {
				return fantasy.NewTextErrorResponse("line and character must be 1-based positive positions"), nil
			}
			m.Start(ctx, path)
			c = findLSPClient(m, path)
			line, char = p.Line, p.Character
		}
		if c == nil || !c.SupportsHover() {
			return fantasy.NewTextErrorResponse("no hover-capable LSP client handles the requested location"), nil
		}
		h, err := c.Hover(ctx, path, line, char)
		if err != nil {
			// A canceled context (Esc) must abort the tool-call batch
			// like any other infrastructure failure, not read back to
			// the model as "hover failed: context canceled" — see
			// lsp_helpers.go's rule for the sibling symbol-lookup tools.
			if ctx.Err() != nil {
				return fantasy.ToolResponse{}, ctx.Err()
			}
			return fantasy.NewTextErrorResponse(fmt.Sprintf("hover failed: %s", err)), nil
		}
		if h == nil {
			return fantasy.NewTextResponse("No hover information found."), nil
		}
		return fantasy.NewTextResponse(hoverContents(h)), nil
	})
}
func hoverContents(h *protocol.Hover) string { return h.Contents.Value }
