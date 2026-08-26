package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeLSPServerEnv, when set to "1" in the child process environment,
// makes the test binary re-exec itself as a minimal LSP server instead of
// running the test suite. This is the standard self-exec helper-process
// pattern (see GO_WANT_HELPER_PROCESS in the standard library's
// os/exec_test.go): it lets tests spawn a real child process that speaks
// just enough LSP to get through Initialize/WaitForServerReady, without
// depending on an actual language server being installed.
const fakeLSPServerEnv = "SENNIT_LSP_FAKE_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(fakeLSPServerEnv) == "1" {
		runFakeLSPServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runFakeLSPServer answers every request over stdio (with the
// "initialize" response carrying real capabilities, everything else a
// null result) and ignores notifications, until stdin closes. Answering
// "shutdown" too matters here, not just for realism: Client.Close blocks
// a goroutine on the shutdown call and only escapes it via a timeout, and
// a client left blocked there past Close's deadline is a real race
// against whatever creates the next client (e.g. Restart) — tests must
// let shutdown complete cleanly to avoid tripping it under -race.
func fakeWorkspaceSymbolResult() string {
	root := os.Getenv("SENNIT_LSP_FAKE_ROOT")
	if root == "" {
		root = "/workspace"
	}
	uri := "file://" + filepath.ToSlash(filepath.Join(root, "main.go"))
	return `[{"name":"Alpha","kind":12,"location":{"uri":"` + uri + `","range":{"start":{"line":1,"character":0},"end":{"line":1,"character":5}}}},{"name":"Beta","kind":12,"location":{"uri":"` + uri + `"}}]`
}

func runFakeLSPServer() {
	r := bufio.NewReader(os.Stdin)
	initialized := false
	for {
		body, err := readLSPFrame(r)
		if err != nil {
			return
		}
		var envelope struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			continue
		}
		if logPath := os.Getenv("SENNIT_LSP_FAKE_LOG"); logPath != "" {
			file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				_, _ = fmt.Fprintf(file, "%d %s\n", os.Getpid(), envelope.Method)
				_ = file.Close()
			}
		}
		if len(envelope.ID) == 0 {
			if os.Getenv("SENNIT_LSP_FAKE_SCENARIO") == "stop-reading-after-workspace-change" && envelope.Method == "workspace/didChangeWatchedFiles" {
				select {}
			}
			continue // notification, no response expected
		}
		result := "null"
		switch envelope.Method {
		case "initialize":
			// "crash-after-init" answers the handshake normally and only
			// dies once the client sends initialized: that is the
			// WaitForServerReady failure path, which needs a process that
			// is initialized but never usable.
			if os.Getenv("SENNIT_LSP_FAKE_SCENARIO") == "crash-after-init" && initialized {
				os.Exit(1)
			}
			if os.Getenv("SENNIT_LSP_FAKE_SCENARIO") == "bad-init" {
				// A syntactically valid response whose result is not an
				// object: the client's Initialize fails to decode it.
				result = `5`
			} else if os.Getenv("SENNIT_LSP_FAKE_SCENARIO") == "symbols" {
				result = `{"capabilities":{"hoverProvider":true,"workspaceSymbolProvider":true}}`
			} else {
				result = `{"capabilities":{}}`
			}
			initialized = true
		case "workspace/symbol":
			if os.Getenv("SENNIT_LSP_FAKE_SCENARIO") == "symbols" {
				result = fakeWorkspaceSymbolResult()
			}
		case "textDocument/hover":
			if os.Getenv("SENNIT_LSP_FAKE_SCENARIO") == "symbols" {
				result = `{"contents":{"kind":"markdown","value":"` + "`Alpha() string`" + `"}}`
			}
		}
		resp := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, envelope.ID, result))
		writeLSPFrame(os.Stdout, resp)
	}
}

// readLSPFrame reads one Content-Length-framed LSP message, per the
// standard LSP base protocol.
func readLSPFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length: "); ok {
			length, _ = strconv.Atoi(v)
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("missing Content-Length header")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeLSPFrame(w io.Writer, body []byte) {
	fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(body), body)
}
