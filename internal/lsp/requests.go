package lsp

import (
	"context"
	"time"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
)

// requests dispatches language intelligence requests (hover, definition,
// references, symbols, rename, call hierarchy) to the current generation
// and normalizes the responses. It never owns the runtime: every call
// resolves the generation through gen() at dispatch time.
type requests struct {
	// gen returns the generation the request should be sent on.
	gen func() *clientGeneration
	// ensureOpen opens the file on demand before a request that needs it.
	ensureOpen func(ctx context.Context, filepath string) error
}

func newRequests(gen func() *clientGeneration, ensureOpen func(ctx context.Context, filepath string) error) *requests {
	return &requests{gen: gen, ensureOpen: ensureOpen}
}

// FindReferences finds all references to the symbol at the given position.
func (q *requests) FindReferences(ctx context.Context, filepath string, line, character int, includeDeclaration bool) ([]protocol.Location, error) {
	if err := q.ensureOpen(ctx, filepath); err != nil {
		return nil, err
	}

	// Add timeout to prevent hanging on slow LSP servers.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// NOTE: line and character should be 0-based.
	// See: https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification/#position
	return q.gen().client.FindReferences(ctx, filepath, line-1, character-1, includeDeclaration)
}

// Rename renames the symbol at the given position across all files.
func (q *requests) Rename(ctx context.Context, filepath string, line, character int, newName string) (*protocol.WorkspaceEdit, error) {
	if err := q.ensureOpen(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return q.gen().client.RequestRename(ctx, filepath, line-1, character-1, newName) //nolint:wrapcheck
}

// Hover returns hover information at a file position.
func (q *requests) Hover(ctx context.Context, filepath string, line, character int) (*protocol.Hover, error) {
	if err := q.ensureOpen(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// NOTE: line and character should be 0-based.
	// See: https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification/#position
	return q.gen().client.RequestHover(ctx, filepath, protocol.Position{Line: uint32(line - 1), Character: uint32(character - 1)}) //nolint:wrapcheck
}

type WorkspaceSymbol struct {
	Name            string
	Kind            protocol.SymbolKind
	Path            string
	Line, Character int
	Container       string
}

// WorkspaceSymbolResults normalizes the legacy SymbolInformation and modern
// WorkspaceSymbol workspace/symbol result variants.
func (q *requests) WorkspaceSymbolResults(ctx context.Context, query string) ([]WorkspaceSymbol, error) {
	raw, err := q.gen().client.RequestWorkspaceSymbols(ctx, query)
	if err != nil {
		return nil, err
	}
	return normalizeWorkspaceSymbolResults(raw)
}

func normalizeWorkspaceSymbolResults(raw protocol.Or_Result_workspace_symbol) ([]WorkspaceSymbol, error) {
	results, err := raw.Results()
	if err != nil {
		return nil, err
	}
	out := make([]WorkspaceSymbol, 0, len(results))
	for _, result := range results {
		loc := result.GetLocation()
		path, err := loc.URI.Path()
		if err != nil || path == "" {
			continue
		}
		item := WorkspaceSymbol{Name: result.GetName(), Path: path, Line: int(loc.Range.Start.Line) + 1, Character: int(loc.Range.Start.Character) + 1}
		switch v := result.(type) {
		case *protocol.WorkspaceSymbol:
			item.Kind, item.Container = v.Kind, v.ContainerName
		case *protocol.SymbolInformation:
			item.Kind, item.Container = v.Kind, v.ContainerName
		}
		out = append(out, item)
	}
	return out, nil
}

// SupportsWorkspaceSymbols reports whether the initialized server advertises workspace symbols.
func (q *requests) SupportsWorkspaceSymbols() bool {
	return q.gen().client.GetCapabilities().WorkspaceSymbolProvider != nil
}

// SupportsHover reports whether the initialized server advertises hover support.
func (q *requests) SupportsHover() bool {
	return q.gen().client.GetCapabilities().HoverProvider != nil
}

// DocumentSymbols returns the document symbols for the given file.
func (q *requests) DocumentSymbols(ctx context.Context, filepath string) ([]protocol.DocumentSymbolResult, error) {
	if err := q.ensureOpen(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	return q.gen().client.RequestDocumentSymbols(ctx, filepath) //nolint:wrapcheck
}

// Definition finds the definition of the symbol at the given position.
func (q *requests) Definition(ctx context.Context, filepath string, line, character int) ([]protocol.Location, error) {
	if err := q.ensureOpen(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	return q.gen().client.RequestDefinition(ctx, filepath, line-1, character-1) //nolint:wrapcheck
}

// PrepareCallHierarchy prepares a call hierarchy item at the given position.
func (q *requests) PrepareCallHierarchy(ctx context.Context, filepath string, line, character int) ([]protocol.CallHierarchyItem, error) {
	if err := q.ensureOpen(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	return q.gen().client.PrepareCallHierarchy(ctx, filepath, line-1, character-1) //nolint:wrapcheck
}

// IncomingCalls returns all callers of the given call hierarchy item.
func (q *requests) IncomingCalls(ctx context.Context, item protocol.CallHierarchyItem) ([]protocol.CallHierarchyIncomingCall, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return q.gen().client.IncomingCalls(ctx, item) //nolint:wrapcheck
}

// OutgoingCalls returns all callees of the given call hierarchy item.
func (q *requests) OutgoingCalls(ctx context.Context, item protocol.CallHierarchyItem) ([]protocol.CallHierarchyOutgoingCall, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return q.gen().client.OutgoingCalls(ctx, item) //nolint:wrapcheck
}
