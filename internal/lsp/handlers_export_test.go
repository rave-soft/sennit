package lsp

import "encoding/json"

// handleDiagnostics delivers a textDocument/publishDiagnostics notification
// that did not arrive through the registered generation handler, forwarding
// it to the current generation's diagnostics path.
//
// Its own doc used to say "tests and manual dispatch", and there is no
// manual dispatch: the server's notifications reach the store through the
// handler registered per generation, which is the path worth testing.
// This exists so a test can deliver one without a server, and lives here so
// the production file does not offer an entry point around the generation
// handler that nothing in production takes.
func handleDiagnostics(client *Client, params json.RawMessage) {
	client.diagnostics.publish(client.runtime.currentGeneration(), params)
}
