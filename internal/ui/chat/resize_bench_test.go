package chat

import (
	"testing"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

func BenchmarkResizeSession(b *testing.B) {
	msgs := []message.Message{
		{ID: "user", Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "Inspect the session and summarize the changes."}}},
		{ID: "assistant", Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "I inspected the workspace and verified the affected packages."}}},
	}
	ptrs := make([]*message.Message, len(msgs))
	for i := range msgs {
		ptrs[i] = &msgs[i]
	}
	toolResults := BuildToolResultMap(ptrs)
	sty := styles.BraidDark()
	var items []list.Item
	for _, msg := range ptrs {
		for _, item := range ExtractMessageItems(&sty, msg, toolResults, nil) {
			items = append(items, item)
		}
	}
	l := list.NewList(items...)
	widths := []int{100, 99}
	i := 0
	for b.Loop() {
		l.SetSize(widths[i%2], 40)
		_ = l.TotalHeight()
		i++
	}
}
