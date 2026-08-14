package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/invopop/jsonschema"
	"github.com/rave-soft/braid/internal/csync"
	"github.com/rave-soft/braid/internal/oauth"
	"github.com/rave-soft/braid/internal/oauth/copilot"
)

const (
	appName              = "braid"
	defaultDataDirectory = ".braid"
	defaultInitializeAs  = "AGENTS.md"
)

// defaultContextPaths lists the project files Braid loads as context
// automatically, without any opt-in. Braid only reads its own conventions
// here (braid.md/AGENTS.md and casing/local variants); files belonging to
// other tools (CLAUDE.md, .cursorrules, .github/copilot-instructions.md,
// etc.) are not auto-loaded — add them explicitly via options.context_paths
// (braidrc: `option context-path CLAUDE.md`) if you want Braid to read them.
var defaultContextPaths = []string{
	"braid.md",
	"braid.local.md",
	"Braid.md",
	"Braid.local.md",
	"BRAID.md",
	"BRAID.local.md",
	"AGENTS.md",
	"agents.md",
	"Agents.md",
}

const (
	AgentCoder string = "coder"
	AgentTask  string = "task"
)

type SelectedModel struct {
	// The model id as used by the provider API.
	// Required.
	Model string `json:"model" jsonschema:"required,description=The model ID as used by the provider API,example=gpt-4o"`
	// The model provider, same as the key/id used in the providers config.
	// Required.
	Provider string `json:"provider" jsonschema:"required,description=The model provider ID that matches a key in the providers config,example=openai"`

	// Only used by models that use the openai provider and need this set.
	ReasoningEffort string `json:"reasoning_effort,omitempty" jsonschema:"description=Reasoning effort level for OpenAI models that support it,enum=low,enum=medium,enum=high"`

	// Used by anthropic models that can reason to indicate if the model should think.
	Think bool `json:"think,omitempty" jsonschema:"description=Enable thinking mode for Anthropic models that support reasoning"`

	// Overrides the default model configuration.
	MaxTokens        int64    `json:"max_tokens,omitempty" jsonschema:"description=Maximum number of tokens for model responses,maximum=200000,example=4096"`
	Temperature      *float64 `json:"temperature,omitempty" jsonschema:"description=Sampling temperature,minimum=0,maximum=1,example=0.7"`
	TopP             *float64 `json:"top_p,omitempty" jsonschema:"description=Top-p (nucleus) sampling parameter,minimum=0,maximum=1,example=0.9"`
	TopK             *int64   `json:"top_k,omitempty" jsonschema:"description=Top-k sampling parameter"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty" jsonschema:"description=Frequency penalty to reduce repetition"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty" jsonschema:"description=Presence penalty to increase topic diversity"`

	// Override provider specific options.
	ProviderOptions map[string]any `json:"provider_options,omitempty" jsonschema:"description=Additional provider-specific options for the model"`
}

type ProviderConfig struct {
	// The provider's id.
	ID string `json:"id,omitempty" jsonschema:"description=Unique identifier for the provider,example=openai"`
	// The provider's name, used for display purposes.
	Name string `json:"name,omitempty" jsonschema:"description=Human-readable name for the provider,example=OpenAI"`
	// The provider's API endpoint.
	BaseURL string `json:"base_url,omitempty" jsonschema:"description=Base URL for the provider's API,format=uri,example=https://api.openai.com/v1"`
	// The provider's proxy URL (http/https/socks5). The value runs
	// through shell expansion at config-load time, the same as api_key
	// and extra_headers, so $VAR and $(cmd) work. Empty means no
	// per-provider proxy override — requests fall back to the standard
	// HTTP_PROXY/HTTPS_PROXY/NO_PROXY environment variables via net/http's
	// default proxy resolution.
	ProxyURL string `json:"proxy_url,omitempty" jsonschema:"description=Proxy URL for requests to this provider (http/https/socks5); set to \"none\" to force a direct connection even if HTTP_PROXY/HTTPS_PROXY are set in the environment,example=http://localhost:8080"`
	// The provider type, e.g. "openai", "anthropic", etc. if empty it defaults to openai.
	Type catwalk.Type `json:"type,omitempty" jsonschema:"description=Provider type that determines the API format,default=openai"`
	// The provider's API key.
	APIKey string `json:"api_key,omitempty" jsonschema:"description=API key for authentication with the provider,example=$OPENAI_API_KEY"`
	// The original API key template before resolution (for re-resolution on auth errors).
	APIKeyTemplate string `json:"-"`
	// OAuthToken for providers that use OAuth2 authentication.
	OAuthToken *oauth.Token `json:"oauth,omitempty" jsonschema:"description=OAuth2 token for authentication with the provider"`
	// Marks the provider as disabled.
	Disable bool `json:"disable,omitempty" jsonschema:"description=Whether this provider is disabled,default=false"`

	// Custom system prompt prefix.
	SystemPromptPrefix string `json:"system_prompt_prefix,omitempty" jsonschema:"description=Custom prefix to add to system prompts for this provider"`

	// Extra headers to send with each request to the provider. Values
	// run through shell expansion at config-load time, so $VAR and
	// $(cmd) work the same way they do in MCP headers. A header whose
	// value resolves to the empty string (unset bare $VAR under
	// lenient nounset, $(echo), or literal "") is omitted from the
	// outgoing request rather than sent as "Header:".
	ExtraHeaders map[string]string `json:"extra_headers,omitempty" jsonschema:"description=Additional HTTP headers to send with requests"`
	// ExtraBody is merged verbatim into OpenAI-compatible request
	// bodies. String values are NOT shell-expanded: this is a plain
	// JSON passthrough so that arbitrary provider-extension fields
	// (numbers, nested objects, booleans) round-trip without a
	// recursive walker guessing at intent. If you need an env-var-
	// driven value at request time, put it in extra_headers, or in
	// the provider's top-level api_key / base_url, all of which do
	// expand.
	ExtraBody map[string]any `json:"extra_body,omitempty" jsonschema:"description=Additional fields to include in request bodies\\, only works with openai-compatible providers"`

	ProviderOptions map[string]any `json:"provider_options,omitempty" jsonschema:"description=Additional provider-specific options for this provider"`

	// Used to pass extra parameters to the provider.
	ExtraParams map[string]string `json:"-"`

	// AWSAuthRefresh is a shell command run when Bedrock returns a
	// credential error. Output is discarded to avoid corrupting the TUI.
	AWSAuthRefresh string `json:"aws_auth_refresh,omitempty" jsonschema:"description=Shell command to run when AWS credentials expire (Bedrock only)."`

	// Skip cost accumulation for this provider when using subscription or flat rate billing.
	FlatRate bool `json:"flat_rate,omitempty" jsonschema:"description=Flat-rate mode for this provider"`

	// AutoDiscoverModels controls model discovery via /v1/models endpoint.
	// When Models is empty and this is nil or true, Braid auto-discovers
	// models. When true and Models is non-empty, discovered models are
	// merged in (user-specified models take precedence). When false,
	// only explicitly listed models are used.
	AutoDiscoverModels *bool `json:"discover_models,omitempty" jsonschema:"description=Auto-discover models from /v1/models endpoint. When true with existing models they are merged (yours win),default=true"`

	// The provider models
	Models []catwalk.Model `json:"models,omitempty" jsonschema:"description=List of models available from this provider"`

	// ModelsSource records where Models came from for this load: the
	// user's own config, or the global model-discovery cache (see
	// internal/config/modelcache.go). It is in-memory bookkeeping only,
	// never serialized — set by resolveCustomProviderModels/
	// validateCustomProviders in load.go, and read by `braid models
	// refresh` (internal/cmd/models.go) to refuse silently overwriting a
	// manually curated list with (possibly junk) discovery output.
	ModelsSource ModelsSource `json:"-"`
}

// ModelsSource identifies where a custom provider's Models list came from.
type ModelsSource string

const (
	// ModelsSourceConfig means Models was written by hand in braidrc/
	// braid.json — refresh must never overwrite it silently.
	ModelsSourceConfig ModelsSource = "config"
	// ModelsSourceCache means Models came from discovery, either just now
	// or from a previous load via the global model-discovery cache.
	ModelsSourceCache ModelsSource = "cache"
)

// ToProvider converts the [ProviderConfig] to a [catwalk.Provider].
func (c *ProviderConfig) ToProvider() catwalk.Provider {
	// Convert config provider to provider.Provider format
	provider := catwalk.Provider{
		Name:   c.Name,
		ID:     catwalk.InferenceProvider(c.ID),
		Models: make([]catwalk.Model, len(c.Models)),
	}

	// Convert models
	for i, model := range c.Models {
		provider.Models[i] = catwalk.Model{
			ID:                     model.ID,
			Name:                   model.Name,
			CostPer1MIn:            model.CostPer1MIn,
			CostPer1MOut:           model.CostPer1MOut,
			CostPer1MInCached:      model.CostPer1MInCached,
			CostPer1MOutCached:     model.CostPer1MOutCached,
			ContextWindow:          model.ContextWindow,
			DefaultMaxTokens:       model.DefaultMaxTokens,
			CanReason:              model.CanReason,
			ReasoningLevels:        model.ReasoningLevels,
			DefaultReasoningEffort: model.DefaultReasoningEffort,
			SupportsImages:         model.SupportsImages,
		}
	}

	return provider
}

// SetupGitHubCopilot adds the headers Copilot requires to the provider.
//
// The map is created when absent: a provider declared without extra_headers
// decodes with a nil map, and copying into a nil map panics. That is reachable
// from the OAuth refresh path, where a Copilot entry holding only a token would
// take down the process.
func (c *ProviderConfig) SetupGitHubCopilot() {
	if c.ExtraHeaders == nil {
		c.ExtraHeaders = make(map[string]string)
	}
	maps.Copy(c.ExtraHeaders, copilot.Headers())
}

type MCPType string

const (
	MCPStdio MCPType = "stdio"
	MCPSSE   MCPType = "sse"
	MCPHttp  MCPType = "http"
)

type MCPConfig struct {
	Command       string            `json:"command,omitempty" jsonschema:"description=Command to execute for stdio MCP servers,example=npx"`
	Env           map[string]string `json:"env,omitempty" jsonschema:"description=Environment variables to set for the MCP server"`
	Args          []string          `json:"args,omitempty" jsonschema:"description=Arguments to pass to the MCP server command"`
	Type          MCPType           `json:"type" jsonschema:"required,description=Type of MCP connection,enum=stdio,enum=sse,enum=http,default=stdio"`
	URL           string            `json:"url,omitempty" jsonschema:"description=URL for HTTP or SSE MCP servers,format=uri,example=http://localhost:3000/mcp"`
	Disabled      bool              `json:"disabled,omitempty" jsonschema:"description=Whether this MCP server is disabled,default=false"`
	DisabledTools []string          `json:"disabled_tools,omitempty" jsonschema:"description=List of tools from this MCP server to disable,example=get-library-doc"`
	EnabledTools  []string          `json:"enabled_tools,omitempty" jsonschema:"description=Allow list of tools from this MCP server,example=get-library-doc"`
	Timeout       int               `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds for MCP server connections,default=10,example=30,example=60,example=120"`

	// Headers are HTTP headers for HTTP/SSE MCP servers. Values run
	// through shell expansion at MCP startup, so $VAR and $(cmd)
	// work. A header whose value resolves to the empty string (unset
	// bare $VAR under lenient nounset, $(echo), or literal "") is
	// omitted from the outgoing request rather than sent as
	// "Header:".
	Headers map[string]string `json:"headers,omitempty" jsonschema:"description=HTTP headers for HTTP/SSE MCP servers"`

	// OAuth enables the MCP OAuth 2.1 authorization flow for HTTP
	// transport servers. When true, the client uses dynamic client
	// registration and opens a browser for the user to authorize.
	// Tokens are persisted automatically. Only supported for type=http.
	OAuth bool `json:"oauth,omitempty" jsonschema:"description=Enable OAuth 2.1 authorization flow for this MCP server (HTTP transport only),default=false"`

	// OAuthClientID is an optional pre-registered OAuth client ID. Set
	// it for servers that do not support dynamic client registration
	// (e.g. GitHub, Slack) and instead issue client credentials when you
	// register an OAuth app. Values run through shell expansion, so
	// $VAR and $(cmd) work.
	OAuthClientID string `json:"oauth_client_id,omitempty" jsonschema:"description=Pre-registered OAuth client ID for servers without dynamic client registration"`

	// OAuthClientSecret is the optional secret paired with
	// OAuthClientID for confidential clients. Values run through shell
	// expansion, so $VAR and $(cmd) work.
	OAuthClientSecret string `json:"oauth_client_secret,omitempty" jsonschema:"description=Pre-registered OAuth client secret paired with oauth_client_id"`

	// OAuthCallbackPort pins the localhost port used for the OAuth
	// redirect listener. Set this when the OAuth provider requires an
	// exact-match callback URL (e.g. GitHub OAuth Apps). When omitted,
	// Braid picks the first free port from its default range.
	OAuthCallbackPort int `json:"oauth_callback_port,omitempty" jsonschema:"description=Fixed localhost port for the OAuth callback, required by providers that enforce exact-match redirect URIs"`

	// OAuthToken is the persisted OAuth token for this server. It is
	// managed internally and stored in the global data config.
	OAuthToken *oauth.Token `json:"oauth_token,omitempty" jsonschema:"-"`
}

// isOrphanedToken reports whether this entry is a leftover OAuth token
// with no real server config.
func (m MCPConfig) isOrphanedToken() bool {
	return m.Type == "" && m.Command == "" && m.URL == "" && m.OAuthToken != nil
}

type LSPConfig struct {
	Disabled    bool              `json:"disabled,omitempty" jsonschema:"description=Whether this LSP server is disabled,default=false"`
	Command     string            `json:"command,omitempty" jsonschema:"description=Command to execute for the LSP server,example=gopls"`
	Args        []string          `json:"args,omitempty" jsonschema:"description=Arguments to pass to the LSP server command"`
	Env         map[string]string `json:"env,omitempty" jsonschema:"description=Environment variables to set to the LSP server command"`
	FileTypes   []string          `json:"filetypes,omitempty" jsonschema:"description=File types this LSP server handles,example=go,example=mod,example=rs,example=c,example=js,example=ts"`
	RootMarkers []string          `json:"root_markers,omitempty" jsonschema:"description=Files or directories that indicate the project root,example=go.mod,example=package.json,example=Cargo.toml"`
	InitOptions map[string]any    `json:"init_options,omitempty" jsonschema:"description=Initialization options passed to the LSP server during initialize request"`
	Options     map[string]any    `json:"options,omitempty" jsonschema:"description=LSP server-specific settings passed during initialization"`
	Timeout     int               `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds for LSP server initialization,default=30,example=60,example=120"`
}

type TUIOptions struct {
	CompactMode bool   `json:"compact_mode,omitempty" jsonschema:"description=Enable compact mode for the TUI interface,default=false"`
	DiffMode    string `json:"diff_mode,omitempty" jsonschema:"description=Diff mode for the TUI interface,enum=unified,enum=split"`
	// Here we can add themes later or any TUI related options
	//

	Completions Completions         `json:"completions,omitzero" jsonschema:"description=Completions UI options"`
	Transparent *bool               `json:"transparent,omitempty" jsonschema:"description=Enable transparent background for the TUI interface,default=false"`
	Scrollbar   string              `json:"scrollbar,omitempty" jsonschema:"description=Chat scrollbar visibility,enum=default,enum=always,enum=never,default=default"`
	Keybindings map[string][]string `json:"keybindings,omitempty" jsonschema:"description=Keyboard shortcuts keyed by action name. Each value replaces the default shortcuts for that action"`
}

// Completions defines options for the completions UI.
type Completions struct {
	MaxDepth *int `json:"max_depth,omitempty" jsonschema:"description=Maximum depth for the ls tool,default=0,example=10"`
	MaxItems *int `json:"max_items,omitempty" jsonschema:"description=Maximum number of items to return for the ls tool,default=1000,example=100"`
}

func (c Completions) Limits() (depth, items int) {
	return ptrValOr(c.MaxDepth, 0), ptrValOr(c.MaxItems, 0)
}

// Scrollbar visibility options.
const (
	ScrollbarDefault = "default" // Auto-hide after 2 seconds
	ScrollbarAlways  = "always"  // Always show when content exceeds viewport
	ScrollbarNever   = "never"   // Never show scrollbar
)

type Permissions struct {
	AllowedTools []string `json:"allowed_tools,omitempty" jsonschema:"description=List of tools that don't require permission prompts,example=bash,example=view"`
	// Bypass, when true, skips every permission prompt from process start —
	// equivalent to always-on yolo mode. It is the persistent counterpart to
	// the session-only --yolo flag / ctrl+y toggle (see permission.Service.
	// SetSkipRequests), which continue to work as a runtime override on top
	// of this.
	Bypass bool `json:"bypass,omitempty" jsonschema:"description=DANGEROUS: skip every permission prompt from startup — the agent runs every tool without asking. Equivalent to always-on yolo mode.,default=false"`
}

type TrailerStyle string

const (
	TrailerStyleNone         TrailerStyle = "none"
	TrailerStyleCoAuthoredBy TrailerStyle = "co-authored-by"
	TrailerStyleAssistedBy   TrailerStyle = "assisted-by"
)

type Attribution struct {
	TrailerStyle  TrailerStyle `json:"trailer_style,omitempty" jsonschema:"description=Style of attribution trailer to add to commits,enum=none,enum=co-authored-by,enum=assisted-by,default=assisted-by"`
	CoAuthoredBy  *bool        `json:"co_authored_by,omitempty" jsonschema:"description=Deprecated: use trailer_style instead"`
	GeneratedWith bool         `json:"generated_with,omitempty" jsonschema:"description=Add Generated with Braid line to commit messages and issues and PRs,default=true"`
}

// JSONSchemaExtend marks the co_authored_by field as deprecated in the schema.
func (Attribution) JSONSchemaExtend(schema *jsonschema.Schema) {
	if schema.Properties != nil {
		if prop, ok := schema.Properties.Get("co_authored_by"); ok {
			prop.Deprecated = true
		}
	}
}

type Options struct {
	ContextPaths         []string    `json:"context_paths,omitempty" jsonschema:"description=Paths to files containing context information for the AI. Braid auto-loads only its own conventions (AGENTS.md/BRAID.md and casing/local variants); list other tools' files here explicitly (e.g. CLAUDE.md) to have Braid read them too,example=BRAID.md,example=CLAUDE.md"`
	GlobalContextPaths   []string    `json:"global_context_paths,omitempty" jsonschema:"description=Paths to files containing global context information for the AI,default=~/.config/braid/BRAID.md,default=~/.config/AGENTS.md"`
	SkillsPaths          []string    `json:"skills_paths,omitempty" jsonschema:"description=Paths to directories containing Agent Skills (folders with SKILL.md files),example=~/.config/braid/skills,example=./skills"`
	TUI                  *TUIOptions `json:"tui,omitempty" jsonschema:"description=Terminal user interface options"`
	Debug                bool        `json:"debug,omitempty" jsonschema:"description=Enable debug logging,default=false"`
	DebugLSP             bool        `json:"debug_lsp,omitempty" jsonschema:"description=Enable debug logging for LSP servers,default=false"`
	DisableAutoSummarize bool        `json:"disable_auto_summarize,omitempty" jsonschema:"description=Disable automatic conversation summarization,default=false"`
	// DataDirectory is a project-local directory (".braid" by default) for
	// workspace-scoped state that is NOT part of the shared global database:
	// the single-instance lock file, workspace config overrides, and (until
	// imported) a pre-shared-database project's legacy braid.db. Session and
	// message history now live in the single global database (see
	// config.GlobalDBDir()), not here. Relative paths are resolved against
	// the working directory; absolute paths are used verbatim. After
	// defaulting the stored value is always absolute.
	DataDirectory           string            `json:"data_directory,omitempty" jsonschema:"description=Project-local directory for workspace-scoped state (lock file\\, config overrides\\, legacy pre-migration database) — not the shared session/message database. Relative paths are resolved against the working directory; absolute paths are used as-is.,default=.braid,example=.braid"`
	DisabledTools           []string          `json:"disabled_tools,omitempty" jsonschema:"description=List of built-in tools to disable and hide from the agent,example=bash,example=web_search"`
	DisableDefaultProviders bool              `json:"disable_default_providers,omitempty" jsonschema:"description=Ignore all default/embedded providers. When enabled\\, providers must be fully specified in the config file with base_url\\, models\\, and api_key - no merging with defaults occurs,default=false"`
	Attribution             *Attribution      `json:"attribution,omitempty" jsonschema:"description=Attribution settings for generated content"`
	DisableMetrics          bool              `json:"disable_metrics,omitempty" jsonschema:"description=Disable sending metrics,default=false"`
	InitializeAs            string            `json:"initialize_as,omitempty" jsonschema:"description=Name of the context file to create/update during project initialization,default=AGENTS.md,example=AGENTS.md,example=BRAID.md,example=CLAUDE.md,example=docs/LLMs.md"`
	AutoLSP                 *bool             `json:"auto_lsp,omitempty" jsonschema:"description=Automatically setup LSPs based on root markers,default=true"`
	Progress                *bool             `json:"progress,omitempty" jsonschema:"description=Show indeterminate progress updates during long operations,default=true"`
	Notifications           string            `json:"notifications,omitempty" jsonschema:"description=Notification style to use. Options: auto (default)\\, native\\, osc\\, bell\\, disabled. Auto selects based on environment: native for local sessions\\, osc for SSH (with automatic OSC 99/777 detection).,enum=auto,enum=native,enum=osc,enum=bell,enum=disabled,default=auto"`
	DisabledSkills          []string          `json:"disabled_skills,omitempty" jsonschema:"description=List of skill names to disable and hide from the agent,example=braid-config"`
	WebSearch               *WebSearchOptions `json:"web_search,omitempty" jsonschema:"description=Web search backend configuration. Defaults to the keyless DuckDuckGo scraper when omitted."`
	Threads                 *ThreadsOptions   `json:"threads,omitempty" jsonschema:"description=Threads (parallel agent work stream) configuration."`
	// HistoryRetentionDays is read by `braid gc`, not enforced automatically:
	// nothing purges history on its own. A pointer distinguishes "unset"
	// (defaults to 90) from an explicit 0, which means keep history forever.
	HistoryRetentionDays *int `json:"history_retention_days,omitempty" jsonschema:"description=Age in days after which \"braid gc\" deletes sessions (and their messages/files) and finished threads. 0 keeps history forever. Not enforced automatically — run \"braid gc\" (e.g. from cron) to apply it.,default=90,example=30,example=180"`
}

// ThreadsOptions configures the threads feature (parallel agent work
// streams running in isolated git worktrees).
type ThreadsOptions struct {
	// WorktreeDir is the parent directory under which each thread's git
	// worktree is created (at <worktree_dir>/<thread-name>). A relative
	// path is resolved against the parent of the repository root (the
	// same directory the default sibling "<repo>-threads" lives in), not
	// against the working directory. Absolute paths are used as-is.
	// Defaults to a "<repo>-threads" sibling of the repository root.
	WorktreeDir string `json:"worktree_dir,omitempty" jsonschema:"description=Parent directory for thread worktrees (<worktree_dir>/<thread-name>). A relative path resolves against the parent of the repository root; an absolute path is used as-is. Defaults to a \"<repo>-threads\" sibling of the repository root.,example=/var/tmp/braid-threads,example=../thread-worktrees"`
}

// WebSearchOptions configures the backend used by the web_search tool.
// Provider defaults to "duckduckgo" (no API key required) when unset.
type WebSearchOptions struct {
	// Provider selects the search backend. "duckduckgo" (default) scrapes
	// DuckDuckGo Lite and needs no API key; "tavily" calls the Tavily
	// Search API and requires APIKey.
	Provider string `json:"provider,omitempty" jsonschema:"description=Web search backend to use,enum=duckduckgo,enum=tavily,default=duckduckgo"`
	// APIKey authenticates with the selected provider. Runs through shell
	// expansion at tool-build time, the same as provider api_key, so $VAR
	// and $(cmd) work. Unused by the duckduckgo provider.
	APIKey string `json:"api_key,omitempty" jsonschema:"description=API key for the web search provider (shell-expanded)\\, e.g. $TAVILY_API_KEY. Not used by the duckduckgo provider.,example=$TAVILY_API_KEY"`
	// BaseURL overrides the provider's default API endpoint, for
	// self-hosted or proxy-compatible search APIs.
	BaseURL string `json:"base_url,omitempty" jsonschema:"description=Override the search provider's default API endpoint (self-hosted or proxy-compatible deployments),example=https://api.tavily.com/search"`
	// ProxyURL routes web search requests through a proxy. Web search has
	// no LLM provider of its own to inherit proxy_url from, so it is
	// configured here; same semantics as ProviderConfig.ProxyURL,
	// including the "none" sentinel and shell expansion.
	ProxyURL string `json:"proxy_url,omitempty" jsonschema:"description=Proxy URL for web search requests (http/https/socks5); set to \"none\" to force a direct connection even if HTTP_PROXY/HTTPS_PROXY are set in the environment,example=http://localhost:8080"`
}

type MCPs map[string]MCPConfig

type MCP struct {
	Name string    `json:"name"`
	MCP  MCPConfig `json:"mcp"`
}

func (m MCPs) Sorted() []MCP {
	sorted := make([]MCP, 0, len(m))
	for k, v := range m {
		sorted = append(sorted, MCP{
			Name: k,
			MCP:  v,
		})
	}
	slices.SortFunc(sorted, func(a, b MCP) int {
		return strings.Compare(a.Name, b.Name)
	})
	return sorted
}

type LSPs map[string]LSPConfig

type LSP struct {
	Name string    `json:"name"`
	LSP  LSPConfig `json:"lsp"`
}

func (l LSPs) Sorted() []LSP {
	sorted := make([]LSP, 0, len(l))
	for k, v := range l {
		sorted = append(sorted, LSP{
			Name: k,
			LSP:  v,
		})
	}
	slices.SortFunc(sorted, func(a, b LSP) int {
		return strings.Compare(a.Name, b.Name)
	})
	return sorted
}

// ResolvedEnv returns m.Env with every value expanded through the
// given resolver. The returned slice is of the form "KEY=value" sorted
// by key so callers get deterministic output; the receiver's Env map is
// not mutated. On the first resolution failure it returns nil and an
// error that identifies the offending key; the inner resolver error is
// already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work. Callers are expected to surface it
// (for MCP, via StateError on the status card) rather than silently
// spawn the server with an empty credential.
//
// The resolver choice matters: in server mode pass the shell resolver
// so $VAR / $(cmd) expand; in client mode pass IdentityResolver so the
// template is forwarded verbatim and expansion happens on the server.
func (m MCPConfig) ResolvedEnv(r VariableResolver) ([]string, error) {
	return resolveEnvs(m.Env, r)
}

// ResolvedArgs returns m.Args with every element expanded through the
// given resolver. A fresh slice is allocated; m.Args is never mutated.
// On the first resolution failure it returns nil and an error
// identifying the offending positional index; the inner resolver error
// is already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work.
//
// See ResolvedEnv for guidance on picking a resolver.
func (m MCPConfig) ResolvedArgs(r VariableResolver) ([]string, error) {
	if len(m.Args) == 0 {
		return nil, nil
	}
	out := make([]string, len(m.Args))
	for i, a := range m.Args {
		v, err := r.ResolveValue(a)
		if err != nil {
			return nil, fmt.Errorf("arg %d: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

// ResolvedURL returns m.URL expanded through the given resolver. The
// receiver is not mutated. Errors from the resolver are already
// sanitized by ResolveValue and are wrapped with %w for errors.Is/As.
//
// URLs run through the same shell-expansion pipeline as the other
// fields, so a literal '$' (e.g. OData query strings containing
// $filter/$select) must be escaped as '\$' or '${DOLLAR:-$}' to avoid
// being interpreted as a variable reference. Same constraint already
// applies to command, args, env, and headers.
//
// See ResolvedEnv for guidance on picking a resolver.
func (m MCPConfig) ResolvedURL(r VariableResolver) (string, error) {
	if m.URL == "" {
		return "", nil
	}
	v, err := r.ResolveValue(m.URL)
	if err != nil {
		return "", fmt.Errorf("url: %w", err)
	}
	return v, nil
}

// ResolvedHeaders returns m.Headers with every value expanded through
// the given resolver. A fresh map is allocated; m.Headers is never
// mutated. On the first resolution failure it returns nil and an error
// identifying the offending header name; the inner resolver error is
// already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work.
//
// A header whose value resolves to the empty string (unset bare $VAR
// under lenient nounset, $(echo), or literal "") is omitted from the
// returned map — sending "X-Auth:" with an empty value is rejected by
// some providers and the user's intent in "optional, env-gated
// header" is clearly "absent when the var isn't set."
//
// See ResolvedEnv for guidance on picking a resolver.
func (m MCPConfig) ResolvedHeaders(r VariableResolver) (map[string]string, error) {
	if len(m.Headers) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(m.Headers))
	// Sort keys so failures are reported deterministically when more
	// than one header would fail.
	keys := make([]string, 0, len(m.Headers))
	for k := range m.Headers {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v, err := r.ResolveValue(m.Headers[k])
		if err != nil {
			return nil, fmt.Errorf("header %s: %w", k, err)
		}
		if v == "" {
			continue
		}
		out[k] = v
	}
	return out, nil
}

// ResolvedArgs returns l.Args with every element expanded through the
// given resolver. A fresh slice is allocated; l.Args is never mutated.
// On the first resolution failure it returns nil and an error
// identifying the offending positional index; the inner resolver error
// is already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work.
//
// Empty resolved values are kept (a deliberate "empty positional arg"
// like --flag "" is sometimes valid), matching MCPConfig.ResolvedArgs.
//
// The resolver choice matters: in server mode pass the shell resolver
// so $VAR / $(cmd) expand; in client mode pass IdentityResolver so the
// template is forwarded verbatim.
func (l LSPConfig) ResolvedArgs(r VariableResolver) ([]string, error) {
	if len(l.Args) == 0 {
		return nil, nil
	}
	out := make([]string, len(l.Args))
	for i, a := range l.Args {
		v, err := r.ResolveValue(a)
		if err != nil {
			return nil, fmt.Errorf("arg %d: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

// ResolvedEnv returns l.Env with every value expanded through the
// given resolver. A fresh map is allocated; l.Env is never mutated.
// On the first resolution failure it returns nil and an error that
// identifies the offending key; the inner resolver error is already
// sanitized by ResolveValue and is wrapped with %w so errors.Is/As
// continues to work.
//
// Empty resolved values are kept ("FOO=" is a legitimate request;
// opt out via ${VAR:+...}), matching MCPConfig.ResolvedEnv.
//
// Shape note: this returns map[string]string rather than the []string
// shape MCPConfig.ResolvedEnv uses because the consumer
// (powernap.ClientConfig.Environment in internal/lsp/client.go) takes
// a map directly — returning a []string here would only force a
// round-trip back to a map at the call site.
//
// See ResolvedArgs for guidance on picking a resolver.
func (l LSPConfig) ResolvedEnv(r VariableResolver) (map[string]string, error) {
	if len(l.Env) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(l.Env))
	// Sort keys so failures are reported deterministically when more
	// than one value would fail.
	keys := make([]string, 0, len(l.Env))
	for k := range l.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v, err := r.ResolveValue(l.Env[k])
		if err != nil {
			return nil, fmt.Errorf("env %q: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}

type Agent struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// This is the id of the system prompt used by the agent
	Disabled bool `json:"disabled,omitempty"`

	// Model optionally pins this agent to a specific "provider/model-id",
	// letting a sub-agent use a model other agents don't. Left empty, the
	// agent inherits the application's main model. A value that does not
	// resolve to a known provider/model falls back to the empty default
	// with a warning logged.
	Model string `json:"model,omitempty" jsonschema:"description=Specific model as 'provider/model-id'; omit to inherit the app's main model"`

	// Prompt is the system prompt for this agent. User-defined agents must
	// set it; the built-in coder and task agents leave it empty and fall back
	// to their embedded templates.
	Prompt string `json:"prompt,omitempty" jsonschema:"description=System prompt for this agent. Required for user-defined agents"`

	// ReasoningEffort overrides the selected model's effort for this agent, so
	// a cheap reviewer and a deep implementer can share one model. An effort
	// the model does not offer is ignored at call time in favour of the
	// model's own default; the enum below lists the common levels, but any
	// level the model advertises is accepted.
	ReasoningEffort string `json:"reasoning_effort,omitempty" jsonschema:"description=Reasoning effort for this agent\\, overriding the model's. Ignored if the model does not support it,enum=low,enum=medium,enum=high"`

	// The available tools for the agent
	//  if this is nil, all tools are available
	AllowedTools []string `json:"allowed_tools,omitempty"`

	// this tells us which MCPs are available for this agent
	//  if this is empty all mcps are available
	//  the string array is the list of tools from the AllowedMCP the agent has available
	//  if the string array is nil, all tools from the AllowedMCP are available
	AllowedMCP map[string][]string `json:"allowed_mcp,omitempty"`

	// Overrides the context paths for this agent
	ContextPaths []string `json:"context_paths,omitempty"`
}

type Tools struct {
	Ls   ToolLs   `json:"ls,omitzero"`
	Grep ToolGrep `json:"grep,omitzero"`
	Glob ToolGlob `json:"glob,omitzero"`
}

type ToolLs struct {
	MaxDepth *int `json:"max_depth,omitempty" jsonschema:"description=Maximum depth for the ls tool,default=0,example=10"`
	MaxItems *int `json:"max_items,omitempty" jsonschema:"description=Maximum number of items to return for the ls tool,default=1000,example=100"`
}

// Limits returns the user-defined max-depth and max-items, or their defaults.
func (t ToolLs) Limits() (depth, items int) {
	return ptrValOr(t.MaxDepth, 0), ptrValOr(t.MaxItems, 0)
}

type ToolGrep struct {
	Timeout *time.Duration `json:"timeout,omitempty" jsonschema:"description=Timeout for the grep tool call,default=5s,example=10s"`
}

// GetTimeout returns the user-defined timeout or the default.
func (t ToolGrep) GetTimeout() time.Duration {
	return ptrValOr(t.Timeout, 5*time.Second)
}

type ToolGlob struct {
	Timeout *time.Duration `json:"timeout,omitempty" jsonschema:"description=Timeout for the glob tool call,default=30s,example=10s"`
}

// GetTimeout returns the user-defined timeout or the default.
func (t ToolGlob) GetTimeout() time.Duration {
	return ptrValOr(t.Timeout, 30*time.Second)
}

// HookConfig defines a user-configured shell command that fires on a hook
// event (e.g. PreToolUse). This is a pure-data struct: matcher compilation
// is owned by hooks.Runner so a JSON round-trip, merge, or reload can't
// silently drop compiled state.
type HookConfig struct {
	// Friendly display name shown in the TUI. Falls back to Command when empty.
	Name string `json:"name,omitempty" jsonschema:"description=Friendly display name shown in the TUI for this hook"`
	// Regex pattern tested against the tool name. Empty means match all.
	Matcher string `json:"matcher,omitempty" jsonschema:"description=Regex pattern tested against the tool name. Empty means match all tools."`
	// Shell command to execute.
	Command string `json:"command" jsonschema:"required,description=Shell command to execute when the hook fires"`
	// Timeout in seconds. Default 30.
	Timeout int `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds for the hook command,default=30"`
}

// DisplayName returns the hook name for display purposes. It returns Name
// when set, otherwise falls back to Command.
func (h *HookConfig) DisplayName() string {
	if h.Name != "" {
		return h.Name
	}
	return h.Command
}

// TimeoutDuration returns the hook timeout as a time.Duration, defaulting
// to 30s.
func (h *HookConfig) TimeoutDuration() time.Duration {
	if h.Timeout <= 0 {
		return 30 * time.Second
	}
	return time.Duration(h.Timeout) * time.Second
}

// Config holds the configuration for braid.
type Config struct {
	Schema string `json:"$schema,omitempty"`

	// Model is the single model Braid uses for the session.
	Model SelectedModel `json:"model,omitzero" jsonschema:"description=The model configuration,example={\"model\":\"gpt-4o\",\"provider\":\"openai\"}"`

	// RecentModels lists recently used models, most-recent-first. Stored
	// in the data directory config.
	RecentModels []SelectedModel `json:"recent_models,omitempty" jsonschema:"-"`

	// The providers that are configured
	Providers *csync.Map[string, ProviderConfig] `json:"providers,omitempty" jsonschema:"description=AI provider configurations"`

	MCP MCPs `json:"mcp,omitempty" jsonschema:"description=Model Context Protocol server configurations"`

	LSP LSPs `json:"lsp,omitempty" jsonschema:"description=Language Server Protocol configurations"`

	Options *Options `json:"options,omitempty" jsonschema:"description=General application options"`

	Permissions *Permissions `json:"permissions,omitempty" jsonschema:"description=Permission settings for tool usage"`

	Tools Tools `json:"tools,omitzero" jsonschema:"description=Tool configurations"`

	Hooks map[string][]HookConfig `json:"hooks,omitempty" jsonschema:"description=User-defined shell commands that fire on hook events (e.g. PreToolUse)"`

	// Env is a map of environment variables set on startup.
	Env map[string]string `json:"env,omitempty" jsonschema:"description=Environment variables to set on startup"`

	// Agents holds both the built-in agents and any the user defines.
	// SetupAgents populates this from .braid/agents/*.md files (via
	// discoverMarkdownAgents) plus the two built-ins; nothing decodes user
	// agents into this field from JSON anymore (see loadFromBytes in
	// load.go), so it is hidden from the generated config schema.
	Agents map[string]Agent `json:"agents,omitempty" jsonschema:"-"`

	// workingDir is where SetupAgents looks for markdown agent definitions.
	// It is set by setDefaults, so a Config decoded from the wire (the client
	// receives one per workspace) leaves it empty and simply skips discovery —
	// by then the agents it needs are already in Agents.
	workingDir string

	// Problems accumulates config problems noticed while loading and
	// setting up agents (a provider dropped for a missing api key, an
	// agent model that fell back, ...). It is populated by addProblem at
	// the same sites that used to only slog.Warn, and read back by
	// Doctor. Not persisted to disk.
	Problems []Problem `json:"-"`

	// jsonAgentsBlockDetected records whether any loaded JSON config layer
	// contained a top-level "agents" key. loadFromBytes (load.go) strips the
	// block before unmarshaling instead of decoding it into Agents — a JSON
	// "agents" entry is no longer read at all, only .braid/agents/*.md files
	// are. SetupAgents turns this into a doctor Problem so the silent ignore
	// is visible instead of quietly no-op'ing forever.
	jsonAgentsBlockDetected bool
}

// cloneForWrite returns a copy of c that the store's typed field mutators
// may modify without racing readers of the currently published Config.
//
// Reads of a published Config take no lock beyond the pointer load, so a
// mutator must never write through the live pointer. Instead it clones,
// mutates the clone, and atomically swaps it in. The clone gives fresh
// copies of every field a typed mutator touches in place — Model (a plain
// value, copied by the struct copy above), RecentModels, MCP, and Options
// (with its nested TUI pointer). Providers is a *csync.Map (internally
// synchronized) and is shared by reference; the remaining fields are
// immutable after load from the mutators' standpoint and are likewise
// shared.
func (c *Config) cloneForWrite() *Config {
	nc := *c
	nc.RecentModels = slices.Clone(c.RecentModels)
	nc.MCP = maps.Clone(c.MCP)
	if c.Providers != nil {
		nc.Providers = csync.NewMap[string, ProviderConfig]()
		for id, provider := range c.Providers.Seq2() {
			provider.ExtraHeaders = maps.Clone(provider.ExtraHeaders)
			provider.ExtraBody = maps.Clone(provider.ExtraBody)
			provider.ProviderOptions = maps.Clone(provider.ProviderOptions)
			provider.ExtraParams = maps.Clone(provider.ExtraParams)
			if provider.OAuthToken != nil {
				token := *provider.OAuthToken
				if token.Client != nil {
					client := *token.Client
					token.Client = &client
				}
				provider.OAuthToken = &token
			}
			nc.Providers.Set(id, provider)
		}
	}
	if c.Options != nil {
		opts := *c.Options
		if c.Options.TUI != nil {
			tui := *c.Options.TUI
			tui.Keybindings = make(map[string][]string, len(c.Options.TUI.Keybindings))
			for action, keys := range c.Options.TUI.Keybindings {
				tui.Keybindings[action] = slices.Clone(keys)
			}
			opts.TUI = &tui
		}
		nc.Options = &opts
	}
	return &nc
}

// ensureTUI returns c.Options.TUI, allocating Options and TUI as needed so
// callers can assign TUI fields without nil checks.
func (c *Config) ensureTUI() *TUIOptions {
	if c.Options == nil {
		c.Options = &Options{}
	}
	if c.Options.TUI == nil {
		c.Options.TUI = &TUIOptions{}
	}
	return c.Options.TUI
}

// providersOrEmpty snapshots the configured providers for model resolution.
// c.Providers is nil in tests that build a Config by hand without going
// through Load, so this returns a nil map rather than panicking.
func (c *Config) providersOrEmpty() map[string]ProviderConfig {
	if c.Providers == nil {
		return nil
	}
	return c.Providers.Copy()
}

func (c *Config) EnabledProviders() []ProviderConfig {
	var enabled []ProviderConfig
	for p := range c.Providers.Seq() {
		if !p.Disable {
			enabled = append(enabled, p)
		}
	}
	return enabled
}

// IsConfigured  return true if at least one provider is configured
func (c *Config) IsConfigured() bool {
	return len(c.EnabledProviders()) > 0
}

func (c *Config) GetModel(provider, model string) *catwalk.Model {
	if providerConfig, ok := c.Providers.Get(provider); ok {
		for _, m := range providerConfig.Models {
			if m.ID == model {
				return &m
			}
		}
	}
	return nil
}

// GetProviderForModel returns the provider configured for c.Model.
func (c *Config) GetProviderForModel() *ProviderConfig {
	if providerConfig, ok := c.Providers.Get(c.Model.Provider); ok {
		return &providerConfig
	}
	return nil
}

// SelectedCatalogModel returns the catalog entry for c.Model, the one model
// Braid is configured to use.
func (c *Config) SelectedCatalogModel() *catwalk.Model {
	return c.GetModel(c.Model.Provider, c.Model.Model)
}

// DefaultModelForProvider resolves the default large model for a single,
// already-configured provider: its catalog DefaultLargeModelID when the
// provider is known, otherwise the first model in its configured list
// (covers custom providers and any provider outside the catalog, whose
// only models are the ones the user discovered/entered).
func (c *Config) DefaultModelForProvider(providerID string, knownProviders []catwalk.Provider) (SelectedModel, error) {
	providerConfig, ok := c.Providers.Get(providerID)
	if !ok {
		return SelectedModel{}, fmt.Errorf("provider %s is not configured", providerID)
	}

	var defaultLargeModelID string
	for _, p := range knownProviders {
		if string(p.ID) == providerID {
			defaultLargeModelID = p.DefaultLargeModelID
			break
		}
	}

	model := c.GetModel(providerID, defaultLargeModelID)
	if model == nil {
		if len(providerConfig.Models) == 0 {
			return SelectedModel{}, fmt.Errorf("provider %s has no models configured", providerID)
		}
		model = &providerConfig.Models[0]
	}

	return SelectedModel{
		Provider:        providerID,
		Model:           model.ID,
		MaxTokens:       model.DefaultMaxTokens,
		ReasoningEffort: model.DefaultReasoningEffort,
	}, nil
}

const maxRecentModels = 5

// AllToolNames returns the names of every built-in tool the agent can be
// given, in the order buildTools constructs them. It is the single source
// of truth for what a "known tool" is — used both to resolve
// disabled_tools/allowed_tools and (from internal/ui) to verify every
// tool has a rendering path.
func AllToolNames() []string {
	return allToolNames()
}

func allToolNames() []string {
	return []string{
		"agent",
		"bash",
		"braid_info",
		"braid_logs",
		"job_output",
		"job_kill",
		"download",
		"edit",
		"multiedit",
		"lsp_diagnostics",
		"lsp_references",
		"lsp_restart",
		"lsp_symbols",
		"lsp_definition",
		"lsp_call_hierarchy",
		"lsp_rename",
		"lsp_replace_symbol",
		"fetch",
		"agentic_fetch",
		"web_fetch",
		"web_search",
		"glob",
		"grep",
		"ripgrep",
		"ls",
		"question",
		"todos",
		"view",
		"write",
		"list_mcp_resources",
		"read_mcp_resource",
		"thread_create",
		"thread_list",
		"thread_status",
		"thread_send",
		"thread_wait",
		"thread_merge",
		"thread_remove",
		"task_list",
		"task_result",
		"task_cancel",
	}
}

func resolveAllowedTools(allTools []string, disabledTools []string) []string {
	if disabledTools == nil {
		return allTools
	}
	// filter out disabled tools (exclude mode)
	return filterSlice(allTools, disabledTools, false)
}

func resolveReadOnlyTools(tools []string) []string {
	// fetch, web_fetch, and web_search don't modify local state, so they're
	// read-only in the same sense as glob/grep/view; the network calls they
	// make still go through the real permission.Service like the coder's.
	readOnlyTools := []string{"glob", "grep", "ripgrep", "ls", "lsp_call_hierarchy", "lsp_definition", "lsp_symbols", "view", "fetch", "web_fetch", "web_search"}
	// filter to only include tools that are in allowedtools (include mode)
	return filterSlice(tools, readOnlyTools, true)
}

func filterSlice(data []string, mask []string, include bool) []string {
	var filtered []string
	for _, s := range data {
		// if include is true, we include items that ARE in the mask
		// if include is false, we include items that are NOT in the mask
		if include == slices.Contains(mask, s) {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

// SetupAgents discovers user-defined agents from .braid/agents/*.md, merges
// them with the two built-ins, and writes the combined set back to c.Agents.
//
// Every user-defined agent becomes a tool the coder can call to delegate work,
// named after the agent's id. Those tool names are appended to the coder's
// allowed tools so the tool filter in the agent package keeps them; the ids are
// validated first so a role can never shadow a real tool.
//
// SetupAgents is idempotent: it fully recomputes c.Agents from markdown on
// every call — it never trusts whatever was already sitting in c.Agents (a
// previous run's built-ins, or nothing at all), so repeated calls (config
// reload, workspace switch) converge on the same result instead of drifting.
func (c *Config) SetupAgents() {
	c.setupAgents(nil)
}

// SetupAgentsWithInherited configures built-in and discovered agents, using
// inherited as the lowest-priority source of user-defined agents. Project-local
// definitions in the current working directory override inherited definitions.
func (c *Config) SetupAgentsWithInherited(inherited map[string]Agent) {
	c.setupAgents(inherited)
}

func (c *Config) setupAgents(inherited map[string]Agent) {
	// SetupAgents fully recomputes c.Agents and can run more than once on
	// the same live Config (e.g. after a markdown agent file changes), so
	// drop any agent problems from a previous run before validUserAgents
	// re-adds the current ones. Otherwise a problem sticks around forever
	// even after the offending agent definition is fixed.
	c.Problems = slices.DeleteFunc(c.Problems, func(p Problem) bool { return p.Area == AreaAgent })

	if c.jsonAgentsBlockDetected {
		c.addProblem(Problem{
			Severity: SeverityWarn,
			Area:     AreaAgent,
			Subject:  "agents",
			Message:  "agents in braid.json are ignored — define agents as .braid/agents/*.md files",
			Hint:     "move each entry to .braid/agents/<name>.md (frontmatter: name, description, model, tools; body is the prompt)",
		})
	}

	allowedTools := resolveAllowedTools(allToolNames(), c.Options.DisabledTools)
	providers := c.providersOrEmpty()

	// Markdown files under .braid/agents are the only source of user-defined
	// agents. A JSON "agents" block is never read (see loadFromBytes in
	// load.go); if one was present, jsonAgentsBlockDetected records it and
	// the Problem above surfaces it instead of using it.
	c.Agents = maps.Clone(inherited)
	if c.Agents == nil {
		c.Agents = make(map[string]Agent)
	}
	maps.Copy(c.Agents, discoverMarkdownAgents(c.workingDir, providers))

	userAgents, invalid := c.validUserAgents()
	for id, reason := range invalid {
		slog.Warn("Ignoring invalid agent definition", "agent", id, "reason", reason)
	}

	// The coder delegates through one tool per user-defined agent, so those
	// names have to survive buildTools' allow-list filter. They are appended
	// after the built-in tools — sorted among themselves for a stable order —
	// so the built-in ordering callers already rely on stays untouched.
	roleTools := slices.Sorted(maps.Keys(userAgents))
	coderTools := append(slices.Clone(allowedTools), roleTools...)

	agents := map[string]Agent{
		AgentCoder: {
			ID:           AgentCoder,
			Name:         "Coder",
			Description:  "An agent that helps with executing coding tasks.",
			ContextPaths: c.Options.ContextPaths,
			AllowedTools: coderTools,
		},

		AgentTask: {
			ID:           AgentTask,
			Name:         "Task",
			Description:  "An agent that helps with searching for context and finding implementation details.",
			ContextPaths: c.Options.ContextPaths,
			AllowedTools: resolveReadOnlyTools(allowedTools),
			// NO MCPs or LSPs by default
			AllowedMCP: map[string][]string{},
		},
	}

	for id, agent := range userAgents {
		agent.ID = id
		if agent.Name == "" {
			agent.Name = id
		}
		// A nil allowed_tools means "everything the coder may use". An empty
		// list is a deliberate choice and stays empty.
		if agent.AllowedTools == nil {
			agent.AllowedTools = slices.Clone(allowedTools)
		}
		if agent.ContextPaths == nil {
			agent.ContextPaths = c.Options.ContextPaths
		}
		agents[id] = agent
	}

	c.Agents = agents
}

// UserAgents returns a copy of the configured user-defined agents.
func (c *Config) UserAgents() map[string]Agent {
	agents := make(map[string]Agent)
	for id, agent := range c.Agents {
		if id == AgentCoder || id == AgentTask {
			continue
		}
		agents[id] = cloneAgent(agent)
	}
	return agents
}

func cloneAgents(agents map[string]Agent) map[string]Agent {
	cloned := make(map[string]Agent, len(agents))
	for id, agent := range agents {
		cloned[id] = cloneAgent(agent)
	}
	return cloned
}

func cloneAgent(agent Agent) Agent {
	agent.AllowedTools = slices.Clone(agent.AllowedTools)
	agent.AllowedMCP = maps.Clone(agent.AllowedMCP)
	agent.ContextPaths = slices.Clone(agent.ContextPaths)
	return agent
}

// validUserAgents splits the decoded agent map into the definitions that can
// be used and the ones that cannot, with a reason for each rejection. Rejecting
// beats failing the whole load: one bad agent should not stop the app from
// starting.
func (c *Config) validUserAgents() (valid map[string]Agent, invalid map[string]string) {
	valid = make(map[string]Agent)
	invalid = make(map[string]string)
	builtinTools := allToolNames()
	providers := c.providersOrEmpty()

	for id, agent := range c.Agents {
		switch {
		case id == AgentCoder || id == AgentTask:
			// Built-ins are rebuilt from scratch on every call; a leftover
			// entry from a previous SetupAgents is not a user definition.
			continue
		case agent.Disabled:
			continue
		case !validAgentID(id):
			invalid[id] = "id must be non-empty and contain only letters, digits, '_' or '-'"
		case slices.Contains(builtinTools, id):
			invalid[id] = "id collides with a built-in tool of the same name"
		case strings.TrimSpace(agent.Prompt) == "":
			invalid[id] = "prompt is required for user-defined agents"
		default:
			// A model string that does not resolve to a known provider/model
			// is not worth rejecting the whole agent over; fall back to the
			// empty default and say so, symmetric to the markdown agent
			// loader. This is also where stray "large"/"small" values land
			// now that those words carry no special meaning here.
			if agent.Model != "" {
				if _, err := ResolveModelString(providers, agent.Model); err != nil {
					slog.Warn("Unrecognised model for agent, falling back to the app's main model",
						"agent", id, "model", agent.Model, "error", err)
					c.addProblem(Problem{
						Severity: SeverityWarn,
						Area:     AreaAgent,
						Subject:  id,
						Message:  fmt.Sprintf("agent %s: model %s not found — falls back to the main model", id, agent.Model),
						Hint:     "run 'braid models' to see available provider/model pairs",
					})
					agent.Model = ""
				}
			}
			valid[id] = agent
		}
	}
	return valid, invalid
}

// validAgentID reports whether id is usable as a tool name. Providers reject
// tool names outside this character set, so an invalid id would otherwise
// surface as an opaque API error on the first request.
func validAgentID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func (c *ProviderConfig) TestConnection(resolver VariableResolver) error {
	var (
		providerID = catwalk.InferenceProvider(c.ID)
		testURL    = ""
		headers    = make(map[string]string)
		apiKey, _  = resolver.ResolveValue(c.APIKey)
	)

	switch providerID {
	case catwalk.InferenceProviderMiniMax, catwalk.InferenceProviderMiniMaxChina:
		// NOTE: MiniMax has no good endpoint we can use to validate the API key.
		return nil
	case catwalk.InferenceProviderAlibabaSingapore:
		// NOTE: Alibaba has no good endpoint we can use to validate the API key.
		// Let's at least check the pattern.
		if !strings.HasPrefix(apiKey, "sk-") {
			return fmt.Errorf("invalid API key format for provider %s", c.ID)
		}
		return nil
	}

	switch c.Type {
	case catwalk.TypeOpenAI, catwalk.TypeOpenAICompat, catwalk.TypeOpenRouter:
		baseURL, _ := resolver.ResolveValue(c.BaseURL)
		baseURL = cmp.Or(baseURL, "https://api.openai.com/v1")

		switch providerID {
		case catwalk.InferenceProviderOpenRouter:
			testURL = baseURL + "/credits"
		case catwalk.InferenceProviderOpenCodeGo:
			testURL = strings.Replace(baseURL, "/go", "", 1) + "/models"
		default:
			testURL = baseURL + "/models"
		}

		headers["Authorization"] = "Bearer " + apiKey
	case catwalk.TypeAnthropic:
		baseURL, _ := resolver.ResolveValue(c.BaseURL)
		baseURL = cmp.Or(baseURL, "https://api.anthropic.com/v1")

		switch providerID {
		case catwalk.InferenceKimiCoding:
			testURL = baseURL + "/v1/models"
		default:
			testURL = baseURL + "/models"
		}

		headers["x-api-key"] = apiKey
		headers["anthropic-version"] = "2023-06-01"
	case catwalk.TypeGoogle:
		baseURL, _ := resolver.ResolveValue(c.BaseURL)
		baseURL = cmp.Or(baseURL, "https://generativelanguage.googleapis.com")
		testURL = baseURL + "/v1beta/models?key=" + url.QueryEscape(apiKey)
	case catwalk.TypeBedrock:
		// NOTE: Bedrock has a `/foundation-models` endpoint that we could in
		// theory use, but apparently the authorization is region-specific,
		// so it's not so trivial.
		if strings.HasPrefix(apiKey, "ABSK") { // Bedrock API keys
			return nil
		}
		return errors.New("not a valid bedrock api key")
	case catwalk.TypeVercel:
		// NOTE: Vercel does not validate API keys on the `/models` endpoint.
		if strings.HasPrefix(apiKey, "vck_") { // Vercel API keys
			return nil
		}
		return errors.New("not a valid vercel api key")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &http.Client{}
	req, err := http.NewRequestWithContext(ctx, "GET", testURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request for provider %s: %w", c.ID, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for k, v := range c.ExtraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to create request for provider %s: %w", c.ID, err)
	}
	defer resp.Body.Close()

	switch providerID {
	case catwalk.InferenceProviderZAI:
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("failed to connect to provider %s: %s", c.ID, resp.Status)
		}
	default:
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("failed to connect to provider %s: %s", c.ID, resp.Status)
		}
	}
	return nil
}

// resolveEnvs expands every value in envs through the given resolver
// and returns a fresh "KEY=value" slice sorted by key. The input map is
// not mutated. On the first resolution failure it returns nil and an
// error identifying the offending variable; the inner resolver error is
// already sanitized by ResolveValue and is wrapped with %w.
func resolveEnvs(envs map[string]string, r VariableResolver) ([]string, error) {
	if len(envs) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(envs))
	for k := range envs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	res := make([]string, 0, len(envs))
	for _, k := range keys {
		v, err := r.ResolveValue(envs[k])
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		res = append(res, fmt.Sprintf("%s=%s", k, v))
	}
	return res, nil
}

func ptrValOr[T any](t *T, el T) T {
	if t == nil {
		return el
	}
	return *t
}
