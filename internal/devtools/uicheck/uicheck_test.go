// Package uicheck holds repository-wide checks that need type information
// and therefore cannot live in a shell script or a golangci-lint pattern.
//
// It is a test rather than a separate binary so it runs wherever tests
// run — locally, in CI, in a bisect — with nothing to wire up and nothing
// to remember.
package uicheck_test

import (
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// terminalPackages are the packages whose output a person reads in a
// terminal. They are the ones where a byte-index slice turns into a
// visible defect.
var terminalPackages = []string{
	"github.com/rave-soft/sennit/internal/ui/...",
	"github.com/rave-soft/sennit/internal/cmd/...",
}

// optOut marks a slice that is deliberately byte-oriented — an ASCII hash,
// a protocol field — rather than text a person reads.
const optOut = "// ok: ascii"

// TestNoByteSlicingOfDisplayText fails on `s[:60]`-style slicing of a
// string in terminal-facing code.
//
// A terminal measures in cells, not bytes. A constant byte bound cuts a
// multi-byte rune in half — Cyrillic, CJK, emoji all become "�" — and
// it ignores ANSI escape sequences, so any width computed that way is
// wrong for styled text. One review found five separate instances of this
// in a single pass (cmd/threads, chat/question, tools/fetch, common/button,
// common/elements), which is what motivated a mechanical check instead of
// catching them one report at a time.
//
// Use ansi.Truncate/ansi.TruncateLeft to cut, ansi.StringWidth or
// lipgloss.Width to measure, utf8.RuneCountInString to count characters.
// If a slice really is byte-oriented, put "// ok: ascii" on the line and
// say why.
//
// Slices of anything other than a string are ignored (`items[:3]` is not a
// text bug), and so are bounds that are not constants, since those are
// usually already the result of a width-aware computation.
func TestNoByteSlicingOfDisplayText(t *testing.T) {
	t.Parallel()

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes |
			packages.NeedTypesInfo | packages.NeedCompiledGoFiles,
		Dir:   "../../..",
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, terminalPackages...)
	if err != nil {
		t.Fatalf("loading packages: %v", err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("packages failed to load; see errors above")
	}
	if len(pkgs) == 0 {
		t.Fatal("no packages matched; the patterns above are stale")
	}

	var findings []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				slice, ok := n.(*ast.SliceExpr)
				if !ok {
					return true
				}
				if !isString(pkg.TypesInfo, slice.X) {
					return true
				}
				if !isConstBound(slice.Low) || !isConstBound(slice.High) {
					return true
				}
				pos := pkg.Fset.Position(slice.Pos())
				if hasOptOut(pkg, pos, optOut) {
					return true
				}
				findings = append(findings, pos.String())
				return true
			})
		}
	}

	if len(findings) > 0 {
		t.Errorf("byte-index slicing of display text in %d place(s):\n  %s\n\n"+
			"Use ansi.Truncate / ansi.StringWidth / utf8.RuneCountInString, or mark a\n"+
			"genuinely byte-oriented slice with %q and say why.",
			len(findings), strings.Join(findings, "\n  "), optOut)
	}
}

// isString reports whether e is of type string (or a named type whose
// underlying type is string).
func isString(info *types.Info, e ast.Expr) bool {
	tv, ok := info.Types[e]
	if !ok || tv.Type == nil {
		return false
	}
	basic, ok := tv.Type.Underlying().(*types.Basic)
	return ok && basic.Info()&types.IsString != 0
}

// isConstBound reports whether a slice bound is an integer literal. A
// missing bound (s[:n], s[n:]) counts, since the other half is what makes
// the slice constant-bounded.
func isConstBound(e ast.Expr) bool {
	if e == nil {
		return true
	}
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return false
	}
	_, err := strconv.Atoi(lit.Value)
	return err == nil
}

// hasOptOut reports whether the source line at pos carries marker. The
// marker is a property of the line a reader sees, which is why this
// reads the file rather than the AST's comment map: a comment map would
// attach the comment to whichever node the parser decided owns it.
func hasOptOut(pkg *packages.Package, pos token.Position, marker string) bool {
	for _, file := range pkg.CompiledGoFiles {
		if file != pos.Filename {
			continue
		}
		lines := readLines(file)
		if pos.Line-1 < len(lines) {
			return strings.Contains(lines[pos.Line-1], marker)
		}
	}
	return false
}

// readLines returns the file's lines, or nil if it cannot be read — an
// unreadable file simply means no opt-out, so the finding stands.
func readLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(string(data), "\n")
}
