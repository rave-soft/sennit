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
	"time"
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
			Params json.RawMessage `json:"params"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			continue
		}
		if logPath := os.Getenv("SENNIT_LSP_FAKE_LOG"); logPath != "" {
			line := fmt.Sprintf("%d %s", os.Getpid(), envelope.Method)
			// textDocument/hover's position is what
			// TestClient_Hover_ConvertsToZeroBasedPosition checks: it pins
			// requests.Hover as the one place that converts the model's
			// 1-based line/character into the LSP wire's 0-based Position,
			// so the log needs to carry what was actually sent, not just
			// which method fired.
			if envelope.Method == "textDocument/hover" {
				var params struct {
					Position struct {
						Line      uint32 `json:"line"`
						Character uint32 `json:"character"`
					} `json:"position"`
				}
				if err := json.Unmarshal(envelope.Params, &params); err == nil {
					line += fmt.Sprintf(" line=%d character=%d", params.Position.Line, params.Position.Character)
				}
			}
			file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				_, _ = fmt.Fprintln(file, line)
				_ = file.Close()
			}
		}
		if len(envelope.ID) == 0 {
			if os.Getenv("SENNIT_LSP_FAKE_SCENARIO") == "stop-reading-after-workspace-change" && envelope.Method == "workspace/didChangeWatchedFiles" {
				// Sleep rather than `select {}`. The scenario needs a
				// process that is alive and no longer reading stdin, and
				// a bare select is neither reliably: it parks the only
				// goroutine this process has, which is the exact
				// condition the runtime's deadlock detector panics on.
				// Whether it fires depends on what else happens to be
				// runnable, so the server sometimes dies here instead of
				// going quiet — and a dead server closes the pipe, which
				// makes the caller's next write return immediately
				// instead of blocking on back-pressure. That is the
				// opposite of the state the caller is trying to reach.
				// A timer keeps the runtime satisfied without giving the
				// process any reason to read again.
				for {
					time.Sleep(time.Hour)
				}
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
			if os.Getenv("SENNIT_LSP_FAKE_SCENARIO") == "request-during-init" {
				// Mirrors a real server (e.g. typescript-language-server)
				// issuing a request of its own before answering
				// "initialize" itself. Send it now, mid-handshake, and
				// read the client's reply before this case falls through
				// to answer "initialize" below.
				writeLSPFrame(os.Stdout, []byte(`{"jsonrpc":"2.0","id":"fake-wdp","method":"window/workDoneProgress/create","params":{"token":"init-scenario"}}`))
				reply, err := readLSPFrame(r)
				status := "ok"
				if err != nil {
					status = "read-error:" + err.Error()
				} else {
					var replyEnvelope struct {
						Error json.RawMessage `json:"error"`
					}
					if err := json.Unmarshal(reply, &replyEnvelope); err != nil {
						status = "unmarshal-error:" + err.Error()
					} else if len(replyEnvelope.Error) > 0 {
						status = "handler-error:" + string(replyEnvelope.Error)
					}
				}
				if logPath := os.Getenv("SENNIT_LSP_FAKE_LOG"); logPath != "" {
					if file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
						_, _ = fmt.Fprintln(file, "workDoneProgress-during-init "+status)
						_ = file.Close()
					}
				}
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
