// Package clipboard provides cross-platform clipboard access behind build-tag
// guards so that unsupported platforms (e.g., Android, iOS) compile without
// requiring CGO or platform-specific dependencies.
package clipboard

import "errors"

// Format identifies the kind of data to read from the clipboard.
type Format int

const (
	// FormatText is plain UTF-8 text.
	FormatText Format = iota
	// FormatImage is binary image data (PNG).
	FormatImage
	// FormatHTML is the text/html clipboard flavor, which browsers and
	// document editors set alongside plain text when the selection carries
	// markup — including the <img> tags of any images it contained.
	FormatHTML
)

var (
	// ErrUnsupported is returned when clipboard access is not available on
	// the current platform.
	ErrUnsupported = errors.New("clipboard operations are not supported on this platform")
	// ErrEmpty is returned when the clipboard holds no data for the
	// requested format.
	ErrEmpty = errors.New("clipboard is empty or holds an unsupported format")
)

// Init initializes the clipboard subsystem. On unsupported platforms it
// returns ErrUnsupported but is otherwise safe to call.
func Init() error {
	return initClipboard()
}

// WriteText writes plain text to the system clipboard. On unsupported
// platforms it is a no-op.
func WriteText(text string) {
	writeText(text)
}

// Read returns the clipboard contents for the given format. It returns
// ErrEmpty when no matching data is present and ErrUnsupported on platforms
// without clipboard support.
func Read(f Format) ([]byte, error) {
	return read(f)
}

// MissingHTMLHelpers names the programs that would let Sennit read
// FormatHTML, when none of them is installed — the one thing that silently
// turns a rich clipboard paste back into a plain one. It returns nil when a
// helper is present, and on platforms with no clipboard support at all,
// where a missing helper is not the interesting part.
func MissingHTMLHelpers() []string {
	return missingHTMLHelpers()
}
