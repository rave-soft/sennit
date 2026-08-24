package tools

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/fantasy"

	"github.com/rave-soft/sennit/internal/lsp"
)

const WorkspaceSymbolsToolName = "lsp_workspace_symbols"

type WorkspaceSymbolsParams struct {
	Query  string `json:"query"`
	Kind   string `json:"kind,omitempty"`
	Path   string `json:"path,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type workspaceSymbolMatch struct {
	symbol lsp.WorkspaceSymbol
	client *lsp.Client
}
type WorkspaceSymbolsMetadata struct {
	Count     int    `json:"count"`
	Total     int    `json:"total"`
	Truncated bool   `json:"truncated"`
	Cursor    string `json:"cursor,omitempty"`
}

func workspaceSymbolMatches(ctx context.Context, m *lsp.Manager, root, query, kind, path string) ([]workspaceSymbolMatch, error) {
	base := filepath.Clean(root)
	var matches []workspaceSymbolMatch
	clients := m.WorkspaceClients(ctx, root)
	capable := 0
	var rpcErrors []error
	for _, c := range clients {
		if !c.SupportsWorkspaceSymbols() {
			continue
		}
		capable++
		got, err := c.WorkspaceSymbolResults(ctx, query)
		if err != nil {
			rpcErrors = append(rpcErrors, fmt.Errorf("%s: %w", c.GetName(), err))
			continue
		}
		for _, x := range got {
			rel, err := filepath.Rel(base, x.Path)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			if path != "" && filepath.ToSlash(rel) != filepath.ToSlash(path) && !strings.HasPrefix(filepath.ToSlash(rel), strings.TrimSuffix(filepath.ToSlash(path), "/")+"/") {
				continue
			}
			if kind != "" && !strings.EqualFold(symbolKindNames[x.Kind], kind) {
				continue
			}
			matches = append(matches, workspaceSymbolMatch{x, c})
		}
	}
	if capable == 0 {
		return nil, fmt.Errorf("no workspace-symbol-capable LSP client is available")
	}
	if len(rpcErrors) == capable {
		return nil, fmt.Errorf("workspace symbol request failed: %w", errors.Join(rpcErrors...))
	}
	return matches, nil
}

func workspaceSymbolKey(x lsp.WorkspaceSymbol, query string) string {
	return fmt.Sprintf("%d\x00%s\x00%s\x00%09d\x00%09d", symbolRank(x.Name, query), strings.ToLower(x.Name), filepath.ToSlash(x.Path), x.Line, x.Character)
}

func NewWorkspaceSymbolsTool(m *lsp.Manager, root string) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewAgentTool(WorkspaceSymbolsToolName, "Search symbols across the workspace.", func(ctx context.Context, p WorkspaceSymbolsParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		p.Query = strings.TrimSpace(p.Query)
		if p.Query == "" {
			return invalidParam("query"), nil
		}
		limit := p.Limit
		if limit == 0 {
			limit = 50
		}
		if limit < 1 || limit > 200 {
			return fantasy.NewTextErrorResponse("limit must be between 1 and 200"), nil
		}
		fingerprint := fingerprintPage(root, p.Query, p.Kind, p.Path)
		continuation, err := openPageKeyCursor(p.Cursor, WorkspaceSymbolsToolName, fingerprint)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		all, err := workspaceSymbolMatches(ctx, m, root, p.Query, p.Kind, p.Path)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		scan := newPageScan[workspaceSymbolMatch](continuation.Last, limit)
		for _, x := range all {
			scan.Add(workspaceSymbolKey(x.symbol, p.Query), x)
		}
		page, last, truncated, total, generation := scan.Finish()
		if err := finishPageKeyCursor(continuation, generation); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		meta := WorkspaceSymbolsMetadata{Count: len(page), Total: total, Truncated: truncated}
		if truncated {
			meta.Cursor = makePageKeyCursor(WorkspaceSymbolsToolName, fingerprint, generation, last)
		}
		if len(page) == 0 {
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse("No workspace symbols found."), meta), nil
		}
		var b strings.Builder
		for _, x := range page {
			path, err := filepath.Rel(filepath.Clean(root), x.symbol.Path)
			if err != nil {
				path = x.symbol.Path
			}
			fmt.Fprintf(&b, "%s %s — %s:%d\n", symbolKindNames[x.symbol.Kind], x.symbol.Name, filepath.ToSlash(path), x.symbol.Line)
		}
		return fantasy.WithResponseMetadata(fantasy.NewTextResponse(b.String()), meta), nil
	}), map[string]toolParameterSchema{"query": {minLength: intPtr(1)}})
}

func symbolRank(name, q string) int {
	n, q := strings.ToLower(name), strings.ToLower(q)
	if n == q {
		return 0
	}
	if strings.HasPrefix(n, q) {
		return 1
	}
	return 2
}
