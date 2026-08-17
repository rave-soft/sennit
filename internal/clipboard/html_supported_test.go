//go:build (darwin || linux || windows || freebsd || openbsd || netbsd) && !ios && !android

package clipboard

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeHTMLPayloadUnwrapsAppleScriptHex(t *testing.T) {
	t.Parallel()

	markup := "<html><img src=\"a.png\"></html>"
	out := []byte("«data HTML " + hex.EncodeToString([]byte(markup)) + "»\n")

	require.Equal(t, markup, string(decodeHTMLPayload(out)))
}

func TestDecodeHTMLPayloadPassesPlainMarkupThrough(t *testing.T) {
	t.Parallel()

	markup := []byte("<html><img src=\"a.png\"></html>")

	require.Equal(t, markup, decodeHTMLPayload(markup))
}

func TestDecodeHTMLPayloadKeepsUndecodableHexAsIs(t *testing.T) {
	t.Parallel()

	out := []byte("«data HTML not-hex»")

	require.Equal(t, out, decodeHTMLPayload(out))
}

func TestMissingHTMLHelpersNamesCandidatesWhenNoneAreInstalled(t *testing.T) {
	t.Setenv("PATH", "")

	missing := missingHTMLHelpers()

	require.NotEmpty(t, missing, "with an empty PATH no helper can be found")
	for _, name := range missing {
		require.NotEmpty(t, name)
	}
}

func TestMissingHTMLHelpersIsSilentWhenOneIsInstalled(t *testing.T) {
	dir := t.TempDir()
	helper := htmlReadCommands()[0][0]
	if runtime.GOOS == "windows" {
		// LookPath only considers PATHEXT extensions there.
		helper += ".exe"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, helper), []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("PATH", dir)

	require.Empty(t, missingHTMLHelpers())
}
