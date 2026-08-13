package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// Deterministic fixture server
// ---------------------------------------------------------------------------

// FixtureToolCall describes one tool call in a canned LLM response.
type FixtureToolCall struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Name   string `json:"name"`
	Args   string `json:"args"`
	Reason string `json:"reason,omitempty"`
}

// FixtureTurn describes one canned LLM turn.
type FixtureTurn struct {
	ToolCalls []FixtureToolCall `json:"tool_calls"`
	Text      string            `json:"text,omitempty"`
}

// FixtureScenario defines the canned responses for one test scenario.
type FixtureScenario struct {
	Turns []FixtureTurn `json:"turns"`
}

// FixtureConfig holds the configuration for the fixture server.
type FixtureConfig struct {
	// Scenario returns the scenario name for the current request.
	Scenario func() string
	// DefaultModel is the model name returned in responses.
	DefaultModel string
}

// FixtureServer is a deterministic HTTP server that simulates an
// OpenAI-compatible chat completions endpoint.  It returns canned
// SSE streams based on the configured scenario.
type FixtureServer struct {
	mu              sync.Mutex
	config          FixtureConfig
	turns           map[string]int // scenario -> current turn index
	resourceBaseURL string
}

// NewFixtureServer creates a new fixture server backed by an
// httptest.Server. Call ServeHTTP to handle requests; call Start
// to return the *httptest.Server.
func NewFixtureServer(cfg FixtureConfig) *FixtureServer {
	return &FixtureServer{
		config: cfg,
		turns:  make(map[string]int),
	}
}

// NewFixtureHTTPServer creates a fixture server and returns both the
// FixtureServer and the *httptest.Server.
func NewFixtureHTTPServer(cfg FixtureConfig) (*FixtureServer, *httptest.Server) {
	fsrv := NewFixtureServer(cfg)
	srv := httptest.NewServer(http.HandlerFunc(fsrv.ServeHTTP))
	fsrv.resourceBaseURL = srv.URL
	return fsrv, srv
}

// ResourceURL returns the URL for a deterministic fixture resource.
func (s *FixtureServer) ResourceURL(path string) string {
	return s.resourceBaseURL + path
}

// ServeHTTP implements deterministic resource and OpenAI-compatible chat endpoints.
func (s *FixtureServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/download":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("fixture download content\n"))
		return
	case r.Method == http.MethodGet && r.URL.Path == "/fetch":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body><p>John Doe fixture content.</p></body></html>"))
		return
	case r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "chat/completions"):
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	scenario := ""
	if s.config.Scenario != nil {
		scenario = s.config.Scenario()
	}
	if scenario == "" {
		scenario = "default"
	}

	body, err := readAndReplaceBody(r)
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	var request struct {
		Tools json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(body, &request) != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	// Title generation does not carry tools. Keep it separate from scenario
	// turns so every tool scenario starts with its declared first response.
	if len(request.Tools) == 0 {
		sseStream(w, FixtureTurn{Text: "Test session"}, s.config.DefaultModel)
		return
	}

	s.mu.Lock()
	turnIdx := s.turns[scenario]
	s.turns[scenario] = turnIdx + 1
	s.mu.Unlock()

	scenarios := fixtureScenarios()
	sc, ok := scenarios[scenario]
	if !ok {
		sc = scenarios["default"]
	}

	if turnIdx >= len(sc.Turns) {
		turnIdx = len(sc.Turns) - 1
	}

	turn := fixtureTurnWithResourceURLs(sc.Turns[turnIdx], s.resourceBaseURL)
	sseStream(w, turn, s.config.DefaultModel)
}

// ResetTurns resets the turn counters for all scenarios. Call this
// before starting a new test run.
func (s *FixtureServer) ResetTurns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.turns {
		delete(s.turns, k)
	}
}

// ---------------------------------------------------------------------------
// SSE response helpers
// ---------------------------------------------------------------------------

// sseStream writes an SSE stream for one turn to the response writer.
func sseStream(w http.ResponseWriter, turn FixtureTurn, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	chunkID := "chatcmpl-fixture-0"
	created := 0

	if turn.Text != "" {
		_, _ = w.Write([]byte("data: {\"id\":\"" + chunkID + "\",\"object\":\"chat.completion.chunk\",\"created\":" + fmt.Sprint(created) + ",\"model\":\"" + model + "\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"" + escapeSSE(turn.Text) + "\"},\"finish_reason\":null}]}\n\n"))
	}

	for index, tc := range turn.ToolCalls {
		_, _ = w.Write([]byte("data: {\"id\":\"" + chunkID + "\",\"object\":\"chat.completion.chunk\",\"created\":" + fmt.Sprint(created) + ",\"model\":\"" + model + "\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":" + fmt.Sprint(index) + ",\"id\":\"" + tc.ID + "\",\"type\":\"function\",\"function\":{\"name\":\"" + tc.Name + "\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"" + chunkID + "\",\"object\":\"chat.completion.chunk\",\"created\":" + fmt.Sprint(created) + ",\"model\":\"" + model + "\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":" + fmt.Sprint(index) + ",\"function\":{\"arguments\":\"" + escapeJSON(tc.Args) + "\"}}]},\"finish_reason\":null}]}\n\n"))
	}

	// Final chunk with finish_reason.
	if turn.Text != "" || len(turn.ToolCalls) > 0 {
		finish := "tool_calls"
		if len(turn.ToolCalls) == 0 && turn.Text != "" {
			finish = "stop"
		}
		_, _ = w.Write([]byte("data: {\"id\":\"" + chunkID + "\",\"object\":\"chat.completion.chunk\",\"created\":" + fmt.Sprint(created) + ",\"model\":\"" + model + "\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"" + finish + "\"}]}\n\n"))
	}

	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

func escapeSSE(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	s = strings.ReplaceAll(s, "\t", "\\t")
	return s
}

func escapeJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return s
	}
	return string(b[1 : len(b)-1]) // strip quotes
}

// ---------------------------------------------------------------------------
// Predefined scenarios (12 subtests + default)
// ---------------------------------------------------------------------------

func fixtureTurnWithResourceURLs(turn FixtureTurn, baseURL string) FixtureTurn {
	for index := range turn.ToolCalls {
		turn.ToolCalls[index].Args = strings.ReplaceAll(turn.ToolCalls[index].Args, "{{fixture_url}}", baseURL)
	}
	return turn
}

func fixtureScenarios() map[string]FixtureScenario {
	return map[string]FixtureScenario{
		"simple_test": {
			Turns: []FixtureTurn{
				{
					Text: "Hello! How can I help you today?",
				},
			},
		},
		"deterministic_flow": {
			Turns: []FixtureTurn{
				{Text: "Hello! How can I help you today?"},
				{ToolCalls: []FixtureToolCall{
					{ID: "call_fetch_1", Type: "function", Name: "fetch", Args: `{"url":"{{fixture_url}}/fetch","format":"text"}`},
				}},
				{Text: "I found the content and it mentions John Doe."},
			},
		},
		"read_a_file": {
			Turns: []FixtureTurn{
				{
					ToolCalls: []FixtureToolCall{
						{ID: "call_view_1", Type: "function", Name: "view", Args: `{"file_path":"go.mod"}`},
					},
				},
				{
					Text: "I found the go.mod file. It contains the module definition for the test project.",
				},
			},
		},
		"update_a_file": {
			Turns: []FixtureTurn{
				{
					ToolCalls: []FixtureToolCall{
						{ID: "call_view_1", Type: "function", Name: "view", Args: `{"file_path":"main.go"}`},
					},
				},
				{
					ToolCalls: []FixtureToolCall{
						{ID: "call_edit_1", Type: "function", Name: "edit", Args: `{"file_path":"main.go","old_string":"fmt.Println(\"Hello, World!\")","new_string":"fmt.Println(\"hello from braid\")"}`},
					},
				},
				{
					Text: "The file has been updated.",
				},
			},
		},
		"bash_tool": {
			Turns: []FixtureTurn{
				{
					ToolCalls: []FixtureToolCall{
						{ID: "call_bash_1", Type: "function", Name: "bash", Args: `{"description":"create test file","command":"printf 'hello bash' > test.txt"}`},
					},
				},
				{
					Text: "The file has been created.",
				},
			},
		},
		"download_tool": {
			Turns: []FixtureTurn{
				{
					ToolCalls: []FixtureToolCall{
						{ID: "call_download_1", Type: "function", Name: "download", Args: `{"url":"{{fixture_url}}/download","file_path":"example.txt"}`},
					},
				},
				{
					Text: "The file has been downloaded.",
				},
			},
		},
		"fetch_tool": {
			Turns: []FixtureTurn{
				{
					ToolCalls: []FixtureToolCall{
						{ID: "call_fetch_1", Type: "function", Name: "fetch", Args: `{"url":"{{fixture_url}}/fetch","format":"text"}`},
					},
				},
				{
					Text: "I found the content and it mentions John Doe.",
				},
			},
		},
		"glob_tool": {
			Turns: []FixtureTurn{
				{
					ToolCalls: []FixtureToolCall{
						{ID: "call_glob_1", Type: "function", Name: "glob", Args: `{"pattern":"main.go"}`},
					},
				},
				{
					Text: "I found main.go in the project.",
				},
			},
		},
		"grep_tool": {
			Turns: []FixtureTurn{
				{ToolCalls: []FixtureToolCall{{ID: "call_grep_1", Type: "function", Name: "grep", Args: `{"pattern":"package","include":"*.go"}`}}},
				{Text: "I found the package declaration in main.go."},
			},
		},
		"ls_tool": {
			Turns: []FixtureTurn{
				{
					ToolCalls: []FixtureToolCall{
						{ID: "call_ls_1", Type: "function", Name: "ls", Args: `{"path":"."}`},
					},
				},
				{
					Text: "The directory contains go.mod and main.go.",
				},
			},
		},
		"multiedit_tool": {
			Turns: []FixtureTurn{
				{ToolCalls: []FixtureToolCall{{ID: "call_view_1", Type: "function", Name: "view", Args: `{"file_path":"main.go"}`}}},
				{
					ToolCalls: []FixtureToolCall{
						{ID: "call_medit_1", Type: "function", Name: "multiedit", Args: `{"file_path":"main.go","edits":[{"old_string":"func main() {\n\tfmt.Println(\"Hello, World!\")\n}","new_string":"func main() {\n\t// Greeting\n\tfmt.Println(\"Hello, Braid!\")\n}"}]}`},
					},
				},
				{
					Text: "The file has been updated.",
				},
			},
		},
		"write_tool": {
			Turns: []FixtureTurn{
				{
					ToolCalls: []FixtureToolCall{
						{ID: "call_write_1", Type: "function", Name: "write", Args: `{"file_path":"config.json","content":"{\"name\":\"test\",\"version\":\"1.0.0\"}"}`},
					},
				},
				{
					Text: "The file has been created.",
				},
			},
		},
		"parallel_tool_calls": {
			Turns: []FixtureTurn{
				{
					ToolCalls: []FixtureToolCall{
						{ID: "call_glob_1", Type: "function", Name: "glob", Args: `{"pattern":"main.go"}`},
						{ID: "call_ls_1", Type: "function", Name: "ls", Args: `{"path":"."}`},
					},
				},
				{
					Text: "I found main.go and listed the directory contents.",
				},
			},
		},
		"default": {
			Turns: []FixtureTurn{
				{
					Text: "I understood your request.",
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Body reading helper for request inspection
// ---------------------------------------------------------------------------

// readAndReplaceBody reads the request body, stores it, and replaces
// r.Body so it can be re-read. Returns the body bytes or error.
func readAndReplaceBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(data))
	return data, nil
}
