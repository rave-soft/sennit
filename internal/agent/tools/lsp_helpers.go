package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/rave-soft/sennit/internal/lsp"
)

// errSymbolNotFound marks a resolveSymbol/resolveSymbolResults failure as
// "the search completed and matched nothing" rather than "the search
// failed". Callers map only an error wrapping this sentinel to the
// model-facing not-found text; anything else — a canceled walk, a
// filesystem permission error, an LSP round-trip failure — is an
// infrastructure failure and must propagate as a Go error instead.
var errSymbolNotFound = errors.New("no matching symbol")

// notFoundWithTruncationNote appends a note to a "not found" message when
// the grep behind the failed lookup was truncated. Without it, a symbol
// that only appears past the grep's match cap reads back identically to
// one that genuinely does not exist anywhere in the tree.
func notFoundWithTruncationNote(message string, truncated bool) string {
	if !truncated {
		return message
	}
	return message + " (the search hit its match limit; some candidate locations were never checked, so this symbol may exist)"
}

// isGenuineSymbolMiss reports whether err from resolveSymbol/
// resolveSymbolResults is a genuine "no such symbol" — the search
// completed and matched nothing — rather than an infrastructure failure:
// a canceled walk, a filesystem permission error, an LSP round-trip
// failure. A caller maps only a genuine miss (true) to its own
// model-facing not-found text; anything else must propagate as a Go
// error instead, so cancellation aborts the tool-call batch rather than
// being reported as "not found". Cancellation errors are never also
// wrapped with errSymbolNotFound, so this one check is sufficient — a
// caller does not need a separate ctx.Err()/context.Canceled check
// alongside it.
func isGenuineSymbolMiss(err error) bool {
	return errors.Is(err, errSymbolNotFound)
}

// resolvedSymbol holds the result of resolving a symbol name to an LSP position.
type resolvedSymbol struct {
	client *lsp.Client
	path   string
	line   int
	char   int
}

// resolveSymbol greps for a symbol name, triggers lazy LSP startup, and
// returns the first match position that a running LSP client confirms
// is a valid identifier. Matches inside comments or strings are skipped
// automatically because the LSP will reject them.
//
// The returned bool reports whether the underlying grep was truncated —
// same meaning as resolveSymbolResults' own truncated return. A caller
// that maps a genuine miss to a "not found" message must fold this in:
// otherwise a symbol that only appears in an untried tail of a capped
// grep reads back as if the search had covered everything.
func resolveSymbol(ctx context.Context, lspManager *lsp.Manager, symbol, workingDir string) (*resolvedSymbol, bool, error) {
	results, truncated, err := resolveSymbolResults(ctx, lspManager, symbol, workingDir)
	if err != nil {
		return nil, truncated, err
	}
	resolved, err := firstSymbolWithDefinition(ctx, results)
	return resolved, truncated, err
}

func firstSymbolWithDefinition(ctx context.Context, results []*resolvedSymbol) (*resolvedSymbol, error) {
	return firstWithDefinition(results, func(r *resolvedSymbol) ([]protocol.Location, error) {
		// The position tried here came from a grep against disk moments
		// ago, but r.client's overlay for r.path can still describe an
		// older version — read and bash never send didChange. Against a
		// stale overlay this Definition call misses the identifier
		// entirely, isNoIdentifierError swallows that as "not an
		// identifier", and a genuine symbol comes back as "not found"
		// instead. Sync first so the position and the overlay agree.
		if err := syncOverlay(ctx, r.client, r.path); err != nil {
			return nil, err
		}
		return r.client.Definition(ctx, r.path, r.line, r.char)
	})
}

func firstWithDefinition[T any](results []T, definition func(T) ([]protocol.Location, error)) (T, error) {
	var zero T
	var lastErr error
	for _, r := range results {
		locations, err := definition(r)
		if err == nil && len(locations) > 0 {
			return r, nil
		}
		if err != nil && !isNoIdentifierError(err) {
			lastErr = err
		}
	}
	if lastErr != nil {
		return zero, lastErr
	}
	return zero, fmt.Errorf("no definition found for any symbol candidate: %w", errSymbolNotFound)
}

// resolveSymbolResults greps for a symbol and returns all viable
// {client, path, position} tuples, plus whether the grep itself was
// capped. Callers that need just one match (definition, rename, call
// hierarchy) use resolveSymbol; callers that want to iterate all matches
// (references) use this directly and must surface truncated themselves —
// a capped grep means candidate positions belonging to a same-named but
// distinct symbol may never have been tried.
func resolveSymbolResults(ctx context.Context, lspManager *lsp.Manager, symbol, workingDir string) ([]*resolvedSymbol, bool, error) {
	// Use word boundaries to avoid matching inside larger identifiers
	// (e.g. "Bar" inside "myBar"). The symbol is already QuoteMeta'd
	// so dots and other regex metacharacters are escaped.
	pattern := `\b` + regexp.QuoteMeta(symbol) + `\b`
	matches, truncated, err := searchFiles(ctx, pattern, workingDir, "", 100)
	if err != nil {
		return nil, false, fmt.Errorf("failed to search for symbol: %w", err)
	}
	if len(matches) == 0 {
		// truncated carries through even here rather than being hardcoded
		// false: searchFiles reports it capped only when a match limit
		// was actually hit, and a zero-match capped search cannot happen
		// in practice, but callers checking this return value should see
		// what the search actually did, not an assumption about it.
		return nil, truncated, fmt.Errorf("symbol '%s' not found in grep results: %w", symbol, errSymbolNotFound)
	}

	var results []*resolvedSymbol
	for _, match := range matches {
		absPath, err := filepath.Abs(match.path)
		if err != nil {
			continue
		}

		// Server selection is per-file (handlesFiletype on a directory
		// never matches), so starting on workingDir started nothing. Start
		// on the matched file itself, before the first read/edit has had a
		// chance to open one.
		lspManager.Start(ctx, absPath)
		client := findLSPClient(lspManager, absPath)
		if client == nil {
			continue
		}

		results = append(results, &resolvedSymbol{
			client: client,
			path:   absPath,
			line:   match.lineNum,
			char:   match.charNum + getSymbolOffset(symbol),
		})
	}

	if len(results) == 0 {
		// Same reasoning as the zero-matches branch above: a capped grep
		// whose every match happens to land in a file with no LSP client
		// is still a capped grep, so truncated must say so rather than
		// silently reporting false.
		return nil, truncated, fmt.Errorf("no LSP client handles any file matching '%s': %w", symbol, errSymbolNotFound)
	}
	return results, truncated, nil
}

// findLSPClient returns the first LSP client that handles the given file path.
func findLSPClient(lspManager *lsp.Manager, filePath string) *lsp.Client {
	if abs, err := filepath.Abs(filePath); err == nil {
		filePath = abs
	}
	for c := range lspManager.Clients().Seq() {
		if c.HandlesFile(filePath) {
			return c
		}
	}
	return nil
}

// collectAffectedFiles extracts all unique file paths from a WorkspaceEdit.
func collectAffectedFiles(edit *protocol.WorkspaceEdit) []string {
	seen := make(map[string]struct{})
	var files []string

	for uri := range edit.Changes {
		path, err := uri.Path()
		if err != nil {
			continue
		}
		if _, ok := seen[path]; !ok {
			seen[path] = struct{}{}
			files = append(files, path)
		}
	}

	addURI := func(uri protocol.DocumentURI) {
		path, err := uri.Path()
		if err != nil {
			return
		}
		if _, ok := seen[path]; !ok {
			seen[path] = struct{}{}
			files = append(files, path)
		}
	}

	for _, change := range edit.DocumentChanges {
		switch {
		case change.TextDocumentEdit != nil:
			addURI(change.TextDocumentEdit.TextDocument.URI)
		case change.CreateFile != nil:
			addURI(change.CreateFile.URI)
		case change.RenameFile != nil:
			addURI(change.RenameFile.OldURI)
			addURI(change.RenameFile.NewURI)
		case change.DeleteFile != nil:
			addURI(change.DeleteFile.URI)
		}
	}

	return files
}

// isNoIdentifierError checks if an error indicates the grep match was not
// actually an identifier (e.g., matched inside a comment or string).
func isNoIdentifierError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no identifier found")
}

// getSymbolOffset returns the character offset to the actual symbol name
// in a qualified symbol (e.g., "Bar" in "foo.Bar" or "method" in "Class::method").
func getSymbolOffset(symbol string) int {
	if idx := strings.LastIndex(symbol, "::"); idx != -1 {
		return idx + 2
	}
	if idx := strings.LastIndex(symbol, "."); idx != -1 {
		return idx + 1
	}
	if idx := strings.LastIndex(symbol, "\\"); idx != -1 {
		return idx + 1
	}
	return 0
}

// cleanupLocations deduplicates and sorts a slice of LSP locations.
func cleanupLocations(locations []protocol.Location) []protocol.Location {
	slices.SortFunc(locations, func(a, b protocol.Location) int {
		if a.URI != b.URI {
			return strings.Compare(string(a.URI), string(b.URI))
		}
		if a.Range.Start.Line != b.Range.Start.Line {
			return cmp.Compare(a.Range.Start.Line, b.Range.Start.Line)
		}
		return cmp.Compare(a.Range.Start.Character, b.Range.Start.Character)
	})
	return slices.CompactFunc(locations, func(a, b protocol.Location) bool {
		return a.URI == b.URI &&
			a.Range.Start.Line == b.Range.Start.Line &&
			a.Range.Start.Character == b.Range.Start.Character
	})
}
