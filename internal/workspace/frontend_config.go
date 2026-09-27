package workspace

import (
	"fmt"
	"net/url"
	"slices"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/home"
)

// FrontendConfig is the allowlist snapshot of *config.Config the UI is
// allowed to see. *config.Config carries secrets (API keys, OAuth tokens)
// and unexported/json:"-" runtime state (RuntimeProviders) that must never
// reach a remote frontend once Workspace is served over gRPC with a JSON
// codec - see CLIENT-SERVER.md PR 0.5. Every field here is exported and
// JSON-tagged, and every method reads only the struct itself, so this type
// is safe to hand to a client that has nothing else to go on.
//
// It carries the methods the UI already calls on *config.Config
// (GetModel, ProviderName, ...) with the same names and semantics, so
// swapping ui/common.Common.Config()'s return type for this one is a
// mechanical change at call sites.
//
// Read-only, and shared. AppWorkspace.Config() (internal/workspace/appws)
// caches the *FrontendConfig it builds and hands the same pointer to every
// caller until the underlying *config.Config snapshot changes (see that
// method's doc comment) - the UI calls Config() several times per rendered
// frame, and this type embeds the model catalog, so rebuilding it on every
// call is O(providers x models) allocation per call. FrontendProvider.
// Models and .Rotation alias the source config.ProviderConfig's own slice/
// pointer rather than cloning them (config.Config is itself immutable
// once published - mutators clone-and-swap - so sharing is safe as long as
// nothing here is mutated). Treat every value reachable from a
// *FrontendConfig as read-only: copy before mutating, never write through
// it.
type FrontendConfig struct {
	Model        config.SelectedModel   `json:"model"`
	RecentModels []config.SelectedModel `json:"recent_models,omitempty"`

	// Providers is sorted by ID so it renders deterministically without
	// each caller having to sort it again.
	Providers []FrontendProvider `json:"providers,omitempty"`

	// MCPNames lists every configured MCP server name, sorted, disabled
	// ones included - see config.Config.MCPServerNames's doc comment for
	// why disabled servers still need to be named.
	MCPNames []string `json:"mcp_names,omitempty"`

	// Agents carries only the fields the UI reads off a config.Agent:
	// the model/effort override a delegation tool renders. It excludes
	// the built-in "coder"/"task" roles, same as AgentOverride.
	Agents map[string]FrontendAgent `json:"agents,omitempty"`

	InitializeAs   string   `json:"initialize_as,omitempty"`
	DisabledSkills []string `json:"disabled_skills,omitempty"`

	// ServerHome is the home directory of the machine running the
	// workspace - home.Dir() as seen from there, not from wherever the UI
	// happens to run. A server-side absolute path (Workspace.WorkingDir(),
	// a permission request's file path, ...) must be shortened against
	// this home, not the client's own - see internal/ui/common's
	// PrettyServerPath, CLIENT-SERVER.md's "PR 0.6".
	ServerHome string `json:"server_home,omitempty"`
}

// FrontendAgent is the slice of config.Agent the UI needs to render a
// delegation tool's model/effort override.
type FrontendAgent struct {
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// FrontendProvider is the allowlist projection of config.ProviderConfig.
// Custom and Auth are computed by NewFrontendConfig, not carried over
// verbatim from the config file, since both depend on state
// (KnownProviders, RuntimeProviders) that lives outside ProviderConfig
// itself.
type FrontendProvider struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	BaseURL string       `json:"base_url,omitempty"`
	Type    catwalk.Type `json:"type,omitempty"`
	Disable bool         `json:"disable,omitempty"`

	// ProxyURL has any userinfo password stripped - see RedactProxyURL.
	ProxyURL string                 `json:"proxy_url,omitempty"`
	Rotation *config.RotationConfig `json:"rotation,omitempty"`
	Models   []catwalk.Model        `json:"models,omitempty"`

	// Custom reports whether this provider is not in the known-providers
	// catalog (a base_url-based provider whose models come from
	// discovery), computed the same way isCustomProvider does today.
	Custom bool `json:"custom,omitempty"`

	Auth ProviderAuth `json:"auth"`
}

// ProviderAuth is the credential state the UI is allowed to know about a
// provider: whether it has one, what kind, and whether an OAuth token has
// expired. It never carries the credential itself.
type ProviderAuth struct {
	// Known is true when the provider has a runtime entry at all - i.e.
	// config.Config.RuntimeProvider(id) resolved. false means "unknown",
	// not "no auth": the UI's providerSettingsAuthUnknown state matches
	// !Known.
	Known bool `json:"known,omitempty"`

	HasAPIKey bool `json:"has_api_key,omitempty"`
	HasOAuth  bool `json:"has_oauth,omitempty"`
	// OAuthExpiresAt is the OAuth token's expiry (Unix seconds), zero if
	// HasOAuth is false.
	OAuthExpiresAt int64 `json:"oauth_expires_at,omitempty"`
	// Account is the ID of the active account, if the provider has more
	// than one - see config.ProviderConfig.Account.
	Account string `json:"account,omitempty"`
}

// oauthExpiryBuffer mirrors oauth.Token's minRefreshBuffer: proactively
// treat a token as expired this many seconds before its real expiry.
// ProviderAuth only carries OAuthExpiresAt (not ExpiresIn), so it cannot
// reproduce oauth.Token.IsExpired's dynamic expires_in/10 buffer exactly;
// a fixed floor is close enough for the UI's "should I show a re-auth
// badge" question.
const oauthExpiryBuffer = 30

// Expired reports whether the OAuth token has passed (or is within
// oauthExpiryBuffer seconds of) its expiry. It is meaningless when
// HasOAuth is false.
func (a ProviderAuth) Expired(nowUnix int64) bool {
	return nowUnix >= (a.OAuthExpiresAt - oauthExpiryBuffer)
}

// NewFrontendConfig builds the allowlist snapshot of cfg that a frontend
// (in-process or, eventually, remote) is allowed to see. knownProviders is
// the catalog used to decide FrontendProvider.Custom and to resolve
// DefaultModelForProvider's default model, exactly as isCustomProvider and
// config.Config.DefaultModelForProvider use it today.
func NewFrontendConfig(cfg *config.Config, knownProviders []catwalk.Provider) *FrontendConfig {
	fc := &FrontendConfig{
		Model:        cfg.Model,
		RecentModels: slices.Clone(cfg.RecentModels),
		MCPNames:     cfg.MCPServerNames(),
		ServerHome:   home.Dir(),
	}
	slices.Sort(fc.MCPNames)

	if cfg.Providers != nil {
		ids := make([]string, 0, cfg.Providers.Len())
		for id := range cfg.Providers.Seq2() {
			ids = append(ids, id)
		}
		slices.Sort(ids)

		fc.Providers = make([]FrontendProvider, 0, len(ids))
		for _, id := range ids {
			pc, _ := cfg.Providers.Get(id)
			fc.Providers = append(fc.Providers, newFrontendProvider(cfg, id, pc, knownProviders))
		}
	}

	if len(cfg.Agents) > 0 {
		fc.Agents = make(map[string]FrontendAgent, len(cfg.Agents))
		for name, a := range cfg.Agents {
			fc.Agents[name] = FrontendAgent{Model: a.Model, ReasoningEffort: a.ReasoningEffort}
		}
	}

	if cfg.Options != nil {
		fc.InitializeAs = cfg.Options.InitializeAs
		fc.DisabledSkills = slices.Clone(cfg.Options.DisabledSkills)
	}

	return fc
}

// newFrontendProvider projects one provider entry, computing Custom and
// Auth from state ProviderConfig itself does not carry.
func newFrontendProvider(cfg *config.Config, id string, pc config.ProviderConfig, knownProviders []catwalk.Provider) FrontendProvider {
	fp := FrontendProvider{
		ID:       id,
		Name:     pc.Name,
		BaseURL:  pc.BaseURL,
		Type:     pc.Type,
		Disable:  pc.Disable,
		ProxyURL: RedactProxyURL(pc.ProxyURL),
		Rotation: pc.Rotation,
		Models:   pc.Models,
		Custom:   isCustomProvider(id, pc, knownProviders),
	}

	// Read straight off cfg.RuntimeProviders rather than through
	// cfg.RuntimeProvider(id): that method exists for a caller that goes
	// on to use the returned providerstate.Provider (its OAuth token,
	// its own Models list, ...), so it defensively deep-clones every
	// mutable field (providerstate.Clone). Only three scalar facts are
	// read here (a key present, a token present and its expiry, the
	// active account id), and NewFrontendConfig runs once per rendered
	// UI frame across every configured provider - the clone's
	// allocations (Models slice, ExtraHeaders/ExtraParams maps, the
	// token) would otherwise be pure waste on every call. Reading the
	// shared value directly is safe because nothing here mutates it.
	if cfg.RuntimeProviders != nil {
		if rp, ok := cfg.RuntimeProviders.Get(id); ok {
			fp.Auth.Known = true
			fp.Auth.HasAPIKey = rp.APIKey != ""
			fp.Auth.Account = rp.Account
			if rp.OAuthToken != nil {
				fp.Auth.HasOAuth = true
				fp.Auth.OAuthExpiresAt = rp.OAuthToken.ExpiresAt
			}
		}
	}

	return fp
}

// isCustomProvider reports whether id is a custom provider with a
// base_url: not in the catalog, so its models come from discovery. Mirrors
// the rule internal/ui/dialog/provider_settings.go's isCustomProvider used
// before this DTO existed.
func isCustomProvider(id string, pc config.ProviderConfig, knownProviders []catwalk.Provider) bool {
	if pc.BaseURL == "" {
		return false
	}
	for _, p := range knownProviders {
		if string(p.ID) == id {
			return false
		}
	}
	return true
}

// RedactProxyURL strips any password from a proxy URL's userinfo before it
// crosses to a frontend. An unparseable value (e.g. a "$VAR" template that
// has not been shell-expanded) is returned unchanged - it carries no parsed
// credential to strip.
//
// Exported so appws's OAuthController methods (OAuthConfiguredProxy,
// OAuthProviderConfiguredProxy) and their resolveSubmittedProxy helper can
// apply and reverse the same redaction a proxy goes through here - see
// those methods' doc comments.
func RedactProxyURL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		return raw
	}
	u.User = url.User(u.User.Username())
	return u.String()
}

// GetModel returns the catalog entry for provider/model, or nil if the
// provider is not configured or has no such model. Mirrors
// config.Config.GetModel.
func (c *FrontendConfig) GetModel(provider, model string) *catwalk.Model {
	pc, ok := c.Provider(provider)
	if !ok {
		return nil
	}
	for _, m := range pc.Models {
		if m.ID == model {
			return &m
		}
	}
	return nil
}

// ProviderName returns the configured display name for provider id, or
// ok=false if the provider isn't configured. Mirrors config.Config.ProviderName.
func (c *FrontendConfig) ProviderName(id string) (name string, ok bool) {
	pc, ok := c.Provider(id)
	if !ok {
		return "", false
	}
	return pc.Name, true
}

// SelectedCatalogModel returns the catalog entry for c.Model, the one
// model Sennit is configured to use. Mirrors config.Config.SelectedCatalogModel.
func (c *FrontendConfig) SelectedCatalogModel() *catwalk.Model {
	return c.GetModel(c.Model.Provider, c.Model.Model)
}

// GetProviderForModel returns the provider configured for c.Model. Mirrors
// config.Config.GetProviderForModel.
func (c *FrontendConfig) GetProviderForModel() *FrontendProvider {
	pc, ok := c.Provider(c.Model.Provider)
	if !ok {
		return nil
	}
	return &pc
}

// RememberedReasoningEffort returns the reasoning effort provider/model was
// last used at, or "" when the pair has never been tuned. Mirrors
// config.Config.RememberedReasoningEffort.
func (c *FrontendConfig) RememberedReasoningEffort(provider, model string) string {
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
// already-configured provider. Mirrors config.Config.DefaultModelForProvider.
func (c *FrontendConfig) DefaultModelForProvider(providerID string, knownProviders []catwalk.Provider) (config.SelectedModel, error) {
	pc, ok := c.Provider(providerID)
	if !ok {
		return config.SelectedModel{}, fmt.Errorf("provider %s is not configured", providerID)
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
		if len(pc.Models) == 0 {
			return config.SelectedModel{}, fmt.Errorf("provider %s has no models configured", providerID)
		}
		model = &pc.Models[0]
	}
	return config.SelectedModel{Provider: providerID, Model: model.ID}, nil
}

// IsConfigured reports whether at least one provider is enabled. Mirrors
// config.Config.IsConfigured.
func (c *FrontendConfig) IsConfigured() bool {
	for _, p := range c.Providers {
		if !p.Disable {
			return true
		}
	}
	return false
}

// AgentOverride returns the model/reasoning-effort override configured for
// the user-defined agent tool named name, or ok=false if name isn't one.
// Mirrors config.Config.AgentOverride.
func (c *FrontendConfig) AgentOverride(name string) (model, effort string, ok bool) {
	if c == nil {
		return "", "", false
	}
	if name == config.AgentCoder || name == config.AgentTask {
		return "", "", false
	}
	a, ok := c.Agents[name]
	if !ok {
		return "", "", false
	}
	return a.Model, a.ReasoningEffort, true
}

// HasCoderAgent reports whether the built-in "coder" agent is set up. The
// UI uses this as a proxy for "is the app past onboarding/ready to run
// turns" - see config.Config's setupAgents, which always populates it once
// a model is configured.
func (c *FrontendConfig) HasCoderAgent() bool {
	if c == nil {
		return false
	}
	_, ok := c.Agents[config.AgentCoder]
	return ok
}

// ServerHomeDir implements chat.CustomAgentConfig. See the ServerHome
// field's doc comment for what it shortens.
func (c *FrontendConfig) ServerHomeDir() string {
	if c == nil {
		return ""
	}
	return c.ServerHome
}

// MCPServerNames returns the names of every configured MCP server,
// disabled ones included. Mirrors config.Config.MCPServerNames.
func (c *FrontendConfig) MCPServerNames() []string {
	if c == nil {
		return nil
	}
	return c.MCPNames
}

// Provider looks up providerID, replacing the config.Config.Providers.Get
// call sites in internal/ui used before this DTO existed.
func (c *FrontendConfig) Provider(providerID string) (FrontendProvider, bool) {
	if c == nil {
		return FrontendProvider{}, false
	}
	for _, p := range c.Providers {
		if p.ID == providerID {
			return p, true
		}
	}
	return FrontendProvider{}, false
}

// ProvidersSeq returns an iterator over c.Providers, replacing the
// config.Config.Providers.Seq2 call sites in internal/ui used before this
// DTO existed.
func (c *FrontendConfig) ProvidersSeq() func(yield func(string, FrontendProvider) bool) {
	return func(yield func(string, FrontendProvider) bool) {
		for _, p := range c.Providers {
			if !yield(p.ID, p) {
				return
			}
		}
	}
}

// IsDockerMCPEnabled reports whether the Docker MCP catalog is configured.
// Mirrors config.Config.IsDockerMCPEnabled.
func (c *FrontendConfig) IsDockerMCPEnabled() bool {
	return slices.Contains(c.MCPNames, config.DockerMCPName)
}

// ProviderAuth returns the credential state for providerID, or a zero
// ProviderAuth (Known=false) if it isn't configured.
func (c *FrontendConfig) ProviderAuth(providerID string) ProviderAuth {
	pc, ok := c.Provider(providerID)
	if !ok {
		return ProviderAuth{}
	}
	return pc.Auth
}
