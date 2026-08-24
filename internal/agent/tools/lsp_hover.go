package tools

import (
	"context"
	"fmt"
	"path/filepath"

	"charm.land/fantasy"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/rave-soft/sennit/internal/filepathext"
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
			c, path, line, char = exact[0].client, exact[0].symbol.Path, exact[0].symbol.Line-1, exact[0].symbol.Character-1
		} else {
			if p.FilePath == "" || p.Line < 0 || p.Character < 0 {
				return fantasy.NewTextErrorResponse("provide symbol or file_path, line, and character"), nil
			}
			path = filepathext.SmartJoin(root, p.FilePath)
			if rel, err := filepath.Rel(root, path); err != nil || rel == ".." {
				return fantasy.NewTextErrorResponse("file_path must be inside the workspace"), nil
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
			return fantasy.NewTextErrorResponse(fmt.Sprintf("hover failed: %s", err)), nil
		}
		if h == nil {
			return fantasy.NewTextResponse("No hover information found."), nil
		}
		return fantasy.NewTextResponse(hoverContents(h)), nil
	})
}
func hoverContents(h *protocol.Hover) string { return h.Contents.Value }
