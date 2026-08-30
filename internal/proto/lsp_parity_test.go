package proto_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/proto"
)

// TestLSPStateParityWithDomain holds proto's names for a server's state
// against the runtime's iota. The two cannot share a definition — proto is
// the transport boundary and deliberately does not depend on internal/lsp,
// which is what lets the sidebar render a server's state without importing
// the LSP runtime — so the mapping in appws is the design, and this keeps
// it complete.
//
// Completeness is checked by reading internal/lsp's const block rather
// than by counting the table below, because a constant appended to an iota
// block is invisible at run time: nothing about the seventh state makes
// any expression here change value. It would simply fall into lspState's
// default and render as "unstarted" — a plausible-looking wrong answer
// rather than a failure.
func TestLSPStateParityWithDomain(t *testing.T) {
	t.Parallel()

	mapped := map[lsp.ServerState]proto.LSPState{
		lsp.StateUnstarted: proto.LSPStateUnstarted,
		lsp.StateStarting:  proto.LSPStateStarting,
		lsp.StateReady:     proto.LSPStateReady,
		lsp.StateError:     proto.LSPStateError,
		lsp.StateStopped:   proto.LSPStateStopped,
		lsp.StateDisabled:  proto.LSPStateDisabled,
	}

	declared := serverStateNames(t)
	require.Len(t, mapped, len(declared),
		"internal/lsp declares %v; every one needs a proto.LSPState and a case in appws.lspState", declared)

	seen := map[proto.LSPState]bool{}
	for _, name := range mapped {
		require.NotEmpty(t, name)
		require.False(t, seen[name], "two ServerStates map to %q", name)
		seen[name] = true
	}
}

// serverStateNames returns every constant declared with type ServerState in
// internal/lsp, read from the source.
func serverStateNames(t *testing.T) []string {
	t.Helper()

	files, err := filepath.Glob(filepath.Join("..", "lsp", "*.go"))
	require.NoError(t, err)

	var names []string
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		require.NoError(t, err)

		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			// Only the block that opens with an explicit ServerState type:
			// iota blocks carry the type on the first spec alone.
			typed := false
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if ident, ok := value.Type.(*ast.Ident); ok && ident.Name == "ServerState" {
					typed = true
				}
				if !typed {
					continue
				}
				for _, name := range value.Names {
					names = append(names, name.Name)
				}
			}
		}
	}
	return names
}
