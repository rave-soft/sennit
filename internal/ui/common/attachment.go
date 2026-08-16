package common

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/rave-soft/sennit/internal/message"
)

// AttachmentFromPath reads the file at path, detects its MIME type, and
// returns it as a message.Attachment. It returns the raw os.ReadFile error
// unwrapped, matching prior call-site behavior; callers decide how to
// report that (some treat a directory or oversized file as a warning
// rather than an error).
func AttachmentFromPath(path string) (message.Attachment, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return message.Attachment{}, err
	}

	return AttachmentFromBytes(path, filepath.Base(path), content), nil
}

// AttachmentFromBytes builds a message.Attachment from in-memory content
// (e.g. pasted or clipboard data), detecting its MIME type the same way as
// AttachmentFromPath.
func AttachmentFromBytes(filePath, fileName string, content []byte) message.Attachment {
	mimeBufferSize := min(512, len(content))
	mimeType := http.DetectContentType(content[:mimeBufferSize])

	return message.Attachment{
		FilePath: filePath,
		FileName: fileName,
		MimeType: mimeType,
		Content:  content,
	}
}
