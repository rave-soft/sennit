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
			results, err := resolveSymbolResults(ctx, lspManager, params.Symbol, searchDir)
			if err != nil {
				if !isGenuineSymbolMiss(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("resolve symbol: %w", err)
				}
				return fantasy.NewTextResponse(fmt.Sprintf("Symbol '%s' not found", params.Symbol)), nil
			}

			var allLocations []protocol.Location
			var allErrs error
			for _, r := range results {
				// r's position came from a grep against disk, which can
				// already be ahead of r.client's overlay for r.path — read
				// and bash never send didChange. Resync first, or this
				// lookup can miss the identifier and read back as "no
				// references" for a symbol that plainly has some.
				if err := syncOverlay(ctx, r.client, r.path); err != nil {
					allErrs = errors.Join(allErrs, err)
					continue
				}
				locations, err := r.client.FindReferences(ctx, r.path, r.line, r.char, true)
				if err != nil {
					if isNoIdentifierError(err) {
						continue
					}
					slog.Error("Failed to find references", "error", err, "symbol", params.Symbol, "path", r.path, "line", r.line)
					allErrs = errors.Join(allErrs, err)
					continue
				}
				allLocations = append(allLocations, locations...)
				// LSP returns all references for the symbol, not just from this file.
				if len(locations) > 0 {
					break
				}
			}

			if len(allLocations) > 0 {
				output := formatReferences(cleanupLocations(allLocations))
				return fantasy.NewTextResponse(output), nil
			}

			if allErrs != nil {
				if ctx.Err() != nil || errors.Is(allErrs, context.Canceled) || errors.Is(allErrs, context.DeadlineExceeded) {
					return fantasy.ToolResponse{}, fmt.Errorf("find references: %w", allErrs)
				}
				return fantasy.NewTextErrorResponse(allErrs.Error()), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf("No references found for symbol '%s'", params.Symbol)), nil
		},
	), map[string]toolParameterSchema{"symbol": {minLength: intPtr(1)}})
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
