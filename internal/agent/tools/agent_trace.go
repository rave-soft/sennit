package tools

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"os"
	"strings"
	"time"

	"charm.land/fantasy"
)

const AgentTraceToolName = "agent_trace"

// defaultTraceLimit and maxTraceLimit bound Limit: the doc template below
// renders both, so a future change to either stays in sync with what the
// description promises instead of silently drifting from it.
const (
	defaultTraceLimit = 200
	maxTraceLimit     = 1000
)

//go:embed agent_trace.md.tpl
var agentTraceDescriptionTmpl []byte

var agentTraceDescriptionTpl = template.Must(
	template.New("agentTraceDescription").
		Parse(string(agentTraceDescriptionTmpl)),
)

type agentTraceDescriptionData struct {
	DefaultLimit int
	MaxLimit     int
}

func agentTraceDescription() string {
	return renderTemplate(agentTraceDescriptionTpl, agentTraceDescriptionData{
		DefaultLimit: defaultTraceLimit,
		MaxLimit:     maxTraceLimit,
	})
}

type AgentTraceParams struct {
	SessionID string `json:"session_id,omitempty" description:"Session ID to trace. One of session_id or run_id is required; giving both narrows to events matching both."`
	RunID     string `json:"run_id,omitempty" description:"Run ID to trace (a single provider-request/tool-loop turn). One of session_id or run_id is required."`
	Since     string `json:"since,omitempty" description:"RFC3339 timestamp (e.g. 2024-01-15T10:30:00Z) or a duration like 5m/1h relative to now; only events at or after it."`
	Limit     int    `json:"limit,omitempty" description:"Max events to return, most recent first. Default 200, max 1000; 0 uses the default."`
	Cursor    string `json:"cursor,omitempty" description:"Continues from a previous response's next_cursor to fetch older events. Bound to session_id/run_id/since — changing any of those invalidates it."`
}

type agentTraceEvent struct {
	Time          string `json:"time,omitempty"`
	Kind          string `json:"kind"`
	SessionID     string `json:"session_id,omitempty"`
	RunID         string `json:"run_id,omitempty"`
	Provider      string `json:"provider,omitempty"`
	RequestReason string `json:"request_reason,omitempty"`
	RetryReason   string `json:"retry_reason,omitempty"`
	Outcome       string `json:"outcome,omitempty"`
	Attempt       int64  `json:"attempt,omitempty"`
	TurnID        string `json:"turn_id,omitempty"`
	LatencyMS     int64  `json:"latency_ms,omitempty"`
	Tool          string `json:"tool,omitempty"`
	Event         string `json:"event,omitempty"`
	Action        string `json:"action,omitempty"`
}

type agentTraceSummary struct {
	Attempts            int   `json:"attempts"`
	Success             int   `json:"success"`
	Errors              int   `json:"errors"`
	Canceled            int   `json:"canceled"`
	Aborted             int   `json:"aborted"`
	Retries             int   `json:"retries"`
	Summaries           int   `json:"summaries"`
	Trims               int   `json:"trims"`
	Repairs             int   `json:"orphan_repairs"`
	ToolCalls           int   `json:"tool_calls"`
	ToolResults         int   `json:"tool_results"`
	UnpairedCalls       int   `json:"unpaired_calls"`
	UnpairedResults     int   `json:"unpaired_results"`
	InputTokens         int64 `json:"input_tokens,omitempty"`
	OutputTokens        int64 `json:"output_tokens,omitempty"`
	ReasoningTokens     int64 `json:"reasoning_tokens,omitempty"`
	TotalTokens         int64 `json:"total_tokens,omitempty"`
	CacheReadTokens     int64 `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
}

type agentTraceResponse struct {
	Events       []agentTraceEvent `json:"events"`
	PageSummary  agentTraceSummary `json:"page_summary"`
	SummaryExact bool              `json:"summary_exact"`
	Truncated    bool              `json:"truncated"`
	NextCursor   string            `json:"next_cursor,omitempty"`
}

func NewAgentTraceTool(logFile string) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewParallelAgentTool(AgentTraceToolName, agentTraceDescription(), func(ctx context.Context, p AgentTraceParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		// See sennit_logs.go's NewSennitLogsTool for why cancellation and
		// Sennit's own file I/O failures must propagate as Go errors
		// rather than land in the transcript as ordinary tool results.
		if err := ctx.Err(); err != nil {
			return fantasy.ToolResponse{}, err
		}
		response, err := runAgentTrace(logFile, p)
		if err != nil {
			if errors.Is(err, errSennitIO) {
				return fantasy.ToolResponse{}, fmt.Errorf("agent trace: %w", err)
			}
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		return fantasy.NewTextResponse(response), nil
	}), map[string]toolParameterSchema{"session_id": {}, "run_id": {}})
}

func runAgentTrace(logFile string, p AgentTraceParams) (string, error) {
	if strings.TrimSpace(p.SessionID) == "" && strings.TrimSpace(p.RunID) == "" {
		return "", fmt.Errorf("one of session_id or run_id is required")
	}
	if p.Limit < 0 || p.Limit > maxTraceLimit {
		return "", fmt.Errorf("limit must be between 1 and %d", maxTraceLimit)
	}
	limit := p.Limit
	if limit == 0 {
		limit = defaultTraceLimit
	}
	p.SessionID = strings.TrimSpace(p.SessionID)
	p.RunID = strings.TrimSpace(p.RunID)
	p.Since = strings.TrimSpace(p.Since)
	filt, err := newLogFilter(SennitLogsParams{SessionID: p.SessionID, RunID: p.RunID, Since: p.Since})
	if err != nil {
		return "", err
	}
	filt.accept = knownTraceRecord
	f, err := os.Open(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			b, _ := json.Marshal(agentTraceResponse{Events: []agentTraceEvent{}})
			return string(b), nil
		}
		return "", fmt.Errorf("opening log file: %w (%w)", err, errSennitIO)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("statting log file: %w (%w)", err, errSennitIO)
	}
	start, boundary := st.Size(), int64(-1)
	if p.Cursor != "" {
		cursor, ok := strings.CutPrefix(p.Cursor, traceQueryHash(p)+".")
		if !ok {
			return "", fmt.Errorf("invalid cursor: cursor does not match this trace filter")
		}
		off, _, id, err := decodeCursor(cursor)
		if err != nil {
			return "", fmt.Errorf("invalid cursor: %w", err)
		}
		same, e := id.matches(f, st, off)
		if e != nil {
			return "", fmt.Errorf("verifying cursor identity: %w (%w)", e, errSennitIO)
		}
		if off >= st.Size() || !same {
			b, _ := json.Marshal(agentTraceResponse{Events: []agentTraceEvent{}})
			return string(b), nil
		}
		start, boundary = off, off
	}
	page := scanBackward(f, start, boundary, filt, limit)
	out := agentTraceResponse{Events: make([]agentTraceEvent, 0, len(page.entries)), Truncated: page.truncated}
	for _, record := range page.entries {
		event, ok := makeTraceEvent(record.entry)
		if !ok {
			continue
		}
		out.Events = append(out.Events, event)
	}
	globalCalls, globalResults := map[string]bool{}, map[string]bool{}
	summaryFilter := *filt
	summaryFilter.observe = func(record logRecord) {
		event, ok := makeTraceEvent(record.entry)
		if !ok {
			return
		}
		countTrace(&out.PageSummary, event, record.entry, globalCalls, globalResults)
	}
	summaryScan := scanBackward(f, st.Size(), -1, &summaryFilter, 1)
	out.SummaryExact = summaryScan.reachedStart
	out.PageSummary.UnpairedCalls, out.PageSummary.UnpairedResults = unpairedTraceIDs(globalCalls, globalResults)
	if page.truncated && len(page.entries) > 0 {
		id, e := newFileIdentity(f, st, page.entries[0].offset)
		if e != nil {
			return "", fmt.Errorf("creating cursor identity: %w (%w)", e, errSennitIO)
		}
		out.NextCursor = traceQueryHash(p) + "." + encodeCursor(page.entries[0].offset, 0, id)
	}
	b, err := json.Marshal(out)
	return string(b), err
}

func traceQueryHash(p AgentTraceParams) string {
	sum := sha256.Sum256([]byte(p.SessionID + "\x00" + p.RunID + "\x00" + p.Since))
	return fmt.Sprintf("atc1%x", sum[:])
}

func unpairedTraceIDs(calls, results map[string]bool) (unpairedCalls, unpairedResults int) {
	for id := range calls {
		if id != "" && !results[id] {
			unpairedCalls++
		}
	}
	for id := range results {
		if id != "" && !calls[id] {
			unpairedResults++
		}
	}
	return unpairedCalls, unpairedResults
}

func knownTraceRecord(r map[string]any) bool { _, ok := makeTraceEvent(r); return ok }
func makeTraceEvent(r map[string]any) (agentTraceEvent, bool) {
	msg := fmt.Sprint(r["msg"])
	kind := ""
	switch msg {
	case "Provider request started":
		kind = "attempt_started"
	case "Provider request finished":
		kind = "attempt_finished"
	case "Provider request failed, retrying":
		kind = "retry"
	case "Tool lifecycle":
		switch stringField(r, "event") {
		case "tool_call":
			kind = "tool_call"
		case "tool_result":
			kind = "tool_result"
		default:
			return agentTraceEvent{}, false
		}
	case "Trimmed the carried sub-agent session to the budget":
		kind = "trim"
	case "Dropping orphaned tool result with no matching tool call", "Injecting synthetic tool result for orphaned tool call":
		kind = "repair"
	default:
		return agentTraceEvent{}, false
	}
	e := agentTraceEvent{Kind: kind, Time: stringField(r, "time"), SessionID: stringField(r, "session_id"), RunID: stringField(r, "run_id"), Provider: stringField(r, "provider"), RequestReason: stringField(r, "request_reason"), RetryReason: stringField(r, "retry_reason"), Attempt: numberField(r, "attempt"), TurnID: stringField(r, "turn_id"), Tool: firstStringField(r, "tool_name", "tool"), Event: stringField(r, "event"), Action: stringField(r, "action")}
	switch kind {
	case "attempt_finished":
		e.Outcome = stringField(r, "outcome")
	case "tool_call", "tool_result":
		e.Outcome = stringField(r, "tool_outcome")
	}
	e.LatencyMS = durationMS(r["latency"])
	if e.LatencyMS == 0 {
		e.LatencyMS = numberField(r, "latency_ms")
	}
	return e, true
}

func stringField(r map[string]any, k string) string {
	v := r[k]
	s, ok := v.(string)
	if ok {
		return s
	}
	return ""
}

func firstStringField(r map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringField(r, key); value != "" {
			return value
		}
	}
	return ""
}

func numberField(r map[string]any, k string) int64 {
	switch v := r[k].(type) {
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case int:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

func durationMS(v any) int64 {
	if s, ok := v.(string); ok {
		d, e := time.ParseDuration(s)
		if e == nil {
			return d.Milliseconds()
		}
	}
	return 0
}

func countTrace(s *agentTraceSummary, e agentTraceEvent, r map[string]any, calls, results map[string]bool) {
	switch e.Kind {
	case "attempt_started":
		s.Attempts++
		if e.RequestReason == "summary" {
			s.Summaries++
		}
	case "attempt_finished":
		switch e.Outcome {
		case "success":
			s.Success++
		case "error":
			s.Errors++
		case "canceled":
			s.Canceled++
		case "aborted":
			s.Aborted++
		}
	case "retry":
		s.Retries++
	case "trim":
		s.Trims++
	case "repair":
		s.Repairs++
	case "tool_call":
		s.ToolCalls++
		calls[stringField(r, "tool_call_id")] = true
	case "tool_result":
		s.ToolResults++
		results[stringField(r, "tool_call_id")] = true
	}
	s.InputTokens += numberField(r, "input_tokens")
	s.OutputTokens += numberField(r, "output_tokens")
	s.ReasoningTokens += numberField(r, "reasoning_tokens")
	s.TotalTokens += numberField(r, "total_tokens")
	s.CacheReadTokens += numberField(r, "cache_read_tokens")
	s.CacheCreationTokens += numberField(r, "cache_creation_tokens")
}
