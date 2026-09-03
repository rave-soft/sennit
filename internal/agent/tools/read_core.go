package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/skills"
)

// readCore is the single authorization and filesystem-read path shared by read
// and multi_read. Renderers supply a content budget and retain ownership of
// their response envelopes.
type readCoreResult struct {
	content                                                                   string
	offset, totalLines, nextOffset                                            int
	truncated, budgetTruncated, requestedLimitTruncated, image, skill, denied bool
	cursor, errText, filePath                                                 string
	denial                                                                    fantasy.ToolResponse
}
type readCore func(context.Context, ReadParams, fantasy.ToolCall, string, int, bool) (readCoreResult, error)

func newReadCore(permissions permission.Requester, tracker FileTracking, workingDir string, skillsPaths ...string) readCore {
	return func(ctx context.Context, p ReadParams, call fantasy.ToolCall, toolName string, outputBudget int, rejectSkills bool) (readCoreResult, error) {
		if p.FilePath == "" {
			return readCoreResult{errText: "file_path is required"}, nil
		}
		if p.Offset < 0 || p.Limit < 0 || p.Limit > DefaultReadLimit || outputBudget < 0 {
			return readCoreResult{errText: "invalid read parameters"}, nil
		}
		if strings.HasPrefix(p.FilePath, skills.BuiltinPrefix) || strings.HasPrefix(p.FilePath, skills.InheritedPrefix) {
			return readCoreResult{errText: "multi_read does not support skill resources; use read"}, nil
		}
		path := filepathext.SmartJoin(workingDir, p.FilePath)
		abs, resolvedAbs, outside, err := resolveWithinWorkdir(workingDir, path)
		if err != nil {
			return readCoreResult{}, err
		}
		isSkill := isInSkillsPath(abs, skillsPaths)
		if rejectSkills && isSkill {
			return readCoreResult{errText: "multi_read does not support skill files; use read"}, nil
		}
		// A session ID is required unconditionally here, not only when the
		// path is outside workingDir: it is also what the RecordRead /
		// RecordPartialRead calls below track file history against, for
		// every read regardless of where the file lives.
		sessionID := GetSessionFromContext(ctx)
		if sessionID == "" {
			return readCoreResult{}, missingSessionID("accessing files outside working directory")
		}
		if outside && !isSkill {
			resp, denied, err := requireOutsideWorkdirPermission(
				ctx, permissions, call,
				toolName, "read", "Read file outside working directory",
				"accessing files outside working directory",
				abs, resolvedAbs, ReadPermissionsParams(p),
			)
			if err != nil {
				return readCoreResult{}, err
			}
			if denied {
				return readCoreResult{denied: true, denial: resp}, nil
			}
		}
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return readCoreResult{errText: fmt.Sprintf("File not found: %s", path)}, nil
			}
			return readCoreResult{}, fmt.Errorf("error accessing file: %w", err)
		}
		if info.IsDir() {
			return readCoreResult{errText: fmt.Sprintf("Path is a directory, not a file: %s", path)}, nil
		}
		if p.Cursor != "" {
			off, err := parsePageCursor(p.Cursor, "read", path)
			if err != nil {
				return readCoreResult{errText: err.Error()}, nil
			}
			p.Offset = off
		}
		if p.Limit == 0 {
			p.Limit = DefaultReadLimit
			if isSkill {
				p.Limit = 1000000
			}
		}
		if image, _ := getImageMimeType(path); image {
			return readCoreResult{image: true, filePath: path}, nil
		}
		maxSize := outputBudget
		if isSkill {
			maxSize = 0
		}
		content, more, lines, err := readTextFileCount(path, p.Offset, p.Limit, maxSize)
		if err != nil {
			return readCoreResult{}, fmt.Errorf("error reading file: %w", err)
		}
		if !utf8.ValidString(content) {
			return readCoreResult{errText: "File content is not valid UTF-8"}, nil
		}
		total, err := countFileLines(path)
		if err != nil {
			return readCoreResult{}, fmt.Errorf("counting file lines: %w", err)
		}
		result := readCoreResult{
			content: content, offset: p.Offset, totalLines: total, truncated: more, skill: isSkill, filePath: path,
			budgetTruncated: more && lines < p.Limit, requestedLimitTruncated: more && lines == p.Limit,
		}
		if more {
			result.nextOffset = p.Offset + lines
			result.cursor, err = makePageCursor("read", path, result.nextOffset)
			if err != nil {
				return readCoreResult{}, fmt.Errorf("creating read cursor: %w", err)
			}
		}
		if lines > 0 {
			if p.Offset == 0 && !more {
				tracker.RecordRead(ctx, sessionID, path)
			} else {
				tracker.RecordPartialRead(ctx, sessionID, path, p.Offset+1, p.Offset+lines)
			}
		}
		return result, nil
	}
}
