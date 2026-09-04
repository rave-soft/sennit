package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/lsp"
)

type ReferencesParams struct {
	Symbol string `json:"symbol" description:"The symbol name to search for (e.g., function name, variable name, type name)"`
	Path   string `json:"path,omitempty" description:"The directory to search in. Use a directory/file to narrow down the symbol search. Defaults to the current working directory."`
}

const ReferencesToolName = "lsp_references"

//go:embed references.md
var referencesDescription string

func NewReferencesTool(lspManager *lsp.Manager, workingDir string) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewAgentTool(
		ReferencesToolName,
		referencesDescription,
		func(ctx context.Context, params ReferencesParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Symbol == "" {
				return invalidParam("symbol"), nil
			}

			// Resolve against the workspace, not the process cwd. A
			// thread runs its agent in its own worktree while the
			// process stays in the main checkout, so "." — and any
			// relative path the model gives — pointed at the wrong tree
			// entirely: the tools searched the main checkout, or found
			// no LSP client for a file that plainly exists.
			searchDir := filepathext.SmartJoin(workingDir, params.Path)
			results, truncated, err := resolveSymbolResults(ctx, lspManager, params.Symbol, searchDir)
			if err != nil {
				if !isGenuineSymbolMiss(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("resolve symbol: %w", err)
				}
				return fantasy.NewTextResponse(notFoundWithTruncationNote(
					fmt.Sprintf("Symbol '%s' not found", params.Symbol), truncated)), nil
			}

			var allLocations []protocol.Location
			var allErrs error
			var unqueried int
			// covered tracks every position that has already turned up as
			// a reference to some identity already queried. A candidate
			// whose own position is already in this set is a second (or
			// third, ...) grep hit on the *same* identity — its own
			// declaration line and every one of its use sites all match
			// the same text — so FindReferences on it would just repeat
			// the answer the first hit already produced. Skipping it is
			// what collapses a common local-variable name, where grep's
			// ~100 hits are ~100 uses of a much smaller number of
			// distinct variables, into one round trip per identity
			// instead of one per hit: measured on this repo, "filePath"
			// (100 grep candidates, 102 distinct references after
			// dedup) went from 656ms to 10ms. A candidate that names a
			// genuinely distinct identity (e.g. a method on two
			// different types) never lands in this set from another
			// candidate's results, so it is still queried in full — the
			// case finding A fixed stays fixed.
			covered := make(map[locKey]bool, len(results))
			for _, r := range results {
				if covered[locKeyFor(r.path, r.line-1, r.char-1)] {
					continue
				}
				// r's position came from a grep against disk, which can
				// already be ahead of r.client's overlay for r.path — read
				// and bash never send didChange. Resync first, or this
				// lookup can miss the identifier and read back as "no
				// references" for a symbol that plainly has some.
				if err := syncOverlay(ctx, r.client, r.path); err != nil {
					allErrs = errors.Join(allErrs, err)
					unqueried++
					continue
				}
				locations, err := r.client.FindReferences(ctx, r.path, r.line, r.char, true)
				if err != nil {
					if isNoIdentifierError(err) {
						continue
					}
					slog.Error("Failed to find references", "error", err, "symbol", params.Symbol, "path", r.path, "line", r.line)
					allErrs = errors.Join(allErrs, err)
					unqueried++
					continue
				}
				// Every viable candidate identity is queried, not just
				// the first one that answers: the grep pattern matches on
				// text alone, so a same-named but distinct symbol shows
				// up as a separate candidate here, and stopping early
				// silently dropped its references. FindReferences within
				// a candidate already returns every reference to that
				// identity project-wide, and cleanupLocations below
				// dedupes candidates that resolved to the same identity.
				for _, loc := range locations {
					if path, err := loc.URI.Path(); err == nil {
						covered[locKeyFor(path, int(loc.Range.Start.Line), int(loc.Range.Start.Character))] = true
					}
				}
				allLocations = append(allLocations, locations...)
			}

			note := referencesIncompleteNote(params.Symbol, truncated, unqueried)

			if len(allLocations) > 0 {
				return fantasy.NewTextResponse(formatReferences(cleanupLocations(allLocations)) + note), nil
			}

			if allErrs != nil {
				if ctx.Err() != nil || errors.Is(allErrs, context.Canceled) || errors.Is(allErrs, context.DeadlineExceeded) {
					return fantasy.ToolResponse{}, fmt.Errorf("find references: %w", allErrs)
				}
				return fantasy.NewTextErrorResponse(allErrs.Error() + note), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf("No references found for symbol '%s'", params.Symbol) + note), nil
		},
	), map[string]toolParameterSchema{"symbol": {minLength: intPtr(1)}})
}

// locKey identifies a position for the covered-candidate check above.
// Both sides that build one already agree on 0-based line/character:
// resolveSymbolResults' 1-based match position minus one on the grep
// side, protocol.Location's own Range.Start on the FindReferences side.
type locKey struct {
	path string
	line int
	char int
}

func locKeyFor(path string, line, char int) locKey {
	return locKey{path: path, line: line, char: char}
}

// referencesIncompleteNote reports why the result below it may not be the
// whole picture, so this reads the same whether it lands on a non-empty
// result, an error, or the empty "no references found" case — see
// finding 2: a truncated grep or an unqueried candidate must not go quiet
// just because it happened to land on the empty branch.
func referencesIncompleteNote(symbol string, truncated bool, unqueried int) string {
	var b strings.Builder
	if truncated {
		fmt.Fprintf(&b, "\n(Search for '%s' hit the match limit; some candidate locations for a "+
			"same-named but distinct symbol were never queried, so results may be incomplete.)\n", symbol)
	}
	if unqueried > 0 {
		fmt.Fprintf(&b, "\n(%d candidate location(s) could not be queried due to errors; "+
			"results may be missing references for a same-named but distinct symbol.)\n", unqueried)
	}
	return b.String()
}

func groupByFilename(locations []protocol.Location) map[string][]protocol.Location {
	files := make(map[string][]protocol.Location)
	for _, loc := range locations {
		path, err := loc.URI.Path()
		if err != nil {
			slog.Error("Failed to convert location URI to path", "uri", loc.URI, "error", err)
			continue
		}
		files[path] = append(files[path], loc)
	}
	return files
}

func formatReferences(locations []protocol.Location) string {
	fileRefs := groupByFilename(locations)
	files := slices.Collect(maps.Keys(fileRefs))
	sort.Strings(files)

	var output strings.Builder
	fmt.Fprintf(&output, "Found %d reference(s) in %d file(s):\n\n", len(locations), len(files))

	for _, file := range files {
		refs := fileRefs[file]
		fmt.Fprintf(&output, "%s (%d reference(s)):\n", file, len(refs))
		for _, ref := range refs {
			line := ref.Range.Start.Line + 1
			char := ref.Range.Start.Character + 1
			fmt.Fprintf(&output, "  Line %d, Column %d\n", line, char)
		}
		output.WriteString("\n")
	}

	return output.String()
}
