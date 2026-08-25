package agent

import (
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/openai"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/session"
)

// cacheKeyModel is a language model that answers only the one question
// withPromptCacheKey asks of it — its model id, which decides whether the
// Responses API or chat completions will read the options. Every other
// method is the embedded nil interface, so a test that reaches for one
// fails loudly instead of quietly proving nothing.
type cacheKeyModel struct {
	fantasy.LanguageModel
	id string
}

func (m cacheKeyModel) Model() string { return m.id }

func testModel(id string) Model { return Model{Model: cacheKeyModel{id: id}} }

func openaiProvider() config.ProviderConfig {
	return config.ProviderConfig{ID: "openai", Type: openai.Name}
}

// TestWithPromptCacheKey_SetsSessionStableKey covers the default this
// exists for: an OpenAI request that carries no prompt_cache_key of its
// own gets one keyed to its session, in whichever options shape the
// model's API reads.
func TestWithPromptCacheKey_SetsSessionStableKey(t *testing.T) {
	t.Parallel()

	t.Run("a Responses model", func(t *testing.T) {
		t.Parallel()
		out := withPromptCacheKey(fantasy.ProviderOptions{}, testModel("gpt-5.6-sol"), openaiProvider(), "sess-1")
		opts, ok := out[openai.Name].(*openai.ResponsesProviderOptions)
		require.True(t, ok, "a Responses model reads *ResponsesProviderOptions")
		require.NotNil(t, opts.PromptCacheKey)
		require.Equal(t, session.HashID("sess-1"), *opts.PromptCacheKey)
	})

	t.Run("a chat completions model", func(t *testing.T) {
		t.Parallel()
		out := withPromptCacheKey(fantasy.ProviderOptions{}, testModel("qwen3.8-27b"), openaiProvider(), "sess-1")
		opts, ok := out[openai.Name].(*openai.ProviderOptions)
		require.True(t, ok, "a chat completions model reads *ProviderOptions")
		require.NotNil(t, opts.PromptCacheKey)
		require.Equal(t, session.HashID("sess-1"), *opts.PromptCacheKey)
	})
}

// TestWithPromptCacheKey_KeyIsPerSession is the property the whole thing
// rests on: every request of one conversation must ask for the same
// machine, and two conversations must not be pushed onto one.
func TestWithPromptCacheKey_KeyIsPerSession(t *testing.T) {
	t.Parallel()

	key := func(sessionID string) string {
		out := withPromptCacheKey(fantasy.ProviderOptions{}, testModel("gpt-5.6-sol"), openaiProvider(), sessionID)
		return *out[openai.Name].(*openai.ResponsesProviderOptions).PromptCacheKey
	}
	require.Equal(t, key("sess-1"), key("sess-1"), "the same session must route to the same prefix cache")
	require.NotEqual(t, key("sess-1"), key("sess-2"))
	require.NotContains(t, key("sess-1"), "sess-1", "the key is a digest, not the session id itself")
}

// TestWithPromptCacheKey_LeavesConfigAlone covers the rule that this is a
// default and nothing more: anything the user (or a provider catalog)
// already decided survives untouched.
func TestWithPromptCacheKey_LeavesConfigAlone(t *testing.T) {
	t.Parallel()

	configured := "my-own-key"

	t.Run("an explicit prompt_cache_key wins", func(t *testing.T) {
		t.Parallel()
		in := fantasy.ProviderOptions{openai.Name: &openai.ResponsesProviderOptions{PromptCacheKey: &configured}}
		out := withPromptCacheKey(in, testModel("gpt-5.6-sol"), openaiProvider(), "sess-1")
		require.Equal(t, configured, *out[openai.Name].(*openai.ResponsesProviderOptions).PromptCacheKey)
	})

	t.Run("other configured options are kept", func(t *testing.T) {
		t.Parallel()
		store := true
		in := fantasy.ProviderOptions{openai.Name: &openai.ResponsesProviderOptions{Store: &store}}
		out := withPromptCacheKey(in, testModel("gpt-5.6-sol"), openaiProvider(), "sess-1")
		opts := out[openai.Name].(*openai.ResponsesProviderOptions)
		require.NotNil(t, opts.Store, "the configured option must survive")
		require.True(t, *opts.Store)
		require.NotNil(t, opts.PromptCacheKey, "and the default must still be filled in")
	})

	t.Run("options of an unrecognized type are not replaced", func(t *testing.T) {
		t.Parallel()
		// A value some other build put under this key: overwriting it
		// would break a request that works today, and the default is
		// never worth that.
		foreign := &anthropic.ProviderCacheControlOptions{}
		in := fantasy.ProviderOptions{openai.Name: foreign}
		out := withPromptCacheKey(in, testModel("gpt-5.6-sol"), openaiProvider(), "sess-1")
		require.Same(t, foreign, out[openai.Name])
	})
}

// TestWithPromptCacheKey_OnlyOpenAI proves the blast radius. Every other
// provider is either uninterested in the option or caches by a mechanism
// of its own (see cacheControlOptions), so none of them is touched.
func TestWithPromptCacheKey_OnlyOpenAI(t *testing.T) {
	t.Parallel()

	in := fantasy.ProviderOptions{}
	for _, tc := range []struct {
		name      string
		provider  config.ProviderConfig
		sessionID string
	}{
		{"anthropic", config.ProviderConfig{ID: "anthropic", Type: anthropic.Name}, "sess-1"},
		{"an openai-compatible provider", config.ProviderConfig{ID: "lmstudio", Type: "openai-compat"}, "sess-1"},
		{"no session to key on", openaiProvider(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := withPromptCacheKey(in, testModel("gpt-5.6-sol"), tc.provider, tc.sessionID)
			require.Empty(t, out, "nothing may be added")
		})
	}
}

// TestWithPromptCacheKey_DoesNotMutateInput matters because the options it
// is handed belong to a compiled runtime shared by every session on it —
// writing the key in place would hand one session's key to all of them.
func TestWithPromptCacheKey_DoesNotMutateInput(t *testing.T) {
	t.Parallel()

	store := true
	shared := &openai.ResponsesProviderOptions{Store: &store}
	in := fantasy.ProviderOptions{openai.Name: shared}

	first := withPromptCacheKey(in, testModel("gpt-5.6-sol"), openaiProvider(), "sess-1")
	second := withPromptCacheKey(in, testModel("gpt-5.6-sol"), openaiProvider(), "sess-2")

	require.Nil(t, shared.PromptCacheKey, "the runtime's own options must come back unchanged")
	require.Len(t, in, 1)
	require.NotEqual(t,
		*first[openai.Name].(*openai.ResponsesProviderOptions).PromptCacheKey,
		*second[openai.Name].(*openai.ResponsesProviderOptions).PromptCacheKey,
		"two sessions sharing a runtime must still get their own key")
}
