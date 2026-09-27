package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Fake OpenAI-compatible chat-completions server
// ---------------------------------------------------------------------------
//
// This mirrors internal/agent's FixtureServer (fixture_server_test.go) --
// same request/response shape, since both talk to charm.land/fantasy's
// openai provider -- but adds what this package's scenarios need that the
// agent package's fixture doesn't: a per-chunk delay so a turn is
// observably in flight for a few seconds (scenario a needs to disconnect
// mid-turn), and tool calls scripted per test rather than picked from a
// fixed catalog by name. It is not shared with internal/agent's copy
// (test-only code, deliberately not exported) -- see that file's own doc
// comment for why FixtureServer isn't reused directly.

// fixtureToolCall is one tool call the fake model emits in a turn.
type fixtureToolCall struct {
	ID   string
	Name string
	Args string
}

// fixtureTurn is one canned assistant turn: either plain text, or one or
// more tool calls (never both -- like a real model, this fixture finishes
// a turn with either a stop or a tool_calls finish reason). ChunkDelay, if
// set, sleeps between each streamed piece of Text so the turn is
// observably in flight for that long; zero streams immediately.
type fixtureTurn struct {
	Text       string
	ToolCalls  []fixtureToolCall
	ChunkDelay time.Duration
}

// fixtureServer serves an OpenAI-compatible /v1/chat/completions endpoint
// backed by a fixed, ordered list of turns: the request that carries no
// "tools" (sennit's title-generation call) always gets a generic title
// back; every other request advances to the next scripted turn, and the
// last turn repeats for any request past the end of the script (so a
// model that unexpectedly asks again doesn't 404 the whole turn).
type fixtureServer struct {
	mu    sync.Mutex
	turns []fixtureTurn
	next  int
}

func newFixtureServer(turns ...fixtureTurn) *httptest.Server {
	fs := &fixtureServer{turns: turns}
	return httptest.NewServer(http.HandlerFunc(fs.serveHTTP))
}

func (s *fixtureServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "chat/completions") {
		http.Error(w, "e2e fixture: unexpected request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "e2e fixture: read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	var req struct {
		Model string          `json:"model"`
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "e2e fixture: invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if len(req.Tools) == 0 || string(req.Tools) == "null" {
		// Title generation: no tools attached to this request.
		s.stream(w, fixtureTurn{Text: "E2E test session"}, req.Model)
		return
	}

	s.mu.Lock()
	idx := s.next
	if idx >= len(s.turns) {
		idx = len(s.turns) - 1
	}
	if s.next < len(s.turns) {
		s.next++
	}
	turn := s.turns[idx]
	s.mu.Unlock()

	if idx < 0 {
		http.Error(w, "e2e fixture: no turns scripted", http.StatusInternalServerError)
		return
	}
	s.stream(w, turn, req.Model)
}

// stream writes turn as an SSE chat-completion-chunk stream, matching the
// shape charm.land/fantasy's openai provider parses.
func (s *fixtureServer) stream(w http.ResponseWriter, turn fixtureTurn, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	const chunkID = "chatcmpl-e2e-fixture"
	write := func(delta string) {
		_, _ = fmt.Fprintf(w, "data: {\"id\":%q,\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":%s,\"finish_reason\":null}]}\n\n",
			chunkID, model, delta)
		if flusher != nil {
			flusher.Flush()
		}
	}

	switch {
	case turn.Text != "":
		pieces := splitForStreaming(turn.Text, turn.ChunkDelay > 0)
		for i, piece := range pieces {
			role := ""
			if i == 0 {
				role = `"role":"assistant",`
			}
			write(fmt.Sprintf(`{%s"content":%s}`, role, jsonString(piece)))
			if turn.ChunkDelay > 0 && i < len(pieces)-1 {
				time.Sleep(turn.ChunkDelay)
			}
		}
		finish(w, chunkID, model, "stop")
	case len(turn.ToolCalls) > 0:
		for i, tc := range turn.ToolCalls {
			write(fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":%s,"type":"function","function":{"name":%s,"arguments":""}}]}`,
				i, jsonString(tc.ID), jsonString(tc.Name)))
			write(fmt.Sprintf(`{"tool_calls":[{"index":%d,"function":{"arguments":%s}}]}`, i, jsonString(tc.Args)))
		}
		finish(w, chunkID, model, "tool_calls")
	default:
		finish(w, chunkID, model, "stop")
	}

	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func finish(w http.ResponseWriter, chunkID, model, reason string) {
	_, _ = fmt.Fprintf(w, "data: {\"id\":%q,\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":%q}]}\n\n",
		chunkID, model, reason)
}

// splitForStreaming breaks text into words (so a slow turn visibly
// dribbles out over ChunkDelay*len(words)) or returns it whole when no
// delay was requested -- an instant turn gains nothing from chunking.
func splitForStreaming(text string, slow bool) []string {
	if !slow {
		return []string{text}
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{text}
	}
	out := make([]string, len(words))
	for i, wd := range words {
		if i > 0 {
			wd = " " + wd
		}
		out[i] = wd
	}
	return out
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
