package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/PuerkitoBio/goquery"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/permission"
)

const (
	FetchToolName = "fetch"
	MaxFetchSize  = 100 * 1024 // 100KB

	// defaultFetchTimeout bounds a fetch call that didn't specify a
	// timeout, now that the http.Client itself carries none.
	defaultFetchTimeout = 30 * time.Second
)

//go:embed fetch.md.tpl
var fetchDescriptionTmpl []byte

var fetchDescriptionTpl = template.Must(
	template.New("fetchDescription").
		Parse(string(fetchDescriptionTmpl)),
)

type fetchDescriptionData struct {
	GhAvailable    bool
	MaxFetchSizeKB int
}

func fetchDescription(availability toolAvailability) string {
	return renderTemplate(fetchDescriptionTpl, fetchDescriptionData{
		GhAvailable:    availability.ghAvailable,
		MaxFetchSizeKB: MaxFetchSize / 1024,
	})
}

func NewFetchTool(permissions permission.Requester, workingDir string, client *http.Client, options ...toolAvailabilityOption) fantasy.AgentTool {
	availability := applyToolAvailability(options)
	if client == nil {
		// No client.Timeout here: it would bound the whole request
		// regardless of the caller-supplied timeout below, capping the
		// documented 120s maximum at whatever this constant said (it used
		// to be 30s, silently truncating any longer request). The
		// per-call context timeout is the only bound now; defaultFetchTimeout
		// below still keeps an unspecified timeout from hanging forever.
		client = NewHTTPClient(0)
	}

	return withToolParameterSchema(fantasy.NewAgentTool(
		FetchToolName,
		fetchDescription(availability),
		func(ctx context.Context, params FetchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.URL == "" {
				return invalidParam("url"), nil
			}

			format := params.Format
			if format != "text" && format != "markdown" && format != "html" {
				return fantasy.NewTextErrorResponse("Format must be one of: text, markdown, html (lowercase)"), nil
			}

			if !strings.HasPrefix(params.URL, "http://") && !strings.HasPrefix(params.URL, "https://") {
				return fantasy.NewTextErrorResponse("URL must start with http:// or https://"), nil
			}

			// maxFetchTimeoutSeconds is the maximum allowed timeout for fetch
			// requests (2 minutes). Validated before the permission request
			// below, like download.go already does — otherwise the model
			// sees the dialog approved and only then learns the call was
			// malformed, instead of never reaching the dialog at all.
			const maxFetchTimeoutSeconds = 120
			if params.Timeout < 0 || params.Timeout > maxFetchTimeoutSeconds {
				return fantasy.NewTextErrorResponse("timeout must be between 0 and 120 seconds"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, missingSessionID("fetching a URL")
			}

			permResp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				Path:        workingDir,
				ToolCallID:  call.ID,
				ToolName:    FetchToolName,
				Action:      "fetch",
				Description: fmt.Sprintf("Fetch content from URL: %s", params.URL),
				Params:      FetchPermissionsParams(params),
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if denied {
				return permResp, nil
			}

			// Handle timeout with context. The client itself carries no
			// Timeout (see NewFetchTool), so this is the only thing bounding
			// the request; an unspecified timeout still falls back to
			// defaultFetchTimeout rather than being allowed to hang forever.
			requestTimeout := defaultFetchTimeout
			if params.Timeout > 0 {
				requestTimeout = time.Duration(params.Timeout) * time.Second
			}
			requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
			defer cancel()

			// A malformed URL or an unreachable target is information about
			// what the model asked for, not about this process, so both
			// come back as a normal tool result the model can react to
			// (e.g. by trying a different URL) — matching web_fetch's
			// handling of the same failure.
			req, err := http.NewRequestWithContext(requestCtx, "GET", params.URL, nil)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to create request: %s", err)), nil
			}

			req.Header.Set("User-Agent", brand.Slug+"/1.0")

			resp, err := client.Do(req)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to fetch URL: %s", err)), nil
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Request failed with status code: %d", resp.StatusCode)), nil
			}

			// Read one byte past the cap so a body that exactly fills it
			// can be told apart from one that was genuinely cut short:
			// truncated is recorded off the RAW read, before any
			// text/markdown conversion shrinks content well under
			// MaxFetchSize and would otherwise make the truncation notice
			// below never fire (a 300KB page cut to 100KB then converted
			// to ~30KB of markdown must still be reported as incomplete).
			body, err := io.ReadAll(io.LimitReader(resp.Body, MaxFetchSize+1))
			if err != nil {
				return fantasy.NewTextErrorResponse("Failed to read response body: " + err.Error()), nil
			}
			truncated := len(body) > MaxFetchSize
			if truncated {
				body = body[:MaxFetchSize]
			}

			// The size cap cuts at a byte offset, which lands mid-rune on
			// any page whose multi-byte character happens to straddle it —
			// see dropTrailingPartialRune.
			content := string(dropTrailingPartialRune(body))

			validUTF8 := utf8.ValidString(content)
			if !validUTF8 {
				return fantasy.NewTextErrorResponse("Response content is not valid UTF-8"), nil
			}
			contentType := resp.Header.Get("Content-Type")

			switch format {
			case "text":
				if strings.Contains(contentType, "text/html") {
					text, err := extractTextFromHTML(content)
					if err != nil {
						return fantasy.NewTextErrorResponse("Failed to extract text from HTML: " + err.Error()), nil
					}
					content = text
				}

			case "markdown":
				if strings.Contains(contentType, "text/html") {
					markdown, err := ConvertHTMLToMarkdown(content)
					if err != nil {
						return fantasy.NewTextErrorResponse("Failed to convert HTML to Markdown: " + err.Error()), nil
					}
					content = markdown
				}

				content = "```\n" + content + "\n```"

			case "html":
				// return only the body of the HTML document
				if strings.Contains(contentType, "text/html") {
					doc, err := goquery.NewDocumentFromReader(strings.NewReader(content))
					if err != nil {
						return fantasy.NewTextErrorResponse("Failed to parse HTML: " + err.Error()), nil
					}
					body, err := doc.Find("body").Html()
					if err != nil {
						return fantasy.NewTextErrorResponse("Failed to extract body from HTML: " + err.Error()), nil
					}
					if body == "" {
						return fantasy.NewTextErrorResponse("No body content found in HTML"), nil
					}
					content = "<html>\n<body>\n" + body + "\n</body>\n</html>"
				}
			}
			// Report truncation off the raw-read flag, not off the
			// (possibly since-shrunk-by-conversion) content length - see
			// the comment on truncated's assignment above.
			if truncated {
				content += fmt.Sprintf("\n\n[Content truncated to %d bytes]", MaxFetchSize)
			}

			return fantasy.NewTextResponse(content), nil
		},
	), map[string]toolParameterSchema{"url": {minLength: intPtr(1)}, "format": {enum: []string{"text", "markdown", "html"}}, "timeout": intSchemaBounds(120)})
}

// truncateToRuneBoundary truncates s to at most n bytes, backing off to the
// nearest earlier rune boundary so a multi-byte UTF-8 rune straddling the
// cut point is dropped whole rather than split into an invalid trailing
// fragment. This is the primitive both the fetch tool (here, for its raw
// byte cap) and the Tavily search backend (search_backend.go, for its
// per-result content budget) need; search_backend.go used to carry its own
// near-identical truncateUTF8 that also appended a truncation marker, but
// the marker is caller-specific — fetch.go adds its own byte-count message,
// and the Tavily caller now adds its own "content truncated" marker inline —
// so only the shared cutting logic lives here.
func truncateToRuneBoundary(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// extractTextFromHTML pulls the plain text of the document body, used for
// the fetch tool's "text" format.
func extractTextFromHTML(html string) (string, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return "", err
	}

	text := doc.Find("body").Text()
	text = strings.Join(strings.Fields(text), " ")

	return text, nil
}
