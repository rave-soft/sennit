package tools

import (
	"testing"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
)

func TestHoverContentsMarkup(t *testing.T) {
	if got := hoverContents(&protocol.Hover{Contents: protocol.MarkupContent{Kind: protocol.Markdown, Value: "`f()`"}}); got != "`f()`" {
		t.Fatalf("got %q", got)
	}
}
