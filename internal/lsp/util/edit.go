package util

import (
	"fmt"
	"os"
	"sort"
	"strings"

	powernap "github.com/charmbracelet/x/powernap/pkg/lsp"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/rave-soft/sennit/internal/fsext"
)

func applyTextEdits(uri protocol.DocumentURI, edits []protocol.TextEdit, encoding powernap.OffsetEncoding) error {
	path, err := uri.Path()
	if err != nil {
		return fmt.Errorf("invalid URI: %w", err)
	}

	// Read the file content
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	// The LSP server numbers lines by ANY line terminator, so a file with
	// even one stray "\r\n" among otherwise bare "\n" lines must still be
	// split on every terminator, not just the majority one — splitting on
	// "\r\n" alone (the old behavior) left bare-"\n" lines fused together,
	// so a server-reported line number pointed at the wrong line and the
	// edit landed one line off. Normalize to LF for the whole edit, and
	// restore CRLF on write if that's what the file had.
	rawContent := string(content)
	normalized, isCRLF := fsext.ToUnixLineEndings(rawContent)

	// Track if file ends with a newline (checked against the normalized
	// form so a lone trailing "\r" — part of a CRLF pair — isn't mistaken
	// for content).
	endsWithNewline := len(normalized) > 0 && strings.HasSuffix(normalized, "\n")

	// Split into lines without the endings
	lines := strings.Split(normalized, "\n")

	// Check for overlapping edits
	for i, edit1 := range edits {
		for j := i + 1; j < len(edits); j++ {
			if rangesOverlap(edit1.Range, edits[j].Range) {
				return fmt.Errorf("overlapping edits detected between edit %d and %d", i, j)
			}
		}
	}

	// Sort edits in reverse order
	sortedEdits := make([]protocol.TextEdit, len(edits))
	copy(sortedEdits, edits)
	sort.Slice(sortedEdits, func(i, j int) bool {
		if sortedEdits[i].Range.Start.Line != sortedEdits[j].Range.Start.Line {
			return sortedEdits[i].Range.Start.Line > sortedEdits[j].Range.Start.Line
		}
		return sortedEdits[i].Range.Start.Character > sortedEdits[j].Range.Start.Character
	})

	// Apply each edit
	for _, edit := range sortedEdits {
		newLines, err := applyTextEdit(lines, edit, encoding)
		if err != nil {
			return fmt.Errorf("failed to apply edit: %w", err)
		}
		lines = newLines
	}

	// Join lines with LF first; convert to CRLF afterward if the file was
	// CRLF, rather than interleaving line-ending choice with line joining.
	var newContent strings.Builder
	for i, line := range lines {
		if i > 0 {
			newContent.WriteString("\n")
		}
		newContent.WriteString(line)
	}

	// Only add a newline if the original file had one and we haven't already added it
	if endsWithNewline && !strings.HasSuffix(newContent.String(), "\n") {
		newContent.WriteString("\n")
	}

	result := newContent.String()
	if isCRLF {
		result, _ = fsext.ToWindowsLineEndings(result)
	}

	if err := os.WriteFile(path, []byte(result), 0o644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

func applyTextEdit(lines []string, edit protocol.TextEdit, encoding powernap.OffsetEncoding) ([]string, error) {
	startLine := int(edit.Range.Start.Line)
	endLine := int(edit.Range.End.Line)

	// Validate positions before accessing lines. An end line past the last
	// line is refused, exactly like an out-of-range start: clamping it to
	// the last line instead would let a stale edit — one whose file has
	// since lost lines — splice a prefix from the intended start line onto
	// a suffix from an unrelated last line, deleting everything between
	// them and still reporting success.
	//
	// No legitimate "append at EOF" idiom needs endLine >= len(lines):
	// strings.Split leaves a trailing empty entry for a file ending in a
	// newline, and the LSP spec's own end-of-document position lands on the
	// last existing line at character len(lastLine). Both already fall
	// within len(lines); an oversized character offset is clamped below.
	if startLine < 0 || startLine >= len(lines) {
		return nil, fmt.Errorf("invalid start line: %d", startLine)
	}
	if endLine < 0 || endLine >= len(lines) {
		return nil, fmt.Errorf("invalid end line: %d", endLine)
	}

	// startLineContent/endLineContent are the two line slices every branch
	// below needs — the encoding switch to compute byte offsets, and the
	// prefix/suffix extraction after it — so fetch each once and reuse it.
	startLineContent := lines[startLine]
	endLineContent := lines[endLine]

	var startChar, endChar int
	switch encoding {
	case powernap.UTF8:
		// UTF-8: Character offset is already a byte offset
		startChar = int(edit.Range.Start.Character)
		endChar = int(edit.Range.End.Character)
	case powernap.UTF16:
		// UTF-16 (default): Convert to byte offset
		startChar = powernap.PositionToByteOffset(startLineContent, edit.Range.Start.Character)
		endChar = powernap.PositionToByteOffset(endLineContent, edit.Range.End.Character)
	default:
		// UTF-32: Character offset is codepoint count, convert to byte offset
		startChar = utf32ToByteOffset(startLineContent, edit.Range.Start.Character)
		endChar = utf32ToByteOffset(endLineContent, edit.Range.End.Character)
	}

	// Create result slice with initial capacity
	result := make([]string, 0, len(lines))

	// Copy lines before edit
	result = append(result, lines[:startLine]...)

	// Get the prefix of the start line
	if startChar < 0 || startChar > len(startLineContent) {
		startChar = len(startLineContent)
	}
	prefix := startLineContent[:startChar]

	// Get the suffix of the end line
	if endChar < 0 || endChar > len(endLineContent) {
		endChar = len(endLineContent)
	}
	suffix := endLineContent[endChar:]

	// Handle the edit
	if edit.NewText == "" {
		// Always emit the merged line, even when prefix+suffix is empty:
		// dropping it here would delete the line itself instead of leaving
		// an empty one, shifting every subsequent line.
		result = append(result, prefix+suffix)
	} else {
		// A server is free to send NewText with CRLF line endings regardless
		// of what the file on disk uses (e.g. a rename edit built from a
		// CRLF template). lines is always LF-normalized by the caller, so
		// NewText must be too, or a "\r" left dangling in a split segment
		// doubles up against the "\n" this function reintroduces below.
		normalizedNewText, _ := fsext.ToUnixLineEndings(edit.NewText)
		newLines := strings.Split(normalizedNewText, "\n")

		if len(newLines) == 1 {
			// Single line change
			result = append(result, prefix+newLines[0]+suffix)
		} else {
			// Multi-line change
			result = append(result, prefix+newLines[0])
			result = append(result, newLines[1:len(newLines)-1]...)
			result = append(result, newLines[len(newLines)-1]+suffix)
		}
	}

	// Add remaining lines
	if endLine+1 < len(lines) {
		result = append(result, lines[endLine+1:]...)
	}

	return result, nil
}

// applyDocumentChange applies a DocumentChange (create/rename/delete operations)
func applyDocumentChange(change protocol.DocumentChange, encoding powernap.OffsetEncoding) error {
	if change.CreateFile != nil {
		path, err := change.CreateFile.URI.Path()
		if err != nil {
			return fmt.Errorf("invalid URI: %w", err)
		}

		// Per the LSP spec, overwrite wins over ignoreIfExists, and with
		// neither set, creating over an existing file is not intended.
		overwrite := change.CreateFile.Options != nil && change.CreateFile.Options.Overwrite
		ignoreIfExists := change.CreateFile.Options != nil && change.CreateFile.Options.IgnoreIfExists
		if !overwrite {
			if _, err := os.Stat(path); err == nil {
				if ignoreIfExists {
					return nil // File exists and we're ignoring it.
				}
				return fmt.Errorf("target file already exists and overwrite is not allowed: %s", path)
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("failed to stat file: %w", err)
			}
		}
		if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
			return fmt.Errorf("failed to create file: %w", err)
		}
	}

	if change.DeleteFile != nil {
		path, err := change.DeleteFile.URI.Path()
		if err != nil {
			return fmt.Errorf("invalid URI: %w", err)
		}

		ignoreIfNotExists := change.DeleteFile.Options != nil && change.DeleteFile.Options.IgnoreIfNotExists
		if change.DeleteFile.Options != nil && change.DeleteFile.Options.Recursive {
			if _, err := os.Stat(path); err != nil {
				if ignoreIfNotExists && os.IsNotExist(err) {
					return nil
				}
				return fmt.Errorf("failed to stat file: %w", err)
			}
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("failed to delete directory recursively: %w", err)
			}
		} else {
			if err := os.Remove(path); err != nil {
				if ignoreIfNotExists && os.IsNotExist(err) {
					return nil
				}
				return fmt.Errorf("failed to delete file: %w", err)
			}
		}
	}

	if change.RenameFile != nil {
		var newPath, oldPath string
		var err error

		oldPath, err = change.RenameFile.OldURI.Path()
		if err != nil {
			return err
		}

		newPath, err = change.RenameFile.NewURI.Path()
		if err != nil {
			return err
		}

		overwrite := change.RenameFile.Options != nil && change.RenameFile.Options.Overwrite
		ignoreIfExists := change.RenameFile.Options != nil && change.RenameFile.Options.IgnoreIfExists
		if !overwrite {
			if _, err := os.Stat(newPath); err == nil {
				if ignoreIfExists {
					return nil
				}
				return fmt.Errorf("target file already exists and overwrite is not allowed: %s", newPath)
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("failed to stat target file: %w", err)
			}
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			return fmt.Errorf("failed to rename file: %w", err)
		}
	}

	if change.TextDocumentEdit != nil {
		textEdits := make([]protocol.TextEdit, len(change.TextDocumentEdit.Edits))
		for i, edit := range change.TextDocumentEdit.Edits {
			var err error
			textEdits[i], err = edit.AsTextEdit()
			if err != nil {
				return fmt.Errorf("invalid edit type: %w", err)
			}
		}
		return applyTextEdits(change.TextDocumentEdit.TextDocument.URI, textEdits, encoding)
	}

	return nil
}

// utf32ToByteOffset converts a UTF-32 codepoint offset to a byte offset.
func utf32ToByteOffset(lineText string, codepointOffset uint32) int {
	if codepointOffset == 0 {
		return 0
	}

	var codepointCount uint32
	for byteOffset := range lineText {
		if codepointCount >= codepointOffset {
			return byteOffset
		}
		codepointCount++
	}
	return len(lineText)
}

// ApplyWorkspaceEdit applies the given WorkspaceEdit to the filesystem.
// The encoding parameter specifies the position encoding used by the LSP server
// (UTF8, UTF16, or UTF32). This affects how character offsets are interpreted.
func ApplyWorkspaceEdit(edit protocol.WorkspaceEdit, encoding powernap.OffsetEncoding) error {
	// Handle Changes field
	for uri, textEdits := range edit.Changes {
		if err := applyTextEdits(uri, textEdits, encoding); err != nil {
			return fmt.Errorf("failed to apply text edits: %w", err)
		}
	}

	// Handle DocumentChanges field
	for _, change := range edit.DocumentChanges {
		if err := applyDocumentChange(change, encoding); err != nil {
			return fmt.Errorf("failed to apply document change: %w", err)
		}
	}

	return nil
}

// rangesOverlap checks if two LSP ranges overlap.
// Per the LSP specification, ranges are half-open intervals [start, end),
// so adjacent ranges where one's end equals another's start do NOT overlap.
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification/#range
func rangesOverlap(r1, r2 protocol.Range) bool {
	if r1.Start.Line > r2.End.Line || r2.Start.Line > r1.End.Line {
		return false
	}
	if r1.Start.Line == r2.End.Line && r1.Start.Character >= r2.End.Character {
		return false
	}
	if r2.Start.Line == r1.End.Line && r2.Start.Character >= r1.End.Character {
		return false
	}
	return true
}
