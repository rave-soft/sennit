// Package richpaste turns a rich clipboard payload — the text/html flavor a
// browser or document editor sets when the selection mixed prose with images
// — into images the editor can attach.
package richpaste

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
)

// MaxImages caps how many images a single paste pulls in, so that copying a
// whole article does not queue dozens of downloads.
const MaxImages = 10

// ImageSources returns the src of every <img> in markup, in document order,
// deduplicated and capped at MaxImages. Sources it cannot use later (relative
// URLs, say) are left in: Resolve is what decides.
func ImageSources(markup []byte) []string {
	doc, err := html.Parse(bytes.NewReader(markup))
	if err != nil {
		return nil
	}

	var (
		srcs []string
		seen = make(map[string]struct{})
		walk func(*html.Node)
	)
	walk = func(n *html.Node) {
		if len(srcs) >= MaxImages {
			return
		}
		if n.Type == html.ElementNode && n.Data == "img" {
			if src := strings.TrimSpace(attr(n, "src")); src != "" {
				if _, dup := seen[src]; !dup {
					seen[src] = struct{}{}
					srcs = append(srcs, src)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return srcs
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}
