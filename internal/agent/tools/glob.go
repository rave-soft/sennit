package tools

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"os/exec"
	"path/filepath"
	"sort"
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
		"max_results": intSchemaBounds(1, maxPageResults),
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

func globFiles(ctx context.Context, pattern, searchPath string, limit int) ([]string, bool, error) {
	// Scope the walk to the pattern's literal directory prefix. A pattern
	// like "internal/agent/*.go" only needs to walk "internal/agent", so we
	// start there instead of enumerating the entire tree and filtering.
	// Patterns that begin with a wildcard (e.g. "**/foo.go") have no prefix
	// and still walk from searchPath.
	prefix, rest := filepathext.SplitGlobPrefix(pattern)
	walkRoot := searchPath
	walkPattern := pattern
	if prefix != "" {
		walkRoot = filepath.Join(searchPath, prefix)
		walkPattern = rest
	}

	cmdRg := getRgCmd(ctx, walkPattern)
	if cmdRg != nil {
		cmdRg.Dir = walkRoot
		matches, err := runRipgrep(cmdRg, walkRoot, limit)
		if err == nil {
			return matches, len(matches) >= limit && limit > 0, nil
		}
		slog.Warn("Ripgrep execution failed, falling back to doublestar", "error", err)
	}

	return fsext.GlobGitignoreAwareCtx(ctx, walkPattern, walkRoot, limit)
}

func runRipgrep(cmd *exec.Cmd, searchRoot string, limit int) ([]string, error) {
	// Stream ripgrep's stdout instead of buffering the whole file list.
	// Over a huge root (e.g. $HOME) the full --files listing can be
	// hundreds of MB; reading it all at once and then sorting allocated
	// gigabytes. We read incrementally and stop once we have a bounded
	// pool of candidates.
	//
	// We collect more than `limit` so the shortest-path preference below
	// still has something to choose from, but the pool is capped so memory
	// stays small (a few thousand paths) no matter how large the tree is.
	candidatePool := max(limit*20, 1000)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ripgrep: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ripgrep: %w", err)
	}

	var matches []string
	reader := bufio.NewReader(stdout)
	for {
		path, err := reader.ReadString(0)
		if len(path) > 0 {
			path = strings.TrimRight(path, "\x00")
			if path != "" {
				absPath := filepathext.SmartJoin(searchRoot, path)
				if !fsext.SkipHidden(absPath) {
					matches = append(matches, absPath)
				}
			}
		}
		if err != nil {
			break // EOF or read error; drain handled by Wait below.
		}
		if len(matches) >= candidatePool {
			// Enough candidates; stop reading and let the process be
			// killed by the command context / Wait. Draining the rest
			// would just buffer paths we are going to discard.
			break
		}
	}

	// Close our end so ripgrep gets SIGPIPE and stops, then reap it.
	_ = stdout.Close()
	waitErr := cmd.Wait()
	if waitErr != nil && len(matches) == 0 {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) && ee.ExitCode() == 1 {
			return nil, nil // No matches.
		}
		return nil, fmt.Errorf("ripgrep: %w\n%s", waitErr, stderr.String())
	}

	sort.SliceStable(matches, func(i, j int) bool {
		return len(matches[i]) < len(matches[j])
	})

	if limit > 0 && len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, nil
}

func normalizeFilePaths(paths []string) {
	for i, p := range paths {
		paths[i] = filepath.ToSlash(p)
	}
}
