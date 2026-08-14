package agent

import (
	"reflect"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"github.com/rave-soft/braid/internal/message"
	"github.com/stretchr/testify/require"
)

// originTaggedHistory builds the fantasy.Prompt history for a single
// message.User message carrying origin, converted the same way
// preparePrompt converts any persisted message: via Message.ToAIMessage.
func originTaggedHistory(text string, origin message.Origin) []fantasy.Message {
	msg := message.Message{
		Role:   message.User,
		Parts:  []message.ContentPart{message.TextContent{Text: text}},
		Origin: origin,
	}
	return msg.ToAIMessage()
}

// TestOriginDoesNotAffectToAIMessage proves Message.ToAIMessage produces
// byte-for-byte identical fantasy.Message output for a message.User
// message regardless of Origin. This is the in-process half of the
// invisible-on-the-wire guarantee: Origin is authorship metadata only
// (see message.Origin's doc comment) and must never influence what a
// dispatched prompt looks like to the conversion layer that ultimately
// feeds the model.
func TestOriginDoesNotAffectToAIMessage(t *testing.T) {
	t.Parallel()

	personMsgs := originTaggedHistory("implement the widget", message.OriginPerson)
	agentMsgs := originTaggedHistory("implement the widget", message.OriginAgent)

	require.True(t, reflect.DeepEqual(personMsgs, agentMsgs),
		"ToAIMessage output must be identical regardless of Origin: got %#v vs %#v", personMsgs, agentMsgs)
}

// TestOriginDoesNotAffectAnthropicRequestBody is the wire-level regression
// test for the same guarantee: it proves the actual HTTP request body an
// Anthropic provider sends is byte-identical whether the dispatching
// context carried PromptOrigin: message.OriginAgent (a thread/task goal or
// a thread_send/task_send follow-up) or was left untagged
// (message.OriginPerson, the default). Origin only ever affects how a
// message is persisted and displayed - never Role (always message.User)
// and never what reaches the model - so the two request bodies must match
// exactly, not just look similar.
func TestOriginDoesNotAffectAnthropicRequestBody(t *testing.T) {
	t.Parallel()

	respBody := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`

	buildPrompt := func(origin message.Origin) fantasy.Prompt {
		history := originTaggedHistory("implement the widget", origin)
		return append(fantasy.Prompt{
			fantasy.NewSystemMessage("you are a helpful coding agent"),
		}, history...)
	}

	send := func(origin message.Origin) []byte {
		srv := newCaptureBodyServer(t, respBody)
		provider, err := anthropic.New(anthropic.WithBaseURL(srv.url), anthropic.WithAPIKey("test-key"))
		require.NoError(t, err)
		lm, err := provider.LanguageModel(t.Context(), "claude-sonnet-4-5")
		require.NoError(t, err)
		_, err = lm.Generate(t.Context(), fantasy.Call{Prompt: buildPrompt(origin)})
		require.NoError(t, err)
		return srv.body
	}

	personBody := send(message.OriginPerson)
	agentBody := send(message.OriginAgent)

	require.Equal(t, string(personBody), string(agentBody),
		"the Anthropic request body must be byte-identical regardless of PromptOrigin")
}
