package richpaste

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImageSourcesKeepsDocumentOrderAndDeduplicates(t *testing.T) {
	t.Parallel()

	markup := []byte(`
		<p>before <img src="a.png"> middle</p>
		<div><img alt="x" src="https://example.com/b.png"></div>
		<img src="a.png">
	`)

	require.Equal(t, []string{"a.png", "https://example.com/b.png"}, ImageSources(markup))
}

func TestImageSourcesIgnoresImagesWithoutSource(t *testing.T) {
	t.Parallel()

	require.Empty(t, ImageSources([]byte(`<img alt="none"><img src="  ">`)))
}

func TestImageSourcesCapsAtMaxImages(t *testing.T) {
	t.Parallel()

	var markup strings.Builder
	for i := range MaxImages + 5 {
		fmt.Fprintf(&markup, `<img src="%d.png">`, i)
	}

	require.Len(t, ImageSources([]byte(markup.String())), MaxImages)
}

func TestImageSourcesToleratesCFHTMLHeader(t *testing.T) {
	t.Parallel()

	// Windows hands back CF_HTML, whose header lines are not markup.
	markup := []byte("Version:0.9\r\nStartHTML:00000097\r\n<html><body><!--StartFragment--><img src=\"a.png\"><!--EndFragment--></body></html>")

	require.Equal(t, []string{"a.png"}, ImageSources(markup))
}
