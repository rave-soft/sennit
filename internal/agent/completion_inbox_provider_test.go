package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"github.com/stretchr/testify/require"
)

// captureBodyServer is a bare HTTP server that records the raw body of
// the first request it receives and answers with a fixed, minimally
// well-formed success payload so the SDK client under test doesn't choke
// trying to parse the response - these tests only care about what was
// sent on the wire, not what came back.
type captureBodyServer struct {
	url  string
	body []byte
}

func newCaptureBodyServer(t *testing.T, responseBody string) *captureBodyServer {
	t.Helper()
	c := &captureBodyServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.body = b
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(responseBody))
	}))
	t.Cleanup(srv.Close)
	c.url = srv.URL
	return c
}

// lateCompletionPrompt builds the exact shape prepareStep produces once a
// completion is folded into a step that already has history: a system
// prompt, an earlier exchange, and the completion appended after it.
// This is precisely the position ("after a user/assistant message has
// already gone by") that silently dropped a system-role completion on
// Anthropic and Google - see taskCompletionsMessage's doc comment.
func lateCompletionPrompt(completion TaskCompletion) fantasy.Prompt {
	return fantasy.Prompt{
		fantasy.NewSystemMessage("you are a helpful coding agent"),
		fantasy.NewUserMessage("main"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "working on it"}}},
		taskCompletionsMessage([]TaskCompletion{completion}),
	}
}

// TestTaskCompletionsMessage_ReachesAnthropicRequest is the regression
// test for the bug the coordinator caught: a completion folded into a
// step after other messages must actually appear in the real request
// body a provider adapter builds, not just in prepareStep's own
// prepared.Messages (which is upstream of every adapter and would not
// have caught this). Before the fix (fantasy.NewSystemMessage), this
// test fails: providers/anthropic/anthropic.go's toPrompt groups
// consecutive system messages into a block and skips (silently) any
// further system block once a user/assistant message has intervened.
func TestTaskCompletionsMessage_ReachesAnthropicRequest(t *testing.T) {
	t.Parallel()

	srv := newCaptureBodyServer(t, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`)

	provider, err := anthropic.New(anthropic.WithBaseURL(srv.url), anthropic.WithAPIKey("test-key"))
	require.NoError(t, err)
	lm, err := provider.LanguageModel(t.Context(), "claude-sonnet-4-5")
	require.NoError(t, err)

	completion := TaskCompletion{
		DelegationID:   "task-1",
		Kind:           "task",
		Name:           "task-name",
		Goal:           "background goal",
		Status:         "completed",
		ChildSessionID: "child-session",
		ResultText:     "anthropic-wire-marker",
	}

	// The error return is not asserted: the fake response above is
	// minimal and the SDK may or may not be fully satisfied by it. What
	// this test proves is what was already written to srv.body by the
	// time the client sent the request, which happens regardless of how
	// the response is handled afterward.
	_, _ = lm.Generate(t.Context(), fantasy.Call{Prompt: lateCompletionPrompt(completion)})

	require.Contains(t, string(srv.body), "anthropic-wire-marker",
		"the completion must reach the actual Anthropic request body; a system-role message here would be silently dropped by toPrompt's late-system-block skip")
	require.Contains(t, string(srv.body), "system-generated delegation report",
		"the completion's self-identifying label must travel with it onto the wire")
}

// TestTaskCompletionsMessage_ReachesGoogleRequest is the same regression
// test for the second provider the coordinator flagged: google.go has
// the identical `if finishedSystemBlock { continue }` skip as Anthropic
// in its prompt-conversion loop, so a completion folded in as a
// system-role message after other turns would vanish there too.
func TestTaskCompletionsMessage_ReachesGoogleRequest(t *testing.T) {
	t.Parallel()

	srv := newCaptureBodyServer(t, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`)

	provider, err := google.New(google.WithBaseURL(srv.url), google.WithGeminiAPIKey("test-key"))
	require.NoError(t, err)
	lm, err := provider.LanguageModel(t.Context(), "gemini-2.5-flash")
	require.NoError(t, err)

	completion := TaskCompletion{
		DelegationID:   "task-1",
		Kind:           "task",
		Name:           "task-name",
		Goal:           "background goal",
		Status:         "completed",
		ChildSessionID: "child-session",
		ResultText:     "google-wire-marker",
	}

	_, _ = lm.Generate(t.Context(), fantasy.Call{Prompt: lateCompletionPrompt(completion)})

	require.Contains(t, string(srv.body), "google-wire-marker",
		"the completion must reach the actual Google/Gemini request body; a system-role message here would be silently dropped by toGooglePrompt's late-system-block skip")
	require.Contains(t, string(srv.body), "system-generated delegation report",
		"the completion's self-identifying label must travel with it onto the wire")
}

// TestTaskCompletionsMessage_MessageVariantReachesAnthropicRequest proves
// a mid-run ask (IsMessage true) rides the exact same safe user-role
// delivery path a terminal completion does - not a system message, which
// this same Anthropic adapter would silently drop once folded in after
// other turns (see TestTaskCompletionsMessage_ReachesAnthropicRequest).
func TestTaskCompletionsMessage_MessageVariantReachesAnthropicRequest(t *testing.T) {
	t.Parallel()

	srv := newCaptureBodyServer(t, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`)

	provider, err := anthropic.New(anthropic.WithBaseURL(srv.url), anthropic.WithAPIKey("test-key"))
	require.NoError(t, err)
	lm, err := provider.LanguageModel(t.Context(), "claude-sonnet-4-5")
	require.NoError(t, err)

	msg := TaskCompletion{
		DelegationID:   "task-1",
		Kind:           "task",
		Name:           "task-name",
		ChildSessionID: "child-session",
		IsMessage:      true,
		Message:        "anthropic-message-wire-marker",
	}

	_, _ = lm.Generate(t.Context(), fantasy.Call{Prompt: lateCompletionPrompt(msg)})

	require.Contains(t, string(srv.body), "anthropic-message-wire-marker",
		"a mid-run ask must reach the actual Anthropic request body; a system-role message here would be silently dropped by toPrompt's late-system-block skip")
	require.Contains(t, string(srv.body), "system-generated delegation report",
		"the message's self-identifying label must travel with it onto the wire")
}

// TestTaskCompletionsMessage_MessageVariantReachesGoogleRequest is the
// same regression test for the Google adapter's identical late-system-
// block skip. See TestTaskCompletionsMessage_ReachesGoogleRequest.
func TestTaskCompletionsMessage_MessageVariantReachesGoogleRequest(t *testing.T) {
	t.Parallel()

	srv := newCaptureBodyServer(t, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`)

	provider, err := google.New(google.WithBaseURL(srv.url), google.WithGeminiAPIKey("test-key"))
	require.NoError(t, err)
	lm, err := provider.LanguageModel(t.Context(), "gemini-2.5-flash")
	require.NoError(t, err)

	msg := TaskCompletion{
		DelegationID:   "task-1",
		Kind:           "task",
		Name:           "task-name",
		ChildSessionID: "child-session",
		IsMessage:      true,
		Message:        "google-message-wire-marker",
	}

	_, _ = lm.Generate(t.Context(), fantasy.Call{Prompt: lateCompletionPrompt(msg)})

	require.Contains(t, string(srv.body), "google-message-wire-marker",
		"a mid-run ask must reach the actual Google/Gemini request body; a system-role message here would be silently dropped by toGooglePrompt's late-system-block skip")
	require.Contains(t, string(srv.body), "system-generated delegation report",
		"the message's self-identifying label must travel with it onto the wire")
}
