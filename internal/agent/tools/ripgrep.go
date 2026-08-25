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
)

type RipgrepParams struct {
	Pattern         string `json:"pattern" description:"The regex pattern (Rust regex syntax) to search for in file contents"`
	Path            string `json:"path,omitempty" description:"The directory to search in. Defaults to the current working directory."`
	Include         string `json:"include,omitempty" description:"Glob pattern for files to include in the search (e.g. \"*.js\", \"*.{ts,tsx}\")"`
	LiteralText     bool   `json:"literal_text,omitempty" description:"If true, the pattern will be treated as literal text with special regex characters escaped. Default is false."`
	CaseInsensitive bool   `json:"case_insensitive,omitempty" description:"If true, the search is case-insensitive. Default is false."`
	MaxResults      int    `json:"max_results,omitempty" description:"Maximum results (1-1000, defaults to 100)"`
	BeforeContext   int    `json:"before_context,omitempty" description:"Lines before each match (0-10)"`
	AfterContext    int    `json:"after_context,omitempty" description:"Lines after each match (0-10)"`
	Cursor          string `json:"cursor,omitempty" description:"Stable continuation token"`
	Sort            string `json:"sort,omitempty" description:"Sort by path or mtime" enum:"path,mtime"`
}

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
func NewSearchTool(workingDir string, cfg config.ToolGrep) fantasy.AgentTool {
	return newSearchTool(workingDir, cfg, getRg())
}

func newSearchTool(workingDir string, cfg config.ToolGrep, ripgrepPath string) fantasy.AgentTool {
	if ripgrepPath != "" {
		return NewRipgrepTool(workingDir, cfg, withRipgrepCommand(ripgrepSearchCommand(ripgrepPath)))
	}
	return NewGrepTool(workingDir, cfg)
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

func NewRipgrepTool(workingDir string, cfg config.ToolGrep, options ...ripgrepToolOption) fantasy.AgentTool {
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
			if params.BeforeContext < 0 || params.BeforeContext > 10 || params.AfterContext < 0 || params.AfterContext > 10 {
				return fantasy.NewTextErrorResponse("context must be between 0 and 10 lines"), nil
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
		"before_context": intSchemaBounds(10),
		"after_context":  intSchemaBounds(10),
	})
}

// searchWithRipgrep collects every match for a case-sensitive search. Case
// insensitivity is reached through searchWithRipgrepCommand directly; no
// caller needs both a canned command and a case-insensitive search.
func searchWithRipgrep(ctx context.Context, pattern, path, include string) ([]grepMatch, error) {
	return searchWithRipgrepCommand(ctx, pattern, path, include, false, getRgSearchCmd)
}

func searchWithRipgrepCommand(ctx context.Context, pattern, path, include string, caseInsensitive bool, command func(context.Context, string, string, string, bool) *exec.Cmd) ([]grepMatch, error) {
	var matches []grepMatch
	err := visitRipgrepMatches(ctx, pattern, path, include, caseInsensitive, command, func(match grepMatch) {
		matches = append(matches, match)
	})
	return matches, err
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
