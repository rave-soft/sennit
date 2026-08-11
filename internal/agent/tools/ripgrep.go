package tools

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/config"
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

func NewRipgrepTool(workingDir string, cfg config.ToolGrep) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		RipgrepToolName,
		ripgrepDescription(),
		func(ctx context.Context, params RipgrepParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Pattern == "" {
				return fantasy.NewTextErrorResponse("pattern is required"), nil
			}

			searchPattern := params.Pattern
			if params.LiteralText {
				searchPattern = escapeRegexPattern(params.Pattern)
			}

			searchPath := cmp.Or(params.Path, workingDir)

			searchCtx, cancel := context.WithTimeout(ctx, cfg.GetTimeout())
			defer cancel()

			matches, err := searchWithRipgrep(searchCtx, searchPattern, searchPath, params.Include, params.CaseInsensitive)
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
	cmd := getRgSearchCmd(ctx, pattern, path, include, caseInsensitive)
	if cmd == nil {
		return nil, fmt.Errorf("ripgrep not found in $PATH")
	}

	// Only add ignore files if they exist
	for _, ignoreFile := range []string{".gitignore", ".braidignore"} {
		ignorePath := filepath.Join(path, ignoreFile)
		if _, err := os.Stat(ignorePath); err == nil {
			cmd.Args = append(cmd.Args, "--ignore-file", ignorePath)
		}
	}

	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
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
