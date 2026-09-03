package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"math"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
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

// GlobParams is the canonical proto data shape used by both the runtime and UI.
type GlobParams = proto.GlobParams

// GlobPermissionsParams is defined in proto; see the comment on
// BashPermissionsParams in bash.go.
type GlobPermissionsParams = proto.GlobPermissionsParams

type GlobResponseMetadata struct {
	NumberOfFiles int  `json:"number_of_files"`
	TotalFiles    int  `json:"total_files"`
	Truncated     bool `json:"truncated"`
	// Incomplete is true when part of the search tree could not be read
	// and was silently left out of the match set. It is reported
	// separately from Truncated, which means the result limit cut the
	// matches short — a different fact from part of the tree never
	// having been read at all.
	Incomplete bool   `json:"incomplete,omitempty"`
	Cursor     string `json:"cursor,omitempty"`
}

func NewGlobTool(permissions permission.Requester, workingDir string, cfg config.ToolGlob) fantasy.AgentTool {
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

			absSearchPath, resolvedSearchPath, outside, err := resolveWithinWorkdir(workingDir, searchPath)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("resolve path: %w", err)
			}
			if outside {
				sessionID := GetSessionFromContext(ctx)
				if sessionID == "" {
					return fantasy.ToolResponse{}, missingSessionID("searching for files outside working directory")
				}

				path, description := outsideWorkdirNotice("List files outside working directory", absSearchPath, resolvedSearchPath)
				resp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        path,
					ToolCallID:  call.ID,
					ToolName:    GlobToolName,
					Action:      "list",
					Description: description,
					Params:      GlobPermissionsParams(params),
				})
				if err != nil {
					return fantasy.ToolResponse{}, err
				}
				if denied {
					return resp, nil
				}
			}

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
			incomplete, err := visitGlobFiles(searchCtx, params.Pattern, searchPath, func(path string, modTime time.Time) {
				scan.Add(globPageKey(path, modTime), path)
			})
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
			if incomplete {
				// A model-recoverable condition, not a Go error (AGENTS.md):
				// part of the tree could not be read (removed mid-walk,
				// permissions, EMFILE/ENFILE, a network mount hiccup), so
				// matches may be missing from a subtree that was never
				// read, separate from the result-limit truncation above.
				output += "\n\n(Part of the search tree could not be read, so some matches may be missing. Retry or narrow the path to confirm.)"
			}
			cursor := ""
			if truncated {
				cursor = makePageKeyCursor("glob", query, generation, last)
			}
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(output), GlobResponseMetadata{NumberOfFiles: len(files), TotalFiles: total, Truncated: truncated, Incomplete: incomplete, Cursor: cursor}), nil
		},
	)
	return withToolParameterSchema(tool, map[string]toolParameterSchema{
		"pattern":     {minLength: intPtr(1)},
		"max_results": intSchemaBounds(maxPageResults),
	})
}

// globPageKey orders the page newest-first while keeping the total order a
// keyset cursor needs. The scan pages by ascending key, so the timestamp is
// inverted; the path is appended so two files written in the same nanosecond
// still have one definite order, and so the key identifies the entry the
// cursor resumes after.
//
// Ordering by modification time is what this tool advertises ("sorted by
// modification time" in glob.md.tpl) and what a caller looking for recent
// work needs. It was lost when the tool moved to keyset pagination in
// 4052fdab0: the key became the path, the description was not updated, and
// the test that asserted newest-first kept passing because it exercised a
// collect-everything helper in fsext that nothing called any more.
//
// A file touched between two pages changes this key and therefore the scan
// generation, so the next page is refused as stale rather than silently
// skipping or repeating an entry — which is the whole point of the
// generation check, and the reason mtime is usable as a page key here at
// all.
func globPageKey(path string, modTime time.Time) string {
	nanos := modTime.UnixNano()
	// Clamp rather than let the subtraction wrap: a zero or pre-epoch
	// mtime (an unreadable info, an archive restored with a bogus time)
	// would otherwise produce a key that sorts before genuinely new files
	// instead of after everything.
	if nanos < 0 {
		nanos = 0
	}
	return fmt.Sprintf("%019d\x00%s", math.MaxInt64-nanos, path)
}

func visitGlobFiles(ctx context.Context, pattern, searchPath string, visit func(path string, modTime time.Time)) (incomplete bool, err error) {
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
