package tools

import (
	"bufio"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/filetracker"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/skills"
)

//go:embed read.md.tpl
var readDescriptionTmpl []byte

var readDescriptionTpl = template.Must(
	template.New("readDescription").
		Parse(string(readDescriptionTmpl)),
)

type readDescriptionData struct {
	DefaultReadLimit int
	MaxReadSizeKB    int
}

func readDescription() string {
	return renderTemplate(readDescriptionTpl, readDescriptionData{
		DefaultReadLimit: DefaultReadLimit,
		MaxReadSizeKB:    MaxReadSize / 1024,
	})
}

type ReadParams struct {
	FilePath string `json:"file_path" description:"The path to the file to read"`
	Offset   int    `json:"offset,omitempty" description:"The line number to start reading from (0-based)"`
	Limit    int    `json:"limit,omitempty" description:"The number of lines to read (defaults to 2000; maximum 2000)"`
	Cursor   string `json:"cursor,omitempty" description:"Stable continuation token returned by a previous read"`
}

// ReadPermissionsParams is defined in proto; see the comment on
// BashPermissionsParams in bash.go.
type ReadPermissionsParams = proto.ReadPermissionsParams

type ReadResourceType string

const (
	ReadResourceUnset ReadResourceType = ""
	ReadResourceSkill ReadResourceType = "skill"
)

type ReadResponseMetadata struct {
	FilePath            string           `json:"file_path"`
	Content             string           `json:"content"`
	TotalLines          int              `json:"total_lines"`
	NextOffset          int              `json:"next_offset,omitempty"`
	Truncated           bool             `json:"truncated"`
	Cursor              string           `json:"cursor,omitempty"`
	ResourceType        ReadResourceType `json:"resource_type,omitempty"`
	ResourceName        string           `json:"resource_name,omitempty"`
	ResourceDescription string           `json:"resource_description,omitempty"`
}

const (
	// ReadToolName and LegacyReadToolName are defined in proto; see the
	// comment on BashPermissionsParams in bash.go. LegacyReadToolName is
	// what this tool was called before it took the name the UI had
	// always shown for it ("Read"), which is also the name every other
	// agent in the ecosystem uses. Sessions recorded before the rename
	// still hold tool calls under the old name and user configs still
	// list it, so the history renderers and the config loader both keep
	// accepting it.
	ReadToolName       = proto.ReadToolName
	LegacyReadToolName = proto.LegacyReadToolName
	MaxReadSize        = 200 * 1024 // 200KB
	// DefaultReadLimit matches Claude Code's Read default; 200 (Sennit's
	// old default) made models page through files in tiny chunks, wasting
	// steps. MaxReadSize and MaxLineLength still bound the worst case.
	DefaultReadLimit = 2000
	MaxLineLength    = 2000
)

func NewReadTool(lspManager *lsp.Manager, permissions permission.Requester, tracker filetracker.Service, skillTracker *skills.Tracker, workingDir string, skillsPaths ...string) fantasy.AgentTool {
	core := newReadCore(lspManager, permissions, tracker, skillTracker, workingDir, skillsPaths...)
	tool := fantasy.NewAgentTool(ReadToolName, readDescription(), func(ctx context.Context, params ReadParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		// Scheme-backed skills retain their public-read behaviour; batches reject
		// them because their in-memory source has no filesystem cursor identity.
		if strings.HasPrefix(params.FilePath, skills.BuiltinPrefix) {
			return readBuiltinFile(params, skillTracker), nil
		}
		if strings.HasPrefix(params.FilePath, skills.InheritedPrefix) {
			source, ok := skillTracker.InheritedSource(params.FilePath)
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Inherited skill not found: %s", params.FilePath)), nil
			}
			return readSkillSource(params, []byte(source), skillTracker), nil
		}
		result, err := core(ctx, params, call, ReadToolName, MaxReadSize, false)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		if result.denied {
			return result.denial, nil
		}
		if result.errText != "" {
			return fantasy.NewTextErrorResponse(result.errText), nil
		}
		if result.image {
			info, err := os.Stat(result.filePath)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("error accessing image file: %w", err)
			}
			if info.Size() > MaxReadSize {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Image file is too large (%d bytes). Maximum size is %d bytes", info.Size(), MaxReadSize)), nil
			}
			data, err := os.ReadFile(result.filePath)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("error reading image file: %w", err)
			}
			if len(data) > MaxReadSize {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Image file is too large (%d bytes). Maximum size is %d bytes", len(data), MaxReadSize)), nil
			}
			if !GetSupportsImagesFromContext(ctx) {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("This model (%s) does not support image data.", GetModelNameFromContext(ctx))), nil
			}
			_, mime := getImageMimeType(result.filePath)
			return fantasy.NewImageResponse(data, sniffImageMimeType(data, mime)), nil
		}
		openInLSPs(ctx, lspManager, result.filePath)
		waitForLSPDiagnostics(ctx, lspManager, result.filePath, 300*time.Millisecond)
		output := "<file>\n" + addLineNumbers(result.content, result.offset+1)
		if result.truncated {
			output += fmt.Sprintf("\n\n(File has more lines. Use 'offset' parameter to read beyond line %d)", result.nextOffset)
		}
		output += "\n</file>\n" + getDiagnostics(result.filePath, lspManager)
		meta := ReadResponseMetadata{FilePath: result.filePath, Content: result.content, TotalLines: result.totalLines, NextOffset: result.nextOffset, Truncated: result.truncated, Cursor: result.cursor}
		if result.skill {
			if skill, err := skills.Parse(result.filePath); err == nil {
				meta.ResourceType, meta.ResourceName, meta.ResourceDescription = ReadResourceSkill, skill.Name, skill.Description
				skillTracker.MarkLoaded(skill.Name)
			}
		}
		return fantasy.WithResponseMetadata(fantasy.NewTextResponse(output), meta), nil
	})
	return withToolParameterSchema(tool, map[string]toolParameterSchema{"file_path": {minLength: intPtr(1)}, "offset": intSchemaMinimum(0), "limit": intSchemaBounds(DefaultReadLimit), "cursor": {minLength: intPtr(1)}})
}

func addLineNumbers(content string, startLine int) string {
	if content == "" {
		return ""
	}

	lines := strings.Split(content, "\n")

	var result []string
	for i, line := range lines {
		line = strings.TrimSuffix(line, "\r")

		lineNum := i + startLine
		numStr := fmt.Sprintf("%d", lineNum)

		if len(numStr) >= 6 {
			result = append(result, fmt.Sprintf("%s|%s", numStr, line))
		} else {
			paddedNum := fmt.Sprintf("%6s", numStr)
			result = append(result, fmt.Sprintf("%s|%s", paddedNum, line))
		}
	}

	return strings.Join(result, "\n")
}

// countFileLines counts physical lines without Scanner's token-size limit.
// A non-empty final line is a line even when it lacks a trailing newline.
func countFileLines(filePath string) (int, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	lines := 0
	for {
		line, err := reader.ReadString('\n')
		if err == nil {
			lines++
			continue
		}
		if err == io.EOF {
			if len(line) > 0 {
				lines++
			}
			return lines, nil
		}
		return 0, err
	}
}

func readTextFileCount(filePath string, offset, limit, maxContentSize int) (string, bool, int, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", false, 0, err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	skipped := 0
	for skipped < offset {
		_, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return "", false, 0, nil
			}
			return "", false, 0, err
		}
		skipped++
	}

	lines := make([]string, 0, min(limit, DefaultReadLimit))
	contentSize := 0

	for len(lines) < limit {
		lineText, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", false, 0, err
		}
		lineText = strings.TrimSuffix(lineText, "\n")
		lineText = strings.TrimSuffix(lineText, "\r")
		if len(lineText) > MaxLineLength {
			// Truncate at a rune boundary to avoid splitting
			// multi-byte characters.
			lineText = strings.ToValidUTF8(lineText[:MaxLineLength], "") + "..."
		}
		projectedSize := contentSize + len(lineText)
		if len(lines) > 0 {
			projectedSize++
		}
		if maxContentSize > 0 && projectedSize > maxContentSize {
			// Stop at the size cap instead of failing the whole read:
			// everything gathered so far is returned, and hasMore=true
			// tells the caller to advertise offset-based continuation.
			return strings.Join(lines, "\n"), true, len(lines), nil
		}
		contentSize = projectedSize
		lines = append(lines, lineText)
		if err == io.EOF {
			break
		}
	}

	// Peek one more line only when we filled the limit.
	hasMore := false
	if len(lines) == limit {
		lineText, peekErr := reader.ReadString('\n')
		hasMore = len(lineText) > 0 || peekErr == nil
	}

	return strings.Join(lines, "\n"), hasMore, len(lines), nil
}

func getImageMimeType(filePath string) (bool, string) {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".jpg", ".jpeg":
		return true, "image/jpeg"
	case ".png":
		return true, "image/png"
	case ".gif":
		return true, "image/gif"
	case ".webp":
		return true, "image/webp"
	default:
		return false, ""
	}
}

// sniffImageMimeType returns the content-sniffed MIME type when it identifies
// a supported image format. Otherwise it returns the provided fallback, which
// is usually the extension-derived type. Providers that validate the image
// media type against the base64 magic bytes (e.g. Anthropic) reject mismatched
// requests with a 400, so trusting the filename alone is unsafe.
func sniffImageMimeType(data []byte, fallback string) string {
	sniffed := http.DetectContentType(data)
	// http.DetectContentType may return the MIME with a ";" parameter
	// (e.g. "image/svg+xml; charset=utf-8") although current image sniffers
	// return bare types; strip defensively.
	if i := strings.IndexByte(sniffed, ';'); i >= 0 {
		sniffed = strings.TrimSpace(sniffed[:i])
	}
	switch sniffed {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return sniffed
	}
	return fallback
}

// isInSkillsPath checks if filePath is within any of the configured skills
// directories. Returns true for files that can be read without permission
// prompts and without size limits.
//
// Note that symlinks are resolved to prevent path traversal attacks via
// symbolic links.
func isInSkillsPath(filePath string, skillsPaths []string) bool {
	if len(skillsPaths) == 0 {
		return false
	}

	absFilePath, err := filepath.Abs(filePath)
	if err != nil {
		return false
	}

	evalFilePath, err := filepath.EvalSymlinks(absFilePath)
	if err != nil {
		return false
	}

	for _, skillsPath := range skillsPaths {
		absSkillsPath, err := filepath.Abs(skillsPath)
		if err != nil {
			continue
		}

		evalSkillsPath, err := filepath.EvalSymlinks(absSkillsPath)
		if err != nil {
			continue
		}

		relPath, err := filepath.Rel(evalSkillsPath, evalFilePath)
		if err == nil && !strings.HasPrefix(relPath, "..") {
			return true
		}
	}

	return false
}

// readBuiltinFile reads a file from the embedded builtin skills filesystem.
func readBuiltinFile(params ReadParams, skillTracker *skills.Tracker) fantasy.ToolResponse {
	embeddedPath := "builtin/" + strings.TrimPrefix(params.FilePath, skills.BuiltinPrefix)
	builtinFS := skills.BuiltinFS()

	data, err := fs.ReadFile(builtinFS, embeddedPath)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Builtin file not found: %s", params.FilePath))
	}

	return readSkillSource(params, data, skillTracker)
}

// readSkillSource renders a skill whose text this process holds rather
// than reads: a builtin from the embedded FS, or one inherited from a
// parent workspace. Both are addressed by a scheme prefix instead of a
// filesystem path, so neither can go through the normal read path.
func readSkillSource(params ReadParams, data []byte, skillTracker *skills.Tracker) fantasy.ToolResponse {
	content := string(data)
	if !utf8.ValidString(content) {
		return fantasy.NewTextErrorResponse("File content is not valid UTF-8")
	}

	limit := params.Limit
	if limit <= 0 {
		limit = 1000000 // Effectively no limit for skill files.
	}

	lines := strings.Split(content, "\n")
	offset := min(params.Offset, len(lines))
	lines = lines[offset:]

	hasMore := len(lines) > limit
	if hasMore {
		lines = lines[:limit]
	}

	output := "<file>\n"
	output += addLineNumbers(strings.Join(lines, "\n"), offset+1)
	if hasMore {
		output += fmt.Sprintf("\n\n(File has more lines. Use 'offset' parameter to read beyond line %d)",
			offset+len(lines))
	}
	output += "\n</file>\n"

	meta := ReadResponseMetadata{
		FilePath: params.FilePath,
		Content:  strings.Join(lines, "\n"),
	}
	if skill, err := skills.ParseContent(data); err == nil {
		meta.ResourceType = ReadResourceSkill
		meta.ResourceName = skill.Name
		meta.ResourceDescription = skill.Description
		skillTracker.MarkLoaded(skill.Name)
	}

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(output),
		meta,
	)
}
