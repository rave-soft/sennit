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

func fetchDescription() string {
	return renderTemplate(fetchDescriptionTpl, fetchDescriptionData{
		GhAvailable:    ghAvailable,
		MaxFetchSizeKB: MaxFetchSize / 1024,
	})
}

func NewFetchTool(permissions permission.Service, workingDir string, client *http.Client) fantasy.AgentTool {
	if client == nil {
		client = newHTTPClient(30 * time.Second)
	}

	return withToolParameterSchema(fantasy.NewParallelAgentTool(
		FetchToolName,
		fetchDescription(),
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

			// maxFetchTimeoutSeconds is the maximum allowed timeout for fetch requests (2 minutes)
			const maxFetchTimeoutSeconds = 120
			if params.Timeout < 0 || params.Timeout > maxFetchTimeoutSeconds {
				return fantasy.NewTextErrorResponse("timeout must be between 0 and 120 seconds"), nil
			}

			// Handle timeout with context
			requestCtx := ctx
			if params.Timeout > 0 {
				var cancel context.CancelFunc
				requestCtx, cancel = context.WithTimeout(ctx, time.Duration(params.Timeout)*time.Second)
				defer cancel()
			}

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

			body, err := io.ReadAll(io.LimitReader(resp.Body, MaxFetchSize))
			if err != nil {
				return fantasy.NewTextErrorResponse("Failed to read response body: " + err.Error()), nil
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
			// truncate content if it exceeds max read size
			if int64(len(content)) >= MaxFetchSize {
				content = truncateToRuneBoundary(content, MaxFetchSize)
				content += fmt.Sprintf("\n\n[Content truncated to %d bytes]", MaxFetchSize)
			}

			return fantasy.NewTextResponse(content), nil
		},
	), map[string]toolParameterSchema{"url": {minLength: intPtr(1)}, "format": {enum: []string{"text", "markdown", "html"}}, "timeout": intSchemaBounds(0, 120)})
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
