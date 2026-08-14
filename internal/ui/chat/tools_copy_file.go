package chat

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rave-soft/braid/internal/diff"
	"github.com/rave-soft/braid/internal/fsext"
	tools "github.com/rave-soft/braid/internal/proto"
)

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

	lang := ""
	if meta.FilePath != "" {
		ext := strings.ToLower(filepath.Ext(meta.FilePath))
		switch ext {
		case ".go":
			lang = "go"
		case ".js", ".mjs":
			lang = "javascript"
		case ".ts":
			lang = "typescript"
		case ".py":
			lang = "python"
		case ".rs":
			lang = "rust"
		case ".java":
			lang = "java"
		case ".c":
			lang = "c"
		case ".cpp", ".cc", ".cxx":
			lang = "cpp"
		case ".sh", ".bash":
			lang = "bash"
		case ".json":
			lang = "json"
		case ".yaml", ".yml":
			lang = "yaml"
		case ".xml":
			lang = "xml"
		case ".html":
			lang = "html"
		case ".css":
			lang = "css"
		case ".md":
			lang = "markdown"
		}
	}

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

	lang := ""
	if params.FilePath != "" {
		ext := strings.ToLower(filepath.Ext(params.FilePath))
		switch ext {
		case ".go":
			lang = "go"
		case ".js", ".mjs":
			lang = "javascript"
		case ".ts":
			lang = "typescript"
		case ".py":
			lang = "python"
		case ".rs":
			lang = "rust"
		case ".java":
			lang = "java"
		case ".c":
			lang = "c"
		case ".cpp", ".cc", ".cxx":
			lang = "cpp"
		case ".sh", ".bash":
			lang = "bash"
		case ".json":
			lang = "json"
		case ".yaml", ".yml":
			lang = "yaml"
		case ".xml":
			lang = "xml"
		case ".html":
			lang = "html"
		case ".css":
			lang = "css"
		case ".md":
			lang = "markdown"
		}
	}

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
