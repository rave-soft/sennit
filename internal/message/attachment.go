package message

import (
	"slices"
	"strings"
)

type Attachment struct {
	FilePath string
	FileName string
	MimeType string
	Content  []byte
}

// MaxAttachmentSize is the largest file this package's callers will attach
// (5 MB). It lives here, rather than in internal/ui/common where it used
// to be the only copy, so internal/workspace/appws can enforce the same
// ceiling when building an attachment for AttachProjectFile without
// depending on internal/ui.
const MaxAttachmentSize = int64(5 * 1024 * 1024)

func (a Attachment) IsText() bool     { return strings.HasPrefix(a.MimeType, "text/") }
func (a Attachment) IsImage() bool    { return strings.HasPrefix(a.MimeType, "image/") }
func (a Attachment) IsMarkdown() bool { return a.MimeType == "text/markdown" }

// ContainsTextAttachment returns true if any of the attachments is a text attachment.
func ContainsTextAttachment(attachments []Attachment) bool {
	return slices.ContainsFunc(attachments, func(a Attachment) bool {
		return a.IsText()
	})
}
