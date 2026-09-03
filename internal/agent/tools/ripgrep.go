package tools

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
)

// RipgrepParams is the canonical proto data shape used by both the runtime and UI.
type RipgrepParams = proto.RipgrepParams

// RipgrepPermissionsParams is defined in proto; see the comment on
// BashPermissionsParams in bash.go.
type RipgrepPermissionsParams = proto.RipgrepPermissionsParams

const RipgrepToolName = "ripgrep"

//go:embed ripgrep.md.tpl
var ripgrepDescriptionTmpl []byte

var ripgrepDescriptionTpl = template.Must(
	template.New("ripgrepDescription").
		Parse(string(ripgrepDescriptionTmpl)),
)

func ripgrepDescription() string {
	return renderTemplate(ripgrepDescriptionTpl, grepDescriptionData{
		MaxResults: 100,
	})
}

// NewSearchTool returns the content-search tool to offer the model on this
// system: ripgrep when the rg binary is installed, otherwise the pure-Go
// grep fallback.
func NewSearchTool(permissions permission.Requester, workingDir string, cfg config.ToolGrep) fantasy.AgentTool {
	return newSearchTool(permissions, workingDir, cfg, getRg())
}

func newSearchTool(permissions permission.Requester, workingDir string, cfg config.ToolGrep, ripgrepPath string) fantasy.AgentTool {
	if ripgrepPath != "" {
		return NewRipgrepTool(permissions, workingDir, cfg, withRipgrepCommand(ripgrepSearchCommand(ripgrepPath)))
	}
	return NewGrepTool(permissions, workingDir, cfg)
}

func ripgrepSearchCommand(name string) func(context.Context, string, string, string, bool) *exec.Cmd {
	return func(ctx context.Context, pattern, path, include string, caseInsensitive bool) *exec.Cmd {
		return newRgSearchCmd(ctx, name, pattern, path, include, caseInsensitive)
	}
}

// ripgrepToolOption supplies a controlled command backend for tests. Production
// callers use getRgSearchCmd; keeping the seam at process creation lets tests
// exercise the real tool handler without requiring rg to be installed.
type ripgrepToolOption func(*ripgrepToolOptions)

type ripgrepToolOptions struct {
	command func(context.Context, string, string, string, bool) *exec.Cmd
}

func withRipgrepCommand(command func(context.Context, string, string, string, bool) *exec.Cmd) ripgrepToolOption {
	return func(options *ripgrepToolOptions) {
		options.command = command
	}
}

func NewRipgrepTool(permissions permission.Requester, workingDir string, cfg config.ToolGrep, options ...ripgrepToolOption) fantasy.AgentTool {
	toolOptions := ripgrepToolOptions{command: getRgSearchCmd}
	for _, option := range options {
		option(&toolOptions)
	}
	tool := fantasy.NewParallelAgentTool(
		RipgrepToolName, ripgrepDescription(),
		func(ctx context.Context, params RipgrepParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Pattern == "" {
				return invalidParam("pattern"), nil
			}
			if params.BeforeContext < 0 || params.BeforeContext > maxGrepContextLines || params.AfterContext < 0 || params.AfterContext > maxGrepContextLines {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("context must be between 0 and %d lines", maxGrepContextLines)), nil
			}
			limit := params.MaxResults
			if limit == 0 {
				limit = 100
			}
			if limit < 1 || limit > maxPageResults {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("max_results must be between 1 and %d", maxPageResults)), nil
			}
			if params.Sort != "" && params.Sort != "path" && params.Sort != "mtime" {
				return fantasy.NewTextErrorResponse("sort must be path or mtime"), nil
			}
			if params.Sort == "" {
				params.Sort = "mtime"
			}
			pattern := params.Pattern
			if params.LiteralText {
				pattern = escapeRegexPattern(pattern)
			}
			searchPath := filepathext.SmartJoin(workingDir, params.Path)

			absSearchPath, resolvedSearchPath, outside, err := resolveWithinWorkdir(workingDir, searchPath)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("resolve path: %w", err)
			}
			if outside {
				resp, denied, err := requireOutsideWorkdirPermission(
					ctx, permissions, call,
					RipgrepToolName, "search", "Search file contents outside working directory",
					"searching file contents outside working directory",
					absSearchPath, resolvedSearchPath, RipgrepPermissionsParams(params),
				)
				if err != nil {
					return fantasy.ToolResponse{}, err
				}
				if denied {
					return resp, nil
				}
			}

			searchCtx, cancel := context.WithTimeout(ctx, cfg.GetTimeout())
			defer cancel()
			query := fingerprintPage(canonicalPath(searchPath), params.Pattern, params.Include, fmt.Sprint(params.LiteralText), fmt.Sprint(params.CaseInsensitive), params.Sort, fmt.Sprint(params.BeforeContext), fmt.Sprint(params.AfterContext))
			continuation, err := openPageKeyCursor(params.Cursor, "ripgrep", query)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			scan := newPageScan[grepMatch](continuation.Last, limit)
			err = visitRipgrepMatches(searchCtx, pattern, searchPath, params.Include, params.CaseInsensitive, toolOptions.command, func(match grepMatch) {
				scan.Add(grepMatchPageKey(match, params.Sort), match)
			})
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("error searching files: %v", err)), nil
			}
			page, last, truncated, total, generation := scan.Finish()
			if err := finishPageKeyCursor(continuation, generation); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			cursor := ""
			if truncated {
				cursor = makePageKeyCursor("ripgrep", query, generation, last)
			}
			output, err := renderGrepMatchesWithContext(searchCtx, page, truncated, params.BeforeContext, params.AfterContext)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("rendering search context: %w", err)
			}
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(output), GrepResponseMetadata{NumberOfMatches: len(page), TotalMatches: total, Truncated: truncated, Cursor: cursor}), nil
		},
	)
	return withToolParameterSchema(tool, map[string]toolParameterSchema{
		"pattern":        {minLength: intPtr(1)},
		"max_results":    intSchemaBounds(maxPageResults),
		"before_context": intSchemaBounds(maxGrepContextLines),
		"after_context":  intSchemaBounds(maxGrepContextLines),
	})
}

func visitRipgrepMatches(ctx context.Context, pattern, path, include string, caseInsensitive bool, command func(context.Context, string, string, string, bool) *exec.Cmd, visit func(grepMatch)) error {
	cmd := command(ctx, pattern, path, include, caseInsensitive)
	if cmd == nil {
		return fmt.Errorf("ripgrep not found in $PATH")
	}

	// Only add ignore files if they exist
	for _, ignoreFile := range []string{".gitignore", brand.IgnoreFile} {
		ignorePath := filepath.Join(path, ignoreFile)
		if _, err := os.Stat(ignorePath); err == nil {
			cmd.Args = append(cmd.Args, "--ignore-file", ignorePath)
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	reader := bufio.NewReader(stdout)
	var readErr error
	for readErr == nil {
		var record []byte
		record, readErr = reader.ReadBytes('\n')
		if len(record) == 0 {
			continue
		}
		var match ripgrepMatch
		if json.Unmarshal(record, &match) != nil || match.Type != "match" || len(match.Data.Submatches) == 0 {
			continue
		}
		fi, statErr := os.Stat(match.Data.Path.Text)
		if statErr != nil {
			continue
		}
		m := match.Data.Submatches[0]
		visit(grepMatch{path: match.Data.Path.Text, modTime: fi.ModTime(), lineNum: match.Data.LineNumber, charNum: m.Start + 1, lineText: strings.TrimRight(match.Data.Lines.Text, "\r\n")})
	}
	waitErr := cmd.Wait()
	if readErr != nil && readErr != io.EOF {
		return readErr
	}
	// rg uses exit code 1 for no matches, which is not an operational error.
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 1 {
			return waitErr
		}
	}
	return nil
}

type ripgrepMatch struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text string `json:"text"`
		} `json:"path"`
		Lines struct {
			Text string `json:"text"`
		} `json:"lines"`
		LineNumber int `json:"line_number"`
		Submatches []struct {
			Start int `json:"start"`
		} `json:"submatches"`
	} `json:"data"`
}
