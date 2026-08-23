package tools

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
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
	if HasRipgrep() {
		return NewRipgrepTool(workingDir, cfg)
	}
	return NewGrepTool(workingDir, cfg)
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

	return fantasy.NewParallelAgentTool(
		RipgrepToolName,
		ripgrepDescription(),
		func(ctx context.Context, params RipgrepParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Pattern == "" {
				return invalidParam("pattern"), nil
			}

			searchPattern := params.Pattern
			if params.LiteralText {
				searchPattern = escapeRegexPattern(params.Pattern)
			}

			// A relative path is relative to the workspace, the same as
			// for every file tool. cmp.Or left it raw, so it resolved
			// against the process cwd — in a thread, the main checkout
			// rather than the worktree the agent is working in.
			searchPath := filepathext.SmartJoin(workingDir, params.Path)

			searchCtx, cancel := context.WithTimeout(ctx, cfg.GetTimeout())
			defer cancel()

			matches, err := searchWithRipgrepCommand(searchCtx, searchPattern, searchPath, params.Include, params.CaseInsensitive, toolOptions.command)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("error searching files: %v", err)), nil
			}
			matches, truncated := sortAndTruncateMatches(matches, 100)

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(renderGrepMatches(matches, truncated)),
				GrepResponseMetadata{
					NumberOfMatches: len(matches),
					Truncated:       truncated,
				},
			), nil
		},
	)
}

func searchWithRipgrep(ctx context.Context, pattern, path, include string, caseInsensitive bool) ([]grepMatch, error) {
	return searchWithRipgrepCommand(ctx, pattern, path, include, caseInsensitive, getRgSearchCmd)
}

func searchWithRipgrepCommand(ctx context.Context, pattern, path, include string, caseInsensitive bool, command func(context.Context, string, string, string, bool) *exec.Cmd) ([]grepMatch, error) {
	cmd := command(ctx, pattern, path, include, caseInsensitive)
	if cmd == nil {
		return nil, fmt.Errorf("ripgrep not found in $PATH")
	}

	// Only add ignore files if they exist
	for _, ignoreFile := range []string{".gitignore", brand.IgnoreFile} {
		ignorePath := filepath.Join(path, ignoreFile)
		if _, err := os.Stat(ignorePath); err == nil {
			cmd.Args = append(cmd.Args, "--ignore-file", ignorePath)
		}
	}

	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return []grepMatch{}, nil
		}
		return nil, err
	}

	var matches []grepMatch
	for line := range bytes.SplitSeq(bytes.TrimSpace(output), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var match ripgrepMatch
		if err := json.Unmarshal(line, &match); err != nil {
			continue
		}
		if match.Type != "match" {
			continue
		}
		for _, m := range match.Data.Submatches {
			fi, err := os.Stat(match.Data.Path.Text)
			if err != nil {
				continue // Skip files we can't access
			}
			matches = append(matches, grepMatch{
				path:     match.Data.Path.Text,
				modTime:  fi.ModTime(),
				lineNum:  match.Data.LineNumber,
				charNum:  m.Start + 1, // ensure 1-based
				lineText: strings.TrimSpace(match.Data.Lines.Text),
			})
			// only get the first match of each line
			break
		}
	}
	return matches, nil
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
