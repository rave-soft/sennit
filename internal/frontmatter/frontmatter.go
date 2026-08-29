// Package frontmatter splits the YAML header off a markdown file.
//
// It is its own package because three unrelated things read the same file
// shape — skills (SKILL.md), agent markdown, and `sennit import` reading a
// foreign tool's files — and the awkward cases have to be answered the same
// way for all of them: a UTF-8 BOM, CRLF line endings, blank lines before
// the opening fence, an unclosed block. It used to live in internal/skills
// and be exported for config's sake, which made a leaf-shaped parser look
// like part of the skills subsystem and gave internal/config a dependency
// on it for nothing else.
package frontmatter

import (
	"errors"
	"slices"
	"strings"
)

// Split extracts YAML frontmatter and body from markdown content.
func Split(content string) (frontmatter, body string, err error) {
	// Strip UTF-8 BOM for compatibility with editors that include it.
	content = strings.TrimPrefix(content, "\uFEFF")
	// Normalize line endings to \n for consistent parsing.
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	lines := strings.Split(content, "\n")
	start := slices.IndexFunc(lines, func(line string) bool {
		return strings.TrimSpace(line) != ""
	})
	if start == -1 || strings.TrimSpace(lines[start]) != "---" {
		return "", "", errors.New("no YAML frontmatter found")
	}

	endOffset := slices.IndexFunc(lines[start+1:], func(line string) bool {
		return strings.TrimSpace(line) == "---"
	})
	if endOffset == -1 {
		return "", "", errors.New("unclosed frontmatter")
	}
	end := start + 1 + endOffset

	frontmatter = strings.Join(lines[start+1:end], "\n")
	body = strings.Join(lines[end+1:], "\n")
	return frontmatter, body, nil
}
