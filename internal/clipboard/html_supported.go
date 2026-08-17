//go:build (darwin || linux || windows || freebsd || openbsd || netbsd) && !ios && !android

package clipboard

import (
	"bytes"
	"context"
	"encoding/hex"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// htmlReadTimeout bounds the helper process that reads the markup flavor.
const htmlReadTimeout = 2 * time.Second

// readHTML returns the text/html clipboard flavor, or ErrEmpty when no
// helper is installed or the clipboard holds no markup.
func readHTML() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), htmlReadTimeout)
	defer cancel()

	for _, argv := range htmlReadCommands() {
		if _, err := exec.LookPath(argv[0]); err != nil {
			continue
		}
		out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
		if err != nil || len(bytes.TrimSpace(out)) == 0 {
			continue
		}
		return decodeHTMLPayload(out), nil
	}
	return nil, ErrEmpty
}

// missingHTMLHelpers reports the helpers that would give Sennit the markup
// flavor, when none of them is installed.
func missingHTMLHelpers() []string {
	var candidates []string
	for _, argv := range htmlReadCommands() {
		if _, err := exec.LookPath(argv[0]); err == nil {
			return nil
		}
		candidates = append(candidates, argv[0])
	}
	return candidates
}

// htmlReadCommands lists the helpers to try, in order, for this platform.
func htmlReadCommands() [][]string {
	switch runtime.GOOS {
	case "darwin":
		return [][]string{{"osascript", "-e", "the clipboard as «class HTML»"}}
	case "windows":
		return [][]string{{
			"powershell", "-NoProfile", "-NonInteractive",
			"-Command", "Get-Clipboard -TextFormatType Html",
		}}
	default:
		return [][]string{
			{"wl-paste", "--no-newline", "--type", "text/html"},
			{"xclip", "-selection", "clipboard", "-t", "text/html", "-o"},
		}
	}
}

// decodeHTMLPayload unwraps AppleScript's «data HTML 3c68…» hex form. Every
// other helper hands back the markup as-is.
func decodeHTMLPayload(out []byte) []byte {
	s := strings.TrimSpace(string(out))
	const prefix = "«data HTML"
	if !strings.HasPrefix(s, prefix) || !strings.HasSuffix(s, "»") {
		return out
	}
	raw := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(s, prefix), "»"))
	decoded, err := hex.DecodeString(raw)
	if err != nil {
		return out
	}
	return decoded
}
