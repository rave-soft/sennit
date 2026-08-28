package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/fsext"
)

const GlobToolName = "glob"

//go:embed glob.md.tpl
var globDescriptionTmpl []byte

var globDescriptionTpl = template.Must(
	template.New("globDescription").
		Parse(string(globDescriptionTmpl)),
)

type globDescriptionData struct {
	MaxResults int
}

func globDescription() string {
	return renderTemplate(globDescriptionTpl, globDescriptionData{
		MaxResults: 100,
	})
}

type GlobParams struct {
	Pattern    string `json:"pattern" description:"The glob pattern to match files against"`
	Path       string `json:"path,omitempty" description:"The directory to search in. Defaults to the current working directory."`
	MaxResults int    `json:"max_results,omitempty" description:"Maximum results (1-1000, defaults to 100)"`
	Cursor     string `json:"cursor,omitempty" description:"Stable continuation token returned by a previous response"`
}

type GlobResponseMetadata struct {
	NumberOfFiles int    `json:"number_of_files"`
	TotalFiles    int    `json:"total_files"`
	Truncated     bool   `json:"truncated"`
	Cursor        string `json:"cursor,omitempty"`
}

func NewGlobTool(workingDir string, cfg config.ToolGlob) fantasy.AgentTool {
	tool := fantasy.NewParallelAgentTool(
		GlobToolName,
		globDescription(),
		func(ctx context.Context, params GlobParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Pattern == "" {
				return invalidParam("pattern"), nil
			}

			// A relative path is relative to the workspace, the same as
			// for every file tool. cmp.Or left it raw, so it resolved
			// against the process cwd — in a thread, the main checkout
			// rather than the worktree the agent is working in.
			searchPath := filepathext.SmartJoin(workingDir, params.Path)

			// Bound the search so a huge or symlink-heavy root (e.g. $HOME
			// or a module cache) fails cleanly instead of pinning the CPU
			// and hanging the agent.
			searchCtx, cancel := context.WithTimeout(ctx, cfg.GetTimeout())
			defer cancel()

			limit := params.MaxResults
			if limit == 0 {
				limit = 100
			}
			if limit < 1 || limit > maxPageResults {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("max_results must be between 1 and %d", maxPageResults)), nil
			}
			query := fingerprintPage(canonicalPath(searchPath), params.Pattern)
			continuation, err := openPageKeyCursor(params.Cursor, "glob", query)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			scan := newPageScan[string](continuation.Last, limit)
			err = visitGlobFiles(searchCtx, params.Pattern, searchPath, func(path string) { scan.Add(path, path) })
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("error finding files: %v", err)), nil
			}
			files, last, truncated, total, generation := scan.Finish()
			if err := finishPageKeyCursor(continuation, generation); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			normalizeFilePaths(files)
			output := "No files found"
			if len(files) > 0 {
				output = strings.Join(files, "\n")
			}
			if truncated {
				output += "\n\n(Results are truncated. Consider using a more specific path or pattern.)"
			}
			cursor := ""
			if truncated {
				cursor = makePageKeyCursor("glob", query, generation, last)
			}
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(output), GlobResponseMetadata{NumberOfFiles: len(files), TotalFiles: total, Truncated: truncated, Cursor: cursor}), nil
		},
	)
	return withToolParameterSchema(tool, map[string]toolParameterSchema{
		"pattern":     {minLength: intPtr(1)},
		"max_results": intSchemaBounds(maxPageResults),
	})
}

func visitGlobFiles(ctx context.Context, pattern, searchPath string, visit func(string)) error {
	prefix, rest := filepathext.SplitGlobPrefix(pattern)
	walkRoot, walkPattern := searchPath, pattern
	if prefix != "" {
		walkRoot, walkPattern = filepath.Join(searchPath, prefix), rest
	}
	return fsext.VisitGlobGitignoreAware(ctx, walkPattern, walkRoot, visit)
}

func normalizeFilePaths(paths []string) {
	for i, p := range paths {
		paths[i] = filepath.ToSlash(p)
	}
}
