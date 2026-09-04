package mcp

import (
	"cmp"
	"fmt"
	"mime"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MaxResourceContentBytes bounds how much of one resource's text is handed
// to the model at once, whether it arrives as an *mcp.EmbeddedResource
// inside a tool result or through a direct read_mcp_resource call. Matches
// fetch_helpers.go's FetchURLAndConvert cap (5 MiB): an MCP resource is
// frequently a whole file rather than a web-page excerpt, and without this
// a 20 MB PDF or log dump lands whole in the model's context, breaks UTF-8
// mid-rune, and gets persisted into every following turn.
const MaxResourceContentBytes = 5 * 1024 * 1024

// ResourceContentIsText reports whether mimeType names textual content
// safe to decode and hand to the model as a string: text/* per RFC 2046,
// plus the JSON family (application/json and the many "+json" structured-
// syntax suffixes, RFC 6839) MCP servers commonly use for structured
// resources. An empty MIME type is treated as text: the protocol doesn't
// require servers to set one, and refusing to print an unlabeled resource
// just because the header is missing would be worse than occasionally
// misjudging a genuinely binary one.
func ResourceContentIsText(mimeType string) bool {
	mimeType = strings.TrimSpace(mimeType)
	if mimeType == "" {
		return true
	}
	base, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		base = mimeType
	}
	base = strings.ToLower(strings.TrimSpace(base))
	return strings.HasPrefix(base, "text/") || base == "application/json" || strings.HasSuffix(base, "+json")
}

// ResourceContentIsImage reports whether mimeType names an image format -
// the one binary shape resource content is turned into a normal tool image
// response for, instead of being rejected as unreadable.
func ResourceContentIsImage(mimeType string) bool {
	base, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		base = mimeType
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(base)), "image/")
}

// truncateResourceText caps s at MaxResourceContentBytes, backing off to
// the nearest earlier rune boundary so a multi-byte UTF-8 rune straddling
// the cut point is dropped whole rather than split into an invalid
// trailing fragment, and reports whether it truncated. This mirrors
// fetch.go's truncateToRuneBoundary; that helper lives in package tools,
// and pulling it in here would make mcp depend on tools (backwards - see
// AGENTS.md's layering rule), so this is its own copy of the same
// one-line cut.
func truncateResourceText(s string) (string, bool) {
	if len(s) <= MaxResourceContentBytes {
		return s, false
	}
	n := MaxResourceContentBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// FormatResourceContentsText renders one resource's content as text for a
// tool result, truncating oversized text (with a notice, matching fetch.go's
// own truncation marker) and describing rather than dumping content this
// can't safely print as-is (G13: read_mcp_resource used to do
// string(content.Blob) unconditionally, base64-decoded bytes and all, with
// no size cap).
func FormatResourceContentsText(rc *mcp.ResourceContents) string {
	if rc == nil {
		return ""
	}
	if rc.Text != "" {
		text, truncated := truncateResourceText(rc.Text)
		if truncated {
			text += fmt.Sprintf("\n\n[Content truncated to %d bytes]", MaxResourceContentBytes)
		}
		return text
	}
	if len(rc.Blob) == 0 {
		return ""
	}
	if ResourceContentIsText(rc.MIMEType) {
		text, truncated := truncateResourceText(string(rc.Blob))
		if truncated {
			text += fmt.Sprintf("\n\n[Content truncated to %d bytes]", MaxResourceContentBytes)
		}
		return text
	}
	return fmt.Sprintf("[binary resource %s: %d bytes, %s - not shown]", rc.URI, len(rc.Blob), cmp.Or(rc.MIMEType, "unknown MIME type"))
}

// TruncateResourceContentText caps s, the text a caller has already joined
// from possibly several ResourceContents parts, at MaxResourceContentBytes -
// the same cap FormatResourceContentsText applies to one part in isolation.
// Without this, a multi-part resource (read_mcp_resource.go's own read, one
// call per part) could hand the model N times the intended budget: each
// part on its own stays under the cap, but the joined response does not.
// Mirrors tools.go's RunTool, which applies the identical bound to its own
// joined textContent for the same reason.
func TruncateResourceContentText(s string) string {
	truncated, wasTruncated := truncateResourceText(s)
	if !wasTruncated {
		return s
	}
	return truncated + fmt.Sprintf("\n\n[Content truncated to %d bytes]", MaxResourceContentBytes)
}

// formatEmbeddedResource renders an *mcp.EmbeddedResource's content as text
// for a tool's textual result. G12: this used to fall through to the
// default %v branch in tools.go's RunTool, printing a memory-address dump
// like "&{map[] <nil> 0xc000...}" instead of the resource's actual
// content whenever a server answered with an embedded resource.
func formatEmbeddedResource(c *mcp.EmbeddedResource) string {
	if c == nil {
		return ""
	}
	return FormatResourceContentsText(c.Resource)
}

// formatResourceLink renders an *mcp.ResourceLink as its name and URI,
// readable in place of the memory-address dump the default %v branch used
// to produce (G12).
func formatResourceLink(c *mcp.ResourceLink) string {
	if c == nil {
		return ""
	}
	name := cmp.Or(c.Title, c.Name)
	if name == "" {
		return c.URI
	}
	return fmt.Sprintf("%s (%s)", name, c.URI)
}
