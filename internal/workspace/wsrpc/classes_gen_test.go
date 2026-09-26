// Package wsrpc_test (external, not wsrpc's own internal test package):
// this file imports genlib, which imports wsrpc itself, and an internal
// ("package wsrpc") test file importing anything that imports wsrpc back
// is a cycle Go's toolchain refuses outright ("import cycle not allowed in
// test") — the external test package is the standard way around that.
package wsrpc_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/rave-soft/sennit/internal/workspace/wsrpc/gen/genlib"
)

// TestGeneratedFilesAreFresh runs the generator in-memory and fails,
// naming the file, if its output differs from what's committed -- so a
// Workspace method added (or reclassified) without re-running `go generate
// ./internal/workspace/wsrpc/...` is caught by a plain `go test` instead of
// only by a separate CI step.
func TestGeneratedFilesAreFresh(t *testing.T) {
	typesSrc, loopbackSrc, err := genlib.Generate()
	if err != nil {
		t.Fatalf("generating: %v", err)
	}
	checkFresh(t, genlib.TypesFileName, typesSrc)
	checkFresh(t, genlib.LoopbackFileName, loopbackSrc)
}

// checkFresh compares want (the generator's own output, always LF -- both
// go/format and gofumpt's library emit "\n" only) against name's on-disk
// bytes, normalizing the latter's line endings first. .gitattributes pins
// these two files to eol=lf, but that only takes effect for the git client
// that does the checkout; normalizing here means a checkout that ends up
// with CRLF anyway (an older clone predating the attribute, a
// core.autocrlf=true config, ...) still gets a real staleness check
// instead of a line-ending false positive.
func checkFresh(t *testing.T, name string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	got = bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(got, want) {
		t.Errorf("%s is stale; run `go generate ./internal/workspace/wsrpc/...`", name)
	}
}
