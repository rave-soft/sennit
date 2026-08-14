package agent

import (
	"testing"

	"github.com/rave-soft/braid/internal/config"
	"github.com/stretchr/testify/require"

	"charm.land/catwalk/pkg/catwalk"
)

// TestSummarizeBuffer_LeavesRoomForTheSummaryItself is the regression test
// for a summarize pass with nowhere to write.
//
// The flat 20k buffer applies to every context window above 200k, which is
// exactly the range where generous reply limits live. A 262k local model
// configured to write up to 32k tokens would trip the threshold with 20k
// of window left — less than a single reply — so the pass that was
// supposed to reclaim the context could not produce its own summary.
func TestSummarizeBuffer_LeavesRoomForTheSummaryItself(t *testing.T) {
	t.Parallel()

	const window, maxOut = 262_144, 32_768
	buffer := summarizeBuffer(window, maxOut)

	require.Greater(t, buffer, int64(maxOut),
		"the buffer must outlast one full reply, or the summary cannot be written")
	require.Equal(t, int64(40_960), buffer)

	// And it therefore fires earlier than the flat buffer would have.
	require.Less(t, window-buffer, int64(window-largeContextWindowBuffer))
}

// TestSummarizeBuffer_KeepsLargeWindowsUsable: the flat buffer exists so a
// very large window is not spent on headroom. A model that cannot write
// much per reply keeps that property.
func TestSummarizeBuffer_KeepsLargeWindowsUsable(t *testing.T) {
	t.Parallel()

	require.Equal(t, int64(largeContextWindowBuffer), summarizeBuffer(1_050_000, 4_000),
		"a small reply limit must not inflate the buffer")
	require.Equal(t, int64(largeContextWindowBuffer), summarizeBuffer(1_050_000, 0),
		"an unknown reply limit must leave the base alone")
}

// TestSummarizeBuffer_SmallWindowsKeepTheirRatio: below the large-window
// cutoff the buffer is a fifth of the window, where a flat 20k would be
// either meaningless or most of the context.
func TestSummarizeBuffer_SmallWindowsKeepTheirRatio(t *testing.T) {
	t.Parallel()

	require.Equal(t, int64(25_600), summarizeBuffer(128_000, 4_096))
	require.Equal(t, int64(1_638), summarizeBuffer(8_192, 512))
}

// TestSummarizeBuffer_NeverTakesMoreThanHalfTheWindow: a reply limit set
// close to (or above) the context window would otherwise put the threshold
// past the whole window, and every turn would summarize before doing any
// work.
func TestSummarizeBuffer_NeverTakesMoreThanHalfTheWindow(t *testing.T) {
	t.Parallel()

	require.Equal(t, int64(16_000), summarizeBuffer(32_000, 32_000))
	require.Equal(t, int64(4_096), summarizeBuffer(8_192, 100_000))
}

// TestMaxOutputTokens_PrefersTheExplicitSetting: a model configured with
// its own max_tokens is what the provider will be asked for, so that is
// the number the buffer has to cover — not the catalog default.
func TestMaxOutputTokens_PrefersTheExplicitSetting(t *testing.T) {
	t.Parallel()

	turn := &runTurn{model: Model{
		CatalogCfg: catwalk.Model{DefaultMaxTokens: 8_000},
		ModelCfg:   config.SelectedModel{MaxTokens: 32_768},
	}}
	require.Equal(t, int64(32_768), turn.maxOutputTokens())

	turn.model.ModelCfg.MaxTokens = 0
	require.Equal(t, int64(8_000), turn.maxOutputTokens())
}

// TestSummarizeBuffer_TripsWhileAReplyStillFits states the bug in the
// numbers it was found with: a junior-developer sub-agent on a 262k local
// model with a 32k reply limit, recorded at 242,592 tokens of context.
//
// The flat buffer did trip there — but only once 19.5k of window was left,
// which is less than a single reply from that model. Summarization fired
// into a space too small to write the summary. The reply-aware buffer
// trips while a full reply still fits, which is the whole point of firing
// at all.
func TestSummarizeBuffer_TripsWhileAReplyStillFits(t *testing.T) {
	t.Parallel()

	const window, maxOut = 262_144, 32_768
	const observed = 242_476 + 116 // prompt + completion, as recorded

	remaining := int64(window - observed)
	require.LessOrEqual(t, remaining, int64(largeContextWindowBuffer),
		"the flat buffer had tripped by this point")
	require.Less(t, remaining, int64(maxOut),
		"and left less room than the summary it asked for")

	require.Greater(t, summarizeBuffer(window, maxOut), int64(maxOut),
		"the reply-aware buffer must trip while a whole reply still fits")
}
