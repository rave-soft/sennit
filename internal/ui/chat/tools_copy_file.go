package chat

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rave-soft/sennit/internal/diff"
	"github.com/rave-soft/sennit/internal/fsext"
	tools "github.com/rave-soft/sennit/internal/proto"
)

// extLangForCopy maps a lowercased file extension to the fenced-code-block
// language used when copying tool results to the clipboard. Shared by
// formatReadResultForCopy and formatWriteResultForCopy so the two don't
// drift out of sync.
var extLangForCopy = map[string]string{
	".go":   "go",
	".js":   "javascript",
	".mjs":  "javascript",
	".ts":   "typescript",
	".py":   "python",
	".rs":   "rust",
	".java": "java",
	".c":    "c",
	".cpp":  "cpp",
	".cc":   "cpp",
	".cxx":  "cpp",
	".sh":   "bash",
	".bash": "bash",
	".json": "json",
	".yaml": "yaml",
	".yml":  "yaml",
	".xml":  "xml",
	".html": "html",
	".css":  "css",
	".md":   "markdown",
}

// langForCopyFile returns the fenced-code-block language for filePath's
// extension, or "" if it isn't recognized.
func langForCopyFile(filePath string) string {
	return extLangForCopy[strings.ToLower(filepath.Ext(filePath))]
}

// formatReadResultForCopy formats view tool results for clipboard.
func (t *baseToolMessageItem) formatReadResultForCopy() string {
	if t.result == nil {
		return ""
	}

	var meta tools.ReadResponseMetadata
	if t.result.Metadata != "" {
		if err := json.Unmarshal([]byte(t.result.Metadata), &meta); err != nil {
			return t.result.Content
		}
	}

	if meta.Content == "" {
		return t.result.Content
	}

	lang := langForCopyFile(meta.FilePath)

	var result strings.Builder
	if lang != "" {
		fmt.Fprintf(&result, "```%s\n", lang)
	} else {
		result.WriteString("```\n")
	}
	result.WriteString(meta.Content)
	result.WriteString("\n```")

	return result.String()
}

// formatEditResultForCopy formats edit tool results for clipboard.
func (t *baseToolMessageItem) formatEditResultForCopy() string {
	if t.result == nil || t.result.Metadata == "" {
		if t.result != nil {
			return t.result.Content
		}
		return ""
	}

	var meta tools.EditResponseMetadata
	if json.Unmarshal([]byte(t.result.Metadata), &meta) != nil {
		return t.result.Content
	}

	var params tools.EditParams
	if err := json.Unmarshal([]byte(t.toolCall.Input), &params); err != nil {
		// Malformed input JSON is non-fatal here; the diff header just omits the pretty file name.
		params = tools.EditParams{}
	}

	var result strings.Builder

	if meta.OldContent != "" || meta.NewContent != "" {
		fileName := params.FilePath
		if fileName != "" {
			fileName = fsext.PrettyPath(fileName)
		}
		diffContent, additions, removals := diff.GenerateDiff(meta.OldContent, meta.NewContent, fileName)

		fmt.Fprintf(&result, "Changes: +%d -%d\n", additions, removals)
		result.WriteString("```diff\n")
		result.WriteString(diffContent)
		result.WriteString("\n```")
	}

	return result.String()
}

// formatMultiEditResultForCopy formats multi-edit tool results for clipboard.
func (t *baseToolMessageItem) formatMultiEditResultForCopy() string {
	if t.result == nil || t.result.Metadata == "" {
		if t.result != nil {
			return t.result.Content
		}
		return ""
	}

	var meta tools.MultiEditResponseMetadata
	if json.Unmarshal([]byte(t.result.Metadata), &meta) != nil {
		return t.result.Content
	}

	var params tools.MultiEditParams
	if err := json.Unmarshal([]byte(t.toolCall.Input), &params); err != nil {
		// Malformed input JSON is non-fatal here; the diff header just omits the pretty file name.
		params = tools.MultiEditParams{}
	}

	var result strings.Builder
	if meta.OldContent != "" || meta.NewContent != "" {
		fileName := params.FilePath
		if fileName != "" {
			fileName = fsext.PrettyPath(fileName)
		}
		diffContent, additions, removals := diff.GenerateDiff(meta.OldContent, meta.NewContent, fileName)

		fmt.Fprintf(&result, "Changes: +%d -%d\n", additions, removals)
		result.WriteString("```diff\n")
		result.WriteString(diffContent)
		result.WriteString("\n```")
	}

	return result.String()
}

// formatWriteResultForCopy formats write tool results for clipboard.
func (t *baseToolMessageItem) formatWriteResultForCopy() string {
	if t.result == nil {
		return ""
	}

	var params tools.WriteParams
	if json.Unmarshal([]byte(t.toolCall.Input), &params) != nil {
		return t.result.Content
	}

	lang := langForCopyFile(params.FilePath)

	var result strings.Builder
	fmt.Fprintf(&result, "File: %s\n", fsext.PrettyPath(params.FilePath))
	if lang != "" {
		fmt.Fprintf(&result, "```%s\n", lang)
	} else {
		result.WriteString("```\n")
	}
	result.WriteString(params.Content)
	result.WriteString("\n```")

	return result.String()
}
