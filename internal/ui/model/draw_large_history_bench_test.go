package model

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	uv "github.com/charmbracelet/ultraviolet"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/chat"
)

// heavyMessageBody is a realistic assistant turn: prose, a list and a code
// block, all of which glamour renders differently. BenchmarkSessionPanelDraw's
// fixture uses one-line filler items instead, which keeps its frame cheap and
// hides what a real transcript costs — the sessions that actually wedge the
// UI average several kilobytes of markdown per message.
func heavyMessageBody(i int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Step %d\n\n", i)
	for para := range 3 {
		fmt.Fprintf(&b, "Looked at the resolver and the cache boundary in pass %d. "+
			"The lookup goes through the shared index before it reaches the store, "+
			"so a miss costs a full walk rather than the single probe it reads like.\n\n", para)
	}
	b.WriteString("- checked the index build\n- reproduced the miss\n- measured the walk\n\n")
	b.WriteString("```go\nfunc lookup(k string) (Entry, bool) {\n\tif e, ok := idx[k]; ok {\n\t\treturn e, true\n\t}\n\treturn walk(k)\n}\n```\n\n")
	return b.String()
}

// BenchmarkDrawLargeHistory measures one full frame against a transcript
// shaped like the ones that have actually pinned a core: many messages, each
// several kilobytes of markdown. It is the same Draw path as
// BenchmarkSessionPanelDraw, and the gap between the two numbers is the cost
// this fixture exists to expose.
func BenchmarkDrawLargeHistory(b *testing.B) {
	for _, count := range []int{200, 800, 1500} {
		b.Run(fmt.Sprintf("messages=%d", count), func(b *testing.B) {
			u := newSessionPanelBenchUI()

			items := make([]chat.MessageItem, 0, count)
			for i := range count {
				items = append(items, chat.NewAssistantMessageItem(u.com.Styles, &message.Message{
					ID:    fmt.Sprintf("heavy-%d", i),
					Role:  message.Assistant,
					Parts: []message.ContentPart{message.TextContent{Text: heavyMessageBody(i)}},
				}))
			}
			u.chat.SetMessages(items...)
			u.updateLayoutAndSize()

			const w, h = 140, 45
			u.lay.width, u.lay.height = w, h
			u.updateLayoutAndSize()
			scr := uv.NewScreenBuffer(w, h)
			area := uv.Rectangle{Max: uv.Position{X: w, Y: h}}

			// Draw once outside the loop so the steady-state cost is
			// measured, not the first-frame cache fill.
			u.Draw(scr, area)

			b.ReportAllocs()
			for b.Loop() {
				u.Draw(scr, area)
			}
		})
	}
}

// TestDrawFrameCostIsHistoryIndependent pins down what the benchmark above
// measured: a frame costs what the viewport costs, not what the transcript
// costs. The list renders only the visible slice out of a per-item cache
// (see list.List.Render), so a session with 1500 multi-kilobyte messages
// must draw in the same time as one with 200.
//
// It is worth guarding because the failure mode is invisible in a short
// session and brutal in a long one, and because the obvious "fix" to a slow
// frame — reaching for the whole item list to recompute a height or a
// count — reintroduces it without looking wrong.
func TestDrawFrameCostIsHistoryIndependent(t *testing.T) {
	measure := func(count int) time.Duration {
		u := newSessionPanelBenchUI()
		items := make([]chat.MessageItem, 0, count)
		for i := range count {
			items = append(items, chat.NewAssistantMessageItem(u.com.Styles, &message.Message{
				ID:    fmt.Sprintf("heavy-%d", i),
				Role:  message.Assistant,
				Parts: []message.ContentPart{message.TextContent{Text: heavyMessageBody(i)}},
			}))
		}
		u.chat.SetMessages(items...)
		const w, h = 140, 45
		u.lay.width, u.lay.height = w, h
		u.updateLayoutAndSize()
		scr := uv.NewScreenBuffer(w, h)
		area := uv.Rectangle{Max: uv.Position{X: w, Y: h}}

		u.Draw(scr, area) // fill the caches; the steady state is what matters
		const frames = 50
		start := time.Now()
		for range frames {
			u.Draw(scr, area)
		}
		return time.Since(start) / frames
	}

	small := measure(200)
	large := measure(1500)

	// Both must clear the same absolute ceiling the filler-fixture guard
	// uses. That is the real assertion: a ratio between two timings on a
	// shared CI machine is too noisy to fail a build on.
	require.Less(t, small, sessionPanelDrawBudget,
		"a frame over a 200-message transcript must stay within the budget")
	require.Less(t, large, sessionPanelDrawBudget,
		"a frame over a 1500-message transcript must cost the same as a short one; "+
			"if this is what regressed, something in the draw path started walking the history")
}
