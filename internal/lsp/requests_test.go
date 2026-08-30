package lsp

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRequests_HoverCallsEnsureOpen pins that Hover opens the file on
// demand before dispatching, matching every other request in this file
// (FindReferences, Rename, DocumentSymbols, Definition,
// PrepareCallHierarchy). Hover used to skip both ensureOpen and the
// request timeout, so servers that only answer for open documents
// returned null. Because ensureOpen's error must short-circuit before
// gen() is ever called, this can be verified without a real LSP server:
// gen() panics the test if reached.
func TestRequests_HoverCallsEnsureOpen(t *testing.T) {
	t.Parallel()

	var ensureOpenCalled bool
	wantErr := errors.New("boom")

	q := newRequests(
		func() *clientGeneration {
			t.Fatal("gen() must not be called when ensureOpen fails")
			return nil
		},
		func(ctx context.Context, filepath string) error {
			ensureOpenCalled = true
			require.Equal(t, "main.go", filepath)
			return wantErr
		},
	)

	_, err := q.Hover(t.Context(), "main.go", 1, 1)
	require.True(t, ensureOpenCalled, "Hover must call ensureOpen before dispatching the request")
	require.ErrorIs(t, err, wantErr)
}
