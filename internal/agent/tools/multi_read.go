package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/filetracker"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/skills"
)

const (
	MultiReadToolName     = "multi_read"
	MaxMultiReadFiles     = 20
	DefaultMultiReadBytes = MaxReadSize
)

type MultiReadItem struct {
	FilePath string `json:"file_path" description:"The path to the file to read"`
	Offset   int    `json:"offset,omitempty" description:"The 0-based line offset"`
	Limit    int    `json:"limit,omitempty" description:"The maximum number of lines"`
	Cursor   string `json:"cursor,omitempty" description:"A continuation cursor from a previous result"`
}
type MultiReadParams struct {
	Files     []MultiReadItem `json:"files" description:"Files and ranges to read"`
	MaxBytes  int             `json:"max_bytes,omitempty" description:"Total rendered response budget in bytes"`
	MaxTokens int             `json:"max_tokens,omitempty" description:"Total rendered response budget using one UTF-8 byte per token"`
	Cursor    string          `json:"cursor,omitempty" description:"Continuation cursor returned by a previous multi_read"`
}
type MultiReadEntry struct {
	FilePath   string `json:"file_path"`
	Status     string `json:"status"`
	TotalLines int    `json:"total_lines,omitempty"`
	NextOffset int    `json:"next_offset,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Cursor     string `json:"cursor,omitempty"`
	Error      string `json:"error,omitempty"`
}
type MultiReadResponse struct {
	Files             []MultiReadEntry `json:"files"`
	Bytes             int              `json:"bytes"`
	NextIndex         int              `json:"next_index,omitempty"`
	CurrentCursor     string           `json:"current_cursor,omitempty"`
	CurrentNextOffset int              `json:"current_next_offset,omitempty"`
	Cursor            string           `json:"cursor,omitempty"`
	Truncated         bool             `json:"truncated"`
}

func NewMultiReadTool(lspManager *lsp.Manager, permissions permission.Service, tracker filetracker.Service, skillTracker *skills.Tracker, workingDir string, skillsPaths ...string) fantasy.AgentTool {
	core := newReadCore(lspManager, permissions, tracker, skillTracker, workingDir, skillsPaths...)
	tool := fantasy.NewAgentTool(MultiReadToolName, "Read multiple file ranges sequentially with one shared rendered-response budget.", func(ctx context.Context, p MultiReadParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		if len(p.Files) == 0 {
			return invalidParam("files"), nil
		}
		if len(p.Files) > MaxMultiReadFiles || p.MaxBytes < 0 || p.MaxTokens < 0 {
			return fantasy.NewTextErrorResponse("invalid multi_read parameters"), nil
		}
		budget := p.MaxBytes
		if budget == 0 {
			budget = DefaultMultiReadBytes
		}
		if p.MaxTokens > 0 && p.MaxTokens < budget {
			// T2 defines one UTF-8 byte per token for tool response budgets.
			budget = p.MaxTokens
		}
		if budget < 1 || budget > MaxReadSize {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("effective budget must be between 1 and %d", MaxReadSize)), nil
		}
		fingerprintInput, _ := json.Marshal(struct {
			Files     []MultiReadItem `json:"files"`
			MaxBytes  int             `json:"max_bytes"`
			MaxTokens int             `json:"max_tokens"`
		}{p.Files, p.MaxBytes, p.MaxTokens})
		fingerprint := fingerprintPage(string(fingerprintInput))
		start := 0
		if p.Cursor != "" {
			c, err := openPageKeyCursor(p.Cursor, MultiReadToolName, fingerprint)
			if err != nil || c.Index < 0 || c.Index >= len(p.Files) {
				return fantasy.NewTextErrorResponse("invalid multi_read cursor"), nil
			}
			start = c.Index
			if c.Path != "" {
				p.Files[start].Cursor = c.Path
			}
		}
		out := MultiReadResponse{Files: make([]MultiReadEntry, 0, len(p.Files)-start)}
		var body strings.Builder
		for i := start; i < len(p.Files); i++ {
			item := p.Files[i]
			okOpen, close := fmt.Sprintf("<file path=%q status=ok>\n", item.FilePath), "\n</file>\n"
			remaining := budget - body.Len() - len(okOpen) - len(close)
			if remaining <= 0 {
				if len(out.Files) == 0 {
					return fantasy.NewTextErrorResponse("budget too small for one line"), nil
				}
				out.Truncated, out.NextIndex = true, i
				break
			}
			result, err := core(ctx, ReadParams(item), call, MultiReadToolName, remaining, true)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if result.denied {
				return result.denial, nil
			}
			entry := MultiReadEntry{FilePath: item.FilePath}
			if result.errText != "" {
				entry.Status, entry.Error = "error", result.errText
				encoded := fmt.Sprintf("<file path=%q status=error>\n%s\n</file>\n", item.FilePath, result.errText)
				if body.Len()+len(encoded) > budget {
					if len(out.Files) == 0 {
						return fantasy.NewTextErrorResponse("budget too small for one error"), nil
					}
					out.Truncated, out.NextIndex = true, i
					break
				}
				body.WriteString(encoded)
				out.Files = append(out.Files, entry)
				continue
			}
			if result.image {
				entry.Status, entry.Error = "unsupported", "multi_read supports text files only; use read for images"
				encoded := fmt.Sprintf("<file path=%q status=unsupported>\n%s\n</file>\n", item.FilePath, entry.Error)
				if body.Len()+len(encoded) > budget {
					if len(out.Files) == 0 {
						return fantasy.NewTextErrorResponse("budget too small for image error"), nil
					}
					out.Truncated, out.NextIndex = true, i
					break
				}
				body.WriteString(encoded)
				out.Files = append(out.Files, entry)
				continue
			}
			if result.content == "" && result.budgetTruncated {
				if len(out.Files) == 0 {
					return fantasy.NewTextErrorResponse("budget too small for one line"), nil
				}
				out.Truncated, out.NextIndex = true, i
				break
			}
			entry.Status, entry.TotalLines, entry.NextOffset, entry.Truncated, entry.Cursor = "ok", result.totalLines, result.nextOffset, result.truncated, result.cursor
			body.WriteString(okOpen)
			body.WriteString(result.content)
			body.WriteString(close)
			out.Files = append(out.Files, entry)
			// A caller-requested line limit is independently resumable, so later
			// files can still run. A shared-budget cut must resume this same item.
			if result.budgetTruncated {
				out.Truncated, out.NextIndex = true, i
				out.CurrentCursor, out.CurrentNextOffset = entry.Cursor, entry.NextOffset
				break
			}
		}
		if out.Truncated {
			c := pageCursor{Version: 2, Kind: MultiReadToolName, Query: fingerprint, Index: out.NextIndex, Path: out.CurrentCursor, Offset: out.CurrentNextOffset}
			out.Cursor, _ = encodePageCursor(c)
		}
		out.Bytes = body.Len()
		return fantasy.WithResponseMetadata(fantasy.NewTextResponse(body.String()), out), nil
	})
	return withToolParameterSchema(tool, map[string]toolParameterSchema{
		"files":                 {minItems: intPtr(1), maxItems: intPtr(MaxMultiReadFiles)},
		"files.items.file_path": {minLength: intPtr(1)}, "files.items.offset": intSchemaMinimum(0), "files.items.limit": intSchemaBounds(DefaultReadLimit), "files.items.cursor": {minLength: intPtr(1)},
		"max_bytes": intSchemaBounds(MaxReadSize), "max_tokens": intSchemaBounds(MaxReadSize), "cursor": {minLength: intPtr(1)},
	})
}
