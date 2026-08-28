package tools

import "github.com/rave-soft/sennit/internal/proto"

// AgenticFetchToolName is the name of the agentic fetch tool.
const AgenticFetchToolName = proto.AgenticFetchToolName

// WebFetchToolName is the name of the web_fetch tool.
const WebFetchToolName = "web_fetch"

// WebSearchToolName is the name of the web_search tool for sub-agents.
const WebSearchToolName = "web_search"

// LargeContentThreshold is the size threshold for saving content to a file.
const LargeContentThreshold = 50000 // 50KB

// AgenticFetchParams defines the parameters for the agentic fetch tool.
type AgenticFetchParams struct {
	URL    string `json:"url,omitempty" description:"The URL to fetch content from (optional - if not provided, the agent will search the web)"`
	Prompt string `json:"prompt" description:"The prompt describing what information to find or extract"`
}

// AgenticFetchPermissionsParams is defined in proto; see the comment on
// BashPermissionsParams in bash.go.
type AgenticFetchPermissionsParams = proto.AgenticFetchPermissionsParams

// WebFetchParams defines the parameters for the web_fetch tool.
type WebFetchParams struct {
	URL string `json:"url" description:"The URL to fetch content from"`
}

// WebFetchPermissionsParams defines the permission parameters for the web_fetch tool.
type WebFetchPermissionsParams struct {
	URL string `json:"url"`
}

// WebSearchParams defines the parameters for the web_search tool.
type WebSearchParams struct {
	Query      string `json:"query" description:"The search query to find information on the web"`
	MaxResults int    `json:"max_results,omitempty" description:"Maximum number of results to return (default: 10, max: 20)"`
}

// WebSearchPermissionsParams defines the permission parameters for the web_search tool.
type WebSearchPermissionsParams struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results,omitempty"`
}

// FetchParams defines the parameters for the simple fetch tool.
type FetchParams struct {
	URL     string `json:"url" description:"The URL to fetch content from"`
	Format  string `json:"format" description:"The format to return the content in (text, markdown, or html)"`
	Timeout int    `json:"timeout,omitempty" description:"Optional timeout in seconds (max 120)"`
}

// FetchPermissionsParams is defined in proto; see the comment on
// BashPermissionsParams in bash.go.
type FetchPermissionsParams = proto.FetchPermissionsParams
