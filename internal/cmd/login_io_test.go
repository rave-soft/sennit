package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// loginIORecorder stands in for the desktop in sign-in tests: it records
// what the flow would have put on the clipboard and opened in the browser,
// and never touches either.
type loginIORecorder struct {
	mu     sync.Mutex
	copied []string
	opened []string
}

func (r *loginIORecorder) io() loginIO {
	return loginIO{
		copyText: func(s string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.copied = append(r.copied, s)
		},
		waitEnter: func() {},
		openURL: func(u string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.opened = append(r.opened, u)
			return nil
		},
	}
}

// recordLoginIO returns a loginIO that reaches neither the browser nor the
// clipboard, for tests that do not inspect what would have been opened.
func recordLoginIO(t *testing.T) loginIO {
	t.Helper()
	return (&loginIORecorder{}).io()
}

func TestLoginCopilot_DeviceFlowOpensVerificationURLThroughLoginIO(t *testing.T) {
	t.Parallel()

	ws := newCopilotLoginFake()
	rec := &loginIORecorder{}
	require.NoError(t, loginCopilot(ws, true, true, rec.io()))
	require.Equal(t, []string{"https://github.com/login/device"}, rec.opened)
	require.Len(t, rec.copied, 1)
}

// TestLoginDesktopEffectsOnlyInDesktopLoginIO fails when a non-test file
// in this package opens a browser, writes the clipboard or waits on stdin
// anywhere but desktopLoginIO. Such a call is reached by the sign-in tests,
// which then open real tabs on the machine running them.
func TestLoginDesktopEffectsOnlyInDesktopLoginIO(t *testing.T) {
	t.Parallel()

	forbidden := map[string]bool{
		"browser.OpenURL":     true,
		"clipboard.WriteText": true,
		"waitEnter":           true,
	}

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		f, err := parser.ParseFile(fset, name, src, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "desktopLoginIO" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var callee string
				switch f := call.Fun.(type) {
				case *ast.Ident:
					callee = f.Name
				case *ast.SelectorExpr:
					if x, ok := f.X.(*ast.Ident); ok {
						callee = x.Name + "." + f.Sel.Name
					}
				}
				if forbidden[callee] {
					t.Errorf("%s: %s calls %s directly; go through loginIO", fset.Position(call.Pos()), fn.Name.Name, callee)
				}
				return true
			})
		}
	}
}
