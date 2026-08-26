package lsp

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestClientStructureGuards pins that Client keeps its component boundaries:
// the runtime, diagnostics, file-sync and request components are distinct
// fields, not re-folded back into Client as raw state. This prevents a
// future refactor from quietly re-aggregating the responsibilities that
// were decomposed in phase 6.1.
func TestClientStructureGuards(t *testing.T) {
	t.Parallel()

	c := newTestClient()
	require.NotNil(t, c.runtime)
	require.NotNil(t, c.diagnostics)
	require.NotNil(t, c.files)
	require.NotNil(t, c.requests)

	rtType := reflect.TypeOf(c.runtime)
	diagType := reflect.TypeOf(c.diagnostics)
	filesType := reflect.TypeOf(c.files)
	reqType := reflect.TypeOf(c.requests)

	// The four components must be four distinct concrete types.
	require.NotEqual(t, rtType, diagType)
	require.NotEqual(t, rtType, filesType)
	require.NotEqual(t, rtType, reqType)
	require.NotEqual(t, diagType, filesType)
	require.NotEqual(t, diagType, reqType)
	require.NotEqual(t, filesType, reqType)

	// Client must not re-own the state that belongs to a component.
	// If a future refactor moves diagnostics, files, or requests state
	// back onto Client, this test fails and forces a deliberate decision.
	// serverState is deliberately allowed: it is the client identity
	// (the published state) and is stored atomically on the façade.
	clientFields := reflect.TypeOf(Client{}).NumField()
	clientFieldNames := make([]string, clientFields)
	for i := range clientFields {
		clientFieldNames[i] = reflect.TypeOf(Client{}).Field(i).Name
	}
	joined := strings.Join(clientFieldNames, ",")
	for _, forbidden := range []string{"diagnosticsMu", "diagnosticEvents", "openFiles", "diagnosticGeneration"} {
		require.NotContains(t, joined, forbidden,
			"Client re-owns %q; that state belongs to a component", forbidden)
	}

	// The server state must be atomic-typed: it is written by the manager
	// and the runtime lifecycle and read by the UI from any goroutine.
	stateField, ok := reflect.TypeOf(Client{}).FieldByName("serverState")
	require.True(t, ok, "Client.serverState must exist")
	require.Contains(t, stateField.Type.String(), "atomic",
		"Client.serverState must be atomic (concurrent read/write)")
}
