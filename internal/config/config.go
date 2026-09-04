package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/invopop/jsonschema"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/hooks"
	"github.com/rave-soft/sennit/internal/oauth"
	providerstate "github.com/rave-soft/sennit/internal/providers/state"
)

const (
	appName              = brand.Slug
	defaultDataDirectory = brand.DataDir
	defaultInitializeAs  = "AGENTS.md"
)

// defaultContextPaths lists the project files Sennit loads as context
// automatically, without any opt-in. Sennit only reads its own conventions
// here (sennit.md/AGENTS.md and casing/local variants); files belonging to
// other tools (CLAUDE.md, .cursorrules, .github/copilot-instructions.md,
// etc.) are not auto-loaded — add them explicitly via options.context_paths
// (sennitrc: `option context-path CLAUDE.md`) if you want Sennit to read them.
var defaultContextPaths = []string{
	brand.Slug + ".md",
	brand.Slug + ".local.md",
	brand.Name + ".md",
	brand.Name + ".local.md",
	brand.ContextFile,
	brand.ContextFileLocal,
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
	// Sennit picks the first free port from its default range.
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
	// Theme names the color palette the TUI renders in, as chosen by the
	// "/theme" command. The palettes themselves live in internal/ui/styles;
	// config deliberately does not know their IDs, so an unknown or stale
	// value here falls back to Sennit's default scheme rather than failing
	// to load (see styles.PaletteByID). No "enum" here for the same reason:
	// this package must not import internal/ui/styles to list its palette
	// IDs, so — like ProviderConfig.Type's "type" field — the schema
	// command overwrites this property's enum from the live registry after
	// reflection instead (see internal/cmd/schema.go's setProviderTypeEnum
	// for the existing pattern; a theme counterpart belongs there).
	Theme string `json:"theme,omitempty" jsonschema:"description=Color palette for the TUI\\, chosen with the /theme command. An unknown value falls back to the default theme,default=steel-teal"`

	Completions Completions `json:"completions,omitzero" jsonschema:"description=Completions UI options"`
	Transparent *bool       `json:"transparent,omitempty" jsonschema:"description=Enable transparent background for the TUI interface,default=false"`
	Scrollbar   string      `json:"scrollbar,omitempty" jsonschema:"description=Chat scrollbar visibility,enum=default,enum=always,enum=never,default=default"`
	// Spinner selects how much motion the working indicator shows while
	// the agent is busy. It governs the LLM-work spinners only: a shell
	// command's "Running…" has never scrambled, because the scrambled
	// glyphs read as thinking rather than executing.
	//
	// An unknown value falls back to "scramble" and is reported as a
	// config problem rather than failing the load, the way Theme does.
	Spinner     string              `json:"spinner,omitempty" jsonschema:"description=Motion of the working indicator while the agent is busy. scramble is the animated glyph band; pulse keeps the band but moves a single highlight across it; dots is one braille spinner; none leaves only the label and the elapsed timer.,enum=scramble,enum=pulse,enum=dots,enum=none,default=scramble"`
	Keybindings map[string][]string `json:"keybindings,omitempty" jsonschema:"description=Keyboard shortcuts keyed by action name. Each value replaces the default shortcuts for that action"`
}

// Completions defines options for the completions UI.
type Completions struct {
	MaxDepth *int `json:"max_depth,omitempty" jsonschema:"description=Maximum directory depth the @-mention completions popup walks when listing files; 0 is unlimited,default=0,example=10"`
	MaxItems *int `json:"max_items,omitempty" jsonschema:"description=Maximum number of files the @-mention completions popup lists; 0 is unlimited,default=0,example=100"`
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

// Working-indicator motion options. The strings match spin.Mode; this
// package deliberately does not import internal/spin (config must not
// depend on the TUI or the spinner's rendering stack), so the pairing is
// held by TestSpinnerModeParity in internal/ui/styles rather than by the
// type system.
const (
	SpinnerScramble = "scramble" // Band of glyphs redrawn every frame
	SpinnerPulse    = "pulse"    // Band of dots with one travelling highlight
	SpinnerDots     = "dots"     // A single braille spinner glyph
	SpinnerNone     = "none"     // No animated region; label and timer only
)

// SpinnerModes lists every accepted value of [TUIOptions.Spinner], in the
// order they are offered to the person.
var SpinnerModes = []string{SpinnerScramble, SpinnerPulse, SpinnerDots, SpinnerNone}

// SpinnerMode returns the configured working-indicator motion, and whether
// the configured value was recognised. An empty setting is not a problem —
// it means "unset" and resolves to the default — so it reports ok.
func (c *Config) SpinnerMode() (mode string, ok bool) {
	// Options is a pointer and TUI is a pointer inside it; a Config
	// built by hand (every test, and sennit_info's doctor path) has
	// neither. Both are checked, not just the inner one.
	if c == nil || c.Options == nil || c.Options.TUI == nil || c.Options.TUI.Spinner == "" {
		return SpinnerScramble, true
	}
	if slices.Contains(SpinnerModes, c.Options.TUI.Spinner) {
		return c.Options.TUI.Spinner, true
	}
	return SpinnerScramble, false
}

type Permissions struct {
	AllowedTools []string `json:"allowed_tools,omitempty" jsonschema:"description=List of tools that don't require permission prompts,example=bash,example=read"`
	// Bypass, when true, skips every permission prompt from process start —
	// equivalent to always-on yolo mode. It is the persistent counterpart to
	// the session-only --yolo flag / ctrl+y toggle (see permission.Service.
	// SetSkipRequests), which continue to work as a runtime override on top
	// of this.
	Bypass bool `json:"bypass,omitempty" jsonschema:"description=DANGEROUS: skip every permission prompt from startup — the agent runs every tool without asking. Equivalent to always-on yolo mode.,default=false"`
}

type TrailerStyle string

const (
	TrailerStyleNone       TrailerStyle = "none"
	TrailerStyleAssistedBy TrailerStyle = "assisted-by"
)

type Attribution struct {
	TrailerStyle  TrailerStyle `json:"trailer_style,omitempty" jsonschema:"description=Style of attribution trailer to add to commits,enum=none,enum=assisted-by,default=assisted-by"`
	CoAuthoredBy  *bool        `json:"co_authored_by,omitempty" jsonschema:"description=Deprecated: use trailer_style instead"`
	GeneratedWith bool         `json:"generated_with,omitempty" jsonschema:"description=Add Generated with Sennit line to commit messages and issues and PRs,default=true"`
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
	ContextPaths         []string    `json:"context_paths,omitempty" jsonschema:"description=Paths to files containing context information for the AI. Sennit auto-loads only its own conventions (AGENTS.md/SENNIT.md and casing/local variants); list other tools' files here explicitly (e.g. CLAUDE.md) to have Sennit read them too,example=SENNIT.md,example=CLAUDE.md"`
	GlobalContextPaths   []string    `json:"global_context_paths,omitempty" jsonschema:"description=Paths to files containing global context information for the AI,default=~/.config/sennit/SENNIT.md,default=~/.config/AGENTS.md"`
	SkillsPaths          []string    `json:"skills_paths,omitempty" jsonschema:"description=Paths to directories containing Agent Skills (folders with SKILL.md files),example=~/.config/sennit/skills,example=./skills"`
	TUI                  *TUIOptions `json:"tui,omitempty" jsonschema:"description=Terminal user interface options"`
	Debug                bool        `json:"debug,omitempty" jsonschema:"description=Enable debug logging,default=false"`
	DebugLSP             bool        `json:"debug_lsp,omitempty" jsonschema:"description=Enable debug logging for LSP servers,default=false"`
	DisableAutoSummarize bool        `json:"disable_auto_summarize,omitempty" jsonschema:"description=Disable automatic conversation summarization,default=false"`
	// AutoSummarizeAt caps the context a session is allowed to work in
	// before it summarizes, in tokens, for models whose own window is
	// larger than anyone wants to pay for. Every step of a turn re-sends
	// the whole conversation, so a session left to fill a 872k window
	// spends the rest of its life carrying 872k; summarizing once at a
	// self-imposed ceiling is cheaper than a hundred steps above it.
	//
	// 0 (unset) means the model's own window is the only limit, which is
	// the behaviour this had before the setting existed. A value at or
	// above the model's window has no effect. Ignored entirely when
	// DisableAutoSummarize is set — that switch still wins.
	AutoSummarizeAt int64 `json:"auto_summarize_at,omitempty" jsonschema:"description=Summarize once a session's context reaches this many tokens even when the model's own window is larger - 0 means no cap,default=0"`
	// AutoSummarizeIdle summarizes a session that has been left alone
	// with a large context, instead of waiting for the next turn to walk
	// into the window. See AutoSummarizeIdleOptions. Ignored entirely
	// when DisableAutoSummarize is set — that switch still wins.
	AutoSummarizeIdle *AutoSummarizeIdleOptions `json:"auto_summarize_idle,omitempty" jsonschema:"description=Summarize a session that has grown past a context size and then sat idle"`
	// DataDirectory is a project-local directory (".sennit" by default) for
	// workspace-scoped state that is NOT part of the shared global database:
	// the single-instance lock file, workspace config overrides, and (until
	// imported) a pre-shared-database project's legacy sennit.db. Session and
	// message history now live in the single global database (see
	// config.GlobalDBDir()), not here. Relative paths are resolved against
	// the working directory; absolute paths are used verbatim. After
	// defaulting the stored value is always absolute.
	DataDirectory           string            `json:"data_directory,omitempty" jsonschema:"description=Project-local directory for workspace-scoped state (lock file\\, config overrides\\, legacy pre-migration database) — not the shared session/message database. Relative paths are resolved against the working directory; absolute paths are used as-is.,default=.sennit,example=.sennit"`
	DisabledTools           []string          `json:"disabled_tools,omitempty" jsonschema:"description=List of built-in tools to disable and hide from the agent,example=bash,example=web_search"`
	DisableDefaultProviders bool              `json:"disable_default_providers,omitempty" jsonschema:"description=Ignore all default/embedded providers. When enabled\\, providers must be fully specified in the config file with base_url\\, models\\, and api_key - no merging with defaults occurs,default=false"`
	Attribution             *Attribution      `json:"attribution,omitempty" jsonschema:"description=Attribution settings for generated content"`
	DisableMetrics          bool              `json:"disable_metrics,omitempty" jsonschema:"description=Disable sending metrics,default=false"`
	InitializeAs            string            `json:"initialize_as,omitempty" jsonschema:"description=Name of the context file to create/update during project initialization,default=AGENTS.md,example=AGENTS.md,example=SENNIT.md,example=CLAUDE.md,example=docs/LLMs.md"`
	AutoLSP                 *bool             `json:"auto_lsp,omitempty" jsonschema:"description=Automatically setup LSPs based on root markers,default=true"`
	Progress                *bool             `json:"progress,omitempty" jsonschema:"description=Show indeterminate progress updates during long operations,default=true"`
	Notifications           string            `json:"notifications,omitempty" jsonschema:"description=Notification style to use. Options: auto (default)\\, native\\, osc\\, bell\\, disabled. Auto selects based on environment: native for local sessions\\, osc for SSH (with automatic OSC 99/777 detection).,enum=auto,enum=native,enum=osc,enum=bell,enum=disabled,default=auto"`
	DisabledSkills          []string          `json:"disabled_skills,omitempty" jsonschema:"description=List of skill names to disable and hide from the agent,example=sennit-config"`
	WebSearch               *WebSearchOptions `json:"web_search,omitempty" jsonschema:"description=Web search backend configuration. Defaults to the keyless DuckDuckGo scraper when omitted."`
	Threads                 *ThreadsOptions   `json:"threads,omitempty" jsonschema:"description=Threads (parallel agent work stream) configuration."`
	// BackgroundAgents is a permanent opt-out, not a rollout flag: it stays
	// in the product for anyone who does not want the model delegating work
	// to background tasks in their workspace. Default true — a pointer
	// distinguishes "unset" from an explicit false, the same tri-state
	// AutoLSP and Progress use above. It defaults on because dispatch is
	// already opt-in per model tool-call and every tool a task runs still
	// goes through the same permission checks as the foreground turn; the
	// switch exists for the person who wants to rule out unattended
	// concurrent work entirely, not as a safety net for a first run.
	//
	// Turning this off only stops *new* delegation dispatch (built-in,
	// custom, and fetch tools), and task_* tools are not registered. A task
	// already running when the config is reloaded is not
	// killed — it runs to completion and its result is still delivered.
	// Threads (the git-worktree feature) are a separate, older feature and
	// are not affected by this switch.
	BackgroundAgents *bool `json:"background_agents,omitempty" jsonschema:"description=Allow asynchronous subagent delegation and the task_* tools\\, letting the model delegate work to background tasks in this workspace. Turning this off only blocks new dispatch — a task already running keeps running to completion. Does not affect threads.,default=true"`
	// HistoryRetentionDays is read by `sennit gc`, not enforced automatically:
	// nothing purges history on its own. A pointer distinguishes "unset"
	// (defaults to 90) from an explicit 0, which means keep history forever.
	HistoryRetentionDays *int `json:"history_retention_days,omitempty" jsonschema:"description=Age in days after which \"sennit gc\" deletes sessions (and their messages/files) and finished threads. 0 keeps history forever. Not enforced automatically — run \"sennit gc\" (e.g. from cron) to apply it.,default=90,example=30,example=180"`
}

// AutoSummarizeIdleOptions configures the idle summarize pass: a session
// whose context has grown past ContextTokens and has then seen no work
// for After is summarized where it stands, without waiting for someone to
// come back and send the next turn.
//
// The point is when the cost is paid, not whether. A session that has
// filled up summarizes sooner or later either way; doing it while nobody
// is waiting means the person's next turn starts on a compacted context
// instead of stopping mid-answer to compress the one it just read.
//
// Every field is optional and answered by the Effective* accessors below,
// all of which are safe to call on a nil *AutoSummarizeIdleOptions — the
// zero config is "on, with the defaults".
type AutoSummarizeIdleOptions struct {
	// Enabled turns the idle pass on or off. A pointer distinguishes
	// "unset" from an explicit false, the same tri-state AutoLSP and
	// Progress use; unset means on.
	Enabled *bool `json:"enabled,omitempty" jsonschema:"description=Summarize a session left idle with a large context,default=true"`
	// ContextTokens is how large a session's context must have grown
	// before idling is worth summarizing at all, in tokens — the same
	// unit as AutoSummarizeAt, measured against the session's recorded
	// prompt tokens. A session under it is left alone however long it
	// sits: summarizing a short conversation costs a request and throws
	// away detail to free room nobody needed.
	ContextTokens int64 `json:"context_tokens,omitempty" jsonschema:"description=Context size (in prompt tokens) a session must exceed before an idle summarize is worth doing,default=60000,example=60000"`
	// After is how long a session must go without work before the pass
	// fires, as a Go duration string ("4m", "90s", "1h"). Measured from
	// the last turn this process ran for the session, so a person
	// thinking, or away entirely, counts as idle — which is the point.
	//
	// The sweep runs on its own coarse tick (see
	// idleSummarizeSweepInterval in internal/agent), so a trip can be up
	// to one interval late. Nothing about this needs to be precise.
	After string `json:"after,omitempty" jsonschema:"description=How long a session must sit idle before it is summarized (Go duration),default=4m,example=4m,example=90s"`
}

// Idle-summarize defaults, applied by the accessors below when a field is
// unset. See AutoSummarizeIdleOptions for what each one means.
const (
	DefaultAutoSummarizeIdleContextTokens = 60_000
	DefaultAutoSummarizeIdleAfter         = 4 * time.Minute
)

// IsEnabled reports whether the idle summarize pass should run. Safe on a
// nil receiver: an absent config block is the default, which is on.
func (o *AutoSummarizeIdleOptions) IsEnabled() bool {
	if o == nil {
		return true
	}
	return ptrValOr(o.Enabled, true)
}

// EffectiveContextTokens returns the context size an idle session must
// exceed to be worth summarizing. A zero or negative value falls back to
// the default rather than meaning "summarize everything": there is no
// useful reading of "summarize a session with no context in it".
func (o *AutoSummarizeIdleOptions) EffectiveContextTokens() int64 {
	if o == nil || o.ContextTokens <= 0 {
		return DefaultAutoSummarizeIdleContextTokens
	}
	return o.ContextTokens
}

// EffectiveAfter returns how long a session must be idle first. An empty,
// unparseable, or non-positive value falls back to the default — the same
// treatment RotationConfig.EffectiveCooldown gives a bad cooldown, and for
// the same reason: a typo here must not turn into "summarize immediately".
func (o *AutoSummarizeIdleOptions) EffectiveAfter() time.Duration {
	if o == nil || o.After == "" {
		return DefaultAutoSummarizeIdleAfter
	}
	d, err := time.ParseDuration(o.After)
	if err != nil || d <= 0 {
		return DefaultAutoSummarizeIdleAfter
	}
	return d
}

// ThreadsOptions configures the threads feature (parallel agent work
// streams running in isolated git worktrees).
type ThreadsOptions struct {
	// WorktreeDir is the parent directory under which each thread's git
	// worktree is created (at <worktree_dir>/<thread-name>). A relative
	// path is resolved against the parent of the repository root, not
	// against the working directory. Absolute paths are used as-is.
	// Defaults to "threads" inside the workspace data directory
	// (<repo>/.sennit/threads), which is ignored by the repository's own
	// git, so a worktree there is not seen as a second copy of the
	// project.
	WorktreeDir string `json:"worktree_dir,omitempty" jsonschema:"description=Parent directory for thread worktrees (<worktree_dir>/<thread-name>). A relative path resolves against the parent of the repository root; an absolute path is used as-is. Defaults to \"threads\" inside the workspace data directory (.sennit/threads).,example=/var/tmp/sennit-threads,example=../thread-worktrees"`
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

// HookConfig defines a user-configured shell command that fires on a hook
// event (e.g. PreToolUse). This is a pure-data struct: matcher compilation
// is owned by hooks.Runner so a JSON round-trip, merge, or reload can't
// silently drop compiled state.
// HookConfig is the shape of one configured hook. It is an alias for
// hooks.Hook, which is where the type now lives: this package is imported
// by nearly everything and pulls in the database, the shell and the OAuth
// providers, so a consumer that only wanted to describe a hook had to link
// all of it. An alias rather than a distinct type means every existing
// use, including the JSON schema generated from these tags, is unchanged.
type HookConfig = hooks.Hook

// normalizeHookEvent maps user-provided event names to their canonical
// form. Matching is case-insensitive and accepts snake_case variants
// (e.g. "pre_tool_use" → "PreToolUse").
func normalizeHookEvent(name string) string {
	switch strings.ToLower(strings.ReplaceAll(name, "_", "")) {
	case "pretooluse":
		return "PreToolUse"
	default:
		return name
	}
}

// ValidateHooks normalizes event names and checks that every configured
// hook has a command and a syntactically valid matcher regex. Matcher
// compilation used for matching is owned by hooks.Runner; this function
// only validates up front so the user sees config errors at load time
// rather than on the first tool call.
func (c *Config) ValidateHooks() error {
	// Normalize event name keys, in sorted key order rather than map
	// iteration order. A config file carrying more than one spelling of
	// the same event (e.g. "pre_tool_use" and "PreToolUse") has each
	// non-canonical key's hooks appended onto the canonical one below;
	// ranging over the map directly made which spelling's hooks landed
	// first — and therefore hook execution order — vary from run to run.
	events := make([]string, 0, len(c.Hooks))
	for event := range c.Hooks {
		events = append(events, event)
	}
	slices.Sort(events)
	for _, event := range events {
		eventHooks := c.Hooks[event]
		canonical := normalizeHookEvent(event)
		if canonical != event {
			c.Hooks[canonical] = append(c.Hooks[canonical], eventHooks...)
			delete(c.Hooks, event)
		}
	}

	for event, eventHooks := range c.Hooks {
		for i, h := range eventHooks {
			if h.Command == "" {
				return fmt.Errorf("hook %s[%d]: command is required", event, i)
			}
			if h.Matcher == "" {
				continue
			}
			if _, err := regexp.Compile(h.Matcher); err != nil {
				return fmt.Errorf("hook %s[%d]: invalid matcher regex %q: %w", event, i, h.Matcher, err)
			}
		}
	}
	return nil
}

// Config holds the configuration for sennit.
type Config struct {
	Schema string `json:"$schema,omitempty"`

	// Model is the single model Sennit uses for the session. Global-only:
	// a "model" block in a project config is stripped before the merge (see
	// globalOnlyKeys).
	Model SelectedModel `json:"model,omitzero" jsonschema:"description=The model configuration. Read only from the global config — a model block in a project config is ignored,example={\"model\":\"gpt-4o\",\"provider\":\"openai\"}"`

	// RecentModels lists recently used models, most-recent-first. Stored
	// in the data directory config. Global-only, like Model.
	RecentModels []SelectedModel `json:"recent_models,omitempty" jsonschema:"-"`

	// The providers that are configured. Global-only: a "providers" block in
	// a project config is stripped before the merge (see globalOnlyKeys), so
	// a cloned repository can never repoint a session at another endpoint.
	// Frozen on publish (see ConfigStore.setConfig) — Set/Del/Reset/Take on
	// a published Config's Providers panics; mutate a clone instead.
	Providers *csync.Map[string, ProviderConfig] `json:"providers,omitempty" jsonschema:"description=AI provider configurations. Read only from the global config — a providers block in a project config is ignored"`

	RuntimeProviders *csync.Map[string, providerstate.Provider] `json:"-" jsonschema:"-"`

	MCP MCPs `json:"mcp,omitempty" jsonschema:"description=Model Context Protocol server configurations"`

	LSP LSPs `json:"lsp,omitempty" jsonschema:"description=Language Server Protocol configurations"`

	Options *Options `json:"options,omitempty" jsonschema:"description=General application options"`

	Permissions *Permissions `json:"permissions,omitempty" jsonschema:"description=Permission settings for tool usage"`

	Tools Tools `json:"tools,omitzero" jsonschema:"description=Tool configurations"`

	Hooks map[string][]HookConfig `json:"hooks,omitempty" jsonschema:"description=User-defined shell commands that fire on hook events (e.g. PreToolUse)"`

	// Env is a map of environment variables set on startup.
	Env map[string]string `json:"env,omitempty" jsonschema:"description=Environment variables to set on startup"`

	// Agents holds both the built-in agents and any the user defines.
	// SetupAgents populates this from .sennit/agents/*.md files (via
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
	// the sites where such problems are detected, and read back by
	// Doctor. Not persisted to disk.
	Problems []Problem `json:"-"`

	// jsonAgentsBlockDetected records whether any loaded JSON config layer
	// contained a top-level "agents" key. loadFromBytes (load.go) strips the
	// block before unmarshaling instead of decoding it into Agents — a JSON
	// "agents" entry is no longer read at all, only .sennit/agents/*.md files
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
// (with its nested TUI pointer). Providers gets a new *csync.Map with each
// entry's own mutable fields (headers, extra params, OAuth token) deep
// copied too, so a mutator can rewrite one provider's credentials without
// racing a reader iterating the old map; the remaining fields are
// immutable after load from the mutators' standpoint and are likewise
// shared.
func (c *Config) cloneForWrite() *Config {
	nc := *c
	nc.RecentModels = slices.Clone(c.RecentModels)
	nc.MCP = maps.Clone(c.MCP)
	// Problems is rewritten in place by setupAgents (it deletes the agent
	// entries and re-adds the current ones), so sharing the published
	// config's slice let that rewrite reach a Config other goroutines are
	// already reading — the one thing cloneForWrite exists to prevent.
	nc.Problems = slices.Clone(c.Problems)
	if c.Providers != nil {
		nc.Providers = csync.NewMap[string, ProviderConfig]()
		for id, provider := range c.Providers.Seq2() {
			provider.ExtraHeaders = maps.Clone(provider.ExtraHeaders)
			provider.ExtraBody = maps.Clone(provider.ExtraBody)
			provider.ProviderOptions = maps.Clone(provider.ProviderOptions)
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
	if c.RuntimeProviders != nil {
		nc.RuntimeProviders = csync.NewMap[string, providerstate.Provider]()
		for id, provider := range c.RuntimeProviders.Seq2() {
			nc.RuntimeProviders.Set(id, providerstate.Clone(provider))
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

// ThemeID returns the configured TUI theme, or the empty string when none is
// set. Callers resolve it through styles.PaletteByID, which maps both the
// empty string and an unknown ID onto the default palette, so this never has
// to guess a name config does not own.
func (c *Config) ThemeID() string {
	if c == nil || c.Options == nil || c.Options.TUI == nil {
		return ""
	}
	return c.Options.TUI.Theme
}

// TransparentEnabled reports whether the TUI's transparent background is
// turned on, or false when Options, TUI, or Transparent itself is unset.
func (c *Config) TransparentEnabled() bool {
	if c == nil || c.Options == nil || c.Options.TUI == nil || c.Options.TUI.Transparent == nil {
		return false
	}
	return *c.Options.TUI.Transparent
}

// CompletionsLimits returns the configured @-mention completion depth and
// item limits, or the zero values (meaning "no limit") when Options or TUI
// is unset — a Config built by hand (every test) has neither.
func (c *Config) CompletionsLimits() (depth, items int) {
	if c == nil || c.Options == nil || c.Options.TUI == nil {
		return 0, 0
	}
	return c.Options.TUI.Completions.Limits()
}

// DiffMode returns the configured TUI diff mode, or the empty string when
// Options or TUI is unset.
func (c *Config) DiffMode() string {
	if c == nil || c.Options == nil || c.Options.TUI == nil {
		return ""
	}
	return c.Options.TUI.DiffMode
}

// Scrollbar returns the configured chat scrollbar visibility, or the empty
// string when Options or TUI is unset (callers fall back to
// [ScrollbarDefault] themselves).
func (c *Config) Scrollbar() string {
	if c == nil || c.Options == nil || c.Options.TUI == nil {
		return ""
	}
	return c.Options.TUI.Scrollbar
}

// Keybindings returns the configured per-action key overrides, or nil when
// Options or TUI is unset.
func (c *Config) Keybindings() map[string][]string {
	if c == nil || c.Options == nil || c.Options.TUI == nil {
		return nil
	}
	return c.Options.TUI.Keybindings
}

// CompactMode reports whether the TUI should start in compact mode, or
// false when Options or TUI is unset.
func (c *Config) CompactMode() bool {
	if c == nil || c.Options == nil || c.Options.TUI == nil {
		return false
	}
	return c.Options.TUI.CompactMode
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

// ProviderName returns the configured display name for provider id, or
// ok=false if the provider isn't configured. It exists so callers that only
// need a provider's friendly name (e.g. the chat view's assistant info
// footer) can depend on a narrow method instead of the Providers field
// directly.
func (c *Config) ProviderName(id string) (name string, ok bool) {
	providerConfig, ok := c.Providers.Get(id)
	if !ok {
		return "", false
	}
	return providerConfig.Name, true
}

// AgentOverride returns the model/reasoning-effort override configured for
// the user-defined agent tool named name, or ok=false if name isn't one.
// The built-in "coder"/"task" agents never carry an override (see
// setupAgents), so excluding them here is observably identical to a plain
// c.Agents[name] lookup. It exists so callers that only need this
// model/effort pair (e.g. the chat view's delegation renderer) can depend
// on two strings instead of the Agents map and the Agent type.
func (c *Config) AgentOverride(name string) (model, effort string, ok bool) {
	if c == nil {
		return "", "", false
	}
	if name == AgentCoder || name == AgentTask {
		return "", "", false
	}
	a, ok := c.Agents[name]
	if !ok {
		return "", "", false
	}
	return a.Model, a.ReasoningEffort, true
}

// GetProviderForModel returns the provider configured for c.Model.
func (c *Config) GetProviderForModel() *ProviderConfig {
	if providerConfig, ok := c.Providers.Get(c.Model.Provider); ok {
		return &providerConfig
	}
	return nil
}

// SelectedCatalogModel returns the catalog entry for c.Model, the one model
// Sennit is configured to use.
func (c *Config) SelectedCatalogModel() *catwalk.Model {
	return c.GetModel(c.Model.Provider, c.Model.Model)
}

// RememberedReasoningEffort returns the reasoning effort provider/model was
// last used at, or "" when the pair has never been tuned.
//
// Effort is a property of the model, not of the app: "high" on a small
// reasoner and "high" on a frontier one buy different things, and a user who
// switches between two models expects each to come back the way they left
// it. The current selection is checked first (it is the freshest value, and
// it is written before the recent list catches up), then the recent-models
// list, which carries the effort of every model still on it.
func (c *Config) RememberedReasoningEffort(provider, model string) string {
	if provider == "" || model == "" {
		return ""
	}
	if c.Model.Provider == provider && c.Model.Model == model {
		return c.Model.ReasoningEffort
	}
	for _, recent := range c.RecentModels {
		if recent.Provider == provider && recent.Model == model {
			return recent.ReasoningEffort
		}
	}
	return ""
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

func ptrValOr[T any](t *T, el T) T {
	if t == nil {
		return el
	}
	return *t
}
