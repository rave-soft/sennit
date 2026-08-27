package config

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/lock"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/providers/accounts"
)

// credentialWriteLockDeadline bounds how long a credential write (e.g.
// storing the token from a fresh interactive login) waits for the
// per-provider refresh lock. It is deliberately shorter than
// credentials.Manager's own refresh-lock deadline (45s) because a user is
// watching: if a peer is wedged we would rather write and risk a rare
// clobber than hang the UI.
const credentialWriteLockDeadline = 10 * time.Second

// UpdateProviderCredentials publishes a credential-only provider update. It
// is the sole runtime mutation path for provider credentials: callers must not
// mutate Config().Providers directly, because a runtime compiled before the
// mutation would otherwise remain cache-valid and continue using old clients.
// The provider's model identity is preserved by the caller; incrementing the
// store version forces future runtime compilation to construct a provider with
// the newly published credentials.
//
// It is a thin wrapper over UpdateProviderAccount with an AccountCredential
// carrying only APIKey and Token — ProxyURL nil, i.e. "leave the provider's
// effective proxy alone", and APIKeyTemplate empty, i.e. "leave
// ProviderConfig.APIKeyTemplate alone". This entry point predates accounts
// entirely; its callers (credentials.Manager, runtime_builder.go's token
// refresh path) are updating a token or a resolved key in place, not
// switching accounts, so neither the request route nor the template should
// move.
func (s *ConfigStore) UpdateProviderCredentials(providerID, apiKey string, token *oauth.Token) error {
	return s.UpdateProviderAccount(providerID, AccountCredential{APIKey: apiKey, Token: token})
}

// AccountCredential is everything publishing an account switch needs to
// hand ConfigStore.UpdateProviderAccount, gathered into one value because
// the mutator was outgrowing a positional-argument signature.
type AccountCredential struct {
	// APIKey is the value requests actually carry in Authorization — for
	// an API-key account this must already be resolved (no "$VAR" or
	// "$(cmd)" left in it), and for an OAuth account it is the token's
	// access token. Sending anything else here would put a raw template
	// on the wire, so callers resolve before constructing this.
	APIKey string
	// APIKeyTemplate is the unresolved form APIKey was resolved from
	// ("$VAR", "$(cmd)", or a literal key an account happens to store
	// verbatim). It is kept alongside the resolved APIKey because
	// ProviderConfig.APIKeyTemplate is what a later auth-error retry
	// re-resolves from (see runtime_builder.go's refreshApiKeyTemplate);
	// without republishing it here, a retry after an account switch
	// would re-resolve the PREVIOUS account's template. Empty means
	// "nothing to update" — the OAuth path, and UpdateProviderCredentials'
	// bare-token-refresh callers, have no template at all.
	APIKeyTemplate string
	// Token is the OAuth credential, nil for an API-key account.
	Token *oauth.Token
	// ProxyURL is the account's own proxy override; nil means "don't
	// touch the provider's effective proxy route", matching
	// UpdateProviderAccount's accountProxy parameter before this type
	// existed — see UpdateProviderAccount's doc comment for the full
	// resolution rule.
	ProxyURL *string
	// ActiveAccountID is the account STORE's own ID for the account being
	// activated (accounts.Account.ID) — not to be confused with
	// accounts.Account.AccountID, which is the provider's own identifier
	// for the account (e.g. Codex's chatgpt_account_id claim). An empty
	// value means "don't touch ProviderConfig.Account". Set this so the
	// in-memory config reflects which account is active immediately,
	// without waiting on a disk reload to catch up.
	ActiveAccountID string
}

// UpdateProviderAccount publishes a full account switch: credentials and,
// when cred.ProxyURL is non-nil, the account's own proxy override.
//
// cred.ProxyURL distinguishes "don't touch the proxy route" (nil, used by
// UpdateProviderCredentials) from "this account's proxy is exactly this"
// (non-nil). A non-nil value is resolved against the provider's own
// configured proxy via accounts.ResolveProxy before being published to
// ProviderConfig.ProxyURL — the field every request-sending call site
// actually reads — rather than written there directly: an empty string
// means "this account has no proxy of its own," which must fall back to
// the provider's, not clear the route entirely, and only
// ConfiguredProxyURL (set once at load time, never touched by an account
// switch) remembers what that provider-level value was. [proxyhttp.Direct]
// ("none") is a real value at either level, not emptiness, and beats
// everything below it — see ResolveProxy's doc comment for why that
// distinction matters. A plain string parameter could not carry "don't
// touch" vs. "clear to provider default" — hence the pointer.
//
// A provider with no config entry yet is not an error here: it is the
// ordinary state of a catalog provider (Codex, Copilot, ...) before its
// first sign-in — sennit.json has nothing for it until credentials are
// actually saved. This used to be a hard failure ("provider %s not
// found"), which broke a brand-new install's very first `sennit login
// codex`, since ActivateAccount (RecordAccount's caller) has no other
// path to create the entry. providerConfigFromCatalogLocked fabricates it
// from the embedded catalog exactly as SetProviderAPIKey's !exists branch
// already did — see that function's doc comment for why the two must
// share one implementation. A providerID that is not catalog-known
// either — a typo, or a custom provider nobody has configured — still
// fails, since there is nothing to fabricate an entry from.
func (s *ConfigStore) UpdateProviderAccount(providerID string, cred AccountCredential) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	current := s.Config()
	if current == nil {
		return fmt.Errorf("provider %s not found", providerID)
	}
	cfg := current.cloneForWrite()
	if cfg.Providers == nil {
		cfg.Providers = csync.NewMap[string, ProviderConfig]()
	}
	provider, ok := cfg.Providers.Get(providerID)
	if !ok {
		var err error
		provider, err = s.providerConfigFromCatalogLocked(providerID)
		if err != nil {
			return fmt.Errorf("provider %s has no config entry and %w", providerID, err)
		}
	}
	provider.APIKey = cred.APIKey
	provider.OAuthToken = cred.Token
	if cred.APIKeyTemplate != "" {
		provider.APIKeyTemplate = cred.APIKeyTemplate
	}
	if cred.ProxyURL != nil {
		provider.ProxyURL = accounts.ResolveProxy(*cred.ProxyURL, provider.ConfiguredProxyURL)
	}
	if cred.ActiveAccountID != "" {
		provider.Account = cred.ActiveAccountID
	}
	provider.ApplyPostCredentialSetup(providerID)
	cfg.Providers.Set(providerID, provider)
	s.credentialVersion.Add(1)
	s.setConfig(cfg)
	return nil
}

// ActivateAccount makes the given account the provider's active one: it
// persists the account's credentials and the "account" pointer to disk,
// then publishes the account's resolved credentials and effective proxy
// to the running config.
//
// Disk goes first, deliberately, because SetConfigFields's own reload
// rebuilds in-memory ProviderConfig from whatever is on disk at the time
// it runs. If the in-memory publish happened first (as this used to do),
// that reload — which knows nothing about the account switch, only
// about the file — would immediately overwrite the freshly-published
// credentials with whatever was on disk before this call, silently
// reverting the switch while the "account" pointer on disk kept pointing
// at the new account. Publishing after the reload is what makes the two
// stay in sync: the reload already picked up the raw api_key/oauth this
// call wrote, and the publish step adds the two things that never go to
// disk at all — the *resolved* API key (disk only ever gets the
// template) and the account's effective proxy override.
func (s *ConfigStore) ActivateAccount(scope Scope, providerID string, a accounts.Account) error {
	if err := a.Validate(); err != nil {
		return fmt.Errorf("account for provider %s is invalid: %w", providerID, err)
	}

	cred := AccountCredential{ProxyURL: &a.ProxyURL, ActiveAccountID: a.ID}
	fields := map[string]any{
		ProviderFieldKey(providerID, "account"): a.ID,
	}
	switch {
	case a.Token != nil:
		// Mirrors SetProviderAPIKey's *oauth.Token case: the access
		// token is what Authorization actually carries for an OAuth
		// provider, alongside the full token for refresh.
		cred.APIKey = a.Token.AccessToken
		cred.Token = a.Token
		fields[ProviderFieldKey(providerID, "api_key")] = a.Token.AccessToken
		fields[ProviderFieldKey(providerID, "oauth")] = a.Token
	default:
		resolved, err := s.Resolve(a.APIKey)
		if err != nil {
			return fmt.Errorf("resolving api key for account %s of provider %s: %w", a.ID, providerID, err)
		}
		if resolved == "" {
			return fmt.Errorf("api key for account %s of provider %s resolved to an empty value", a.ID, providerID)
		}
		cred.APIKey = resolved
		cred.APIKeyTemplate = a.APIKey
		// Account.APIKey is the unresolved template the user configured
		// (see its doc comment) — write that to disk, never the resolved
		// secret, which stays memory-only via cred.APIKey below.
		fields[ProviderFieldKey(providerID, "api_key")] = a.APIKey
	}

	if err := s.SetConfigFields(scope, fields); err != nil {
		return fmt.Errorf("persisting active account for provider %s: %w", providerID, err)
	}

	// Runs after the reload SetConfigFields just triggered, so its
	// publish of the resolved API key and effective proxy (neither of
	// which the reload could reconstruct from disk alone) is what the
	// running process ends up with, rather than being clobbered by it.
	return s.UpdateProviderAccount(providerID, cred)
}

// isCatalogProvider reports whether providerID is present in the embedded
// provider catalog. Only providers outside the catalog need their identity
// fields (type/base_url/name) persisted alongside credentials: a catalog
// provider's identity is reconstructed from the embedded list on every
// reload (see configureProviders), and pinning its base_url in the user's
// config file would freeze it against future catalog updates.
func (s *ConfigStore) isCatalogProvider(providerID string) bool {
	return s.findKnownProvider(providerID) != nil
}

// findKnownProvider looks up providerID in the embedded provider catalog,
// returning nil when the provider is not catalog-known (e.g. a custom
// OAuth provider the user configured by hand).
func (s *ConfigStore) findKnownProvider(providerID string) *catwalk.Provider {
	// Under the same lock KnownProviders takes: reloadFromDisk reassigns
	// the slice, and every caller of this reaches it from outside writeMu
	// (each one runs before the write it is preparing for takes the lock).
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	return s.findKnownProviderLocked(providerID)
}

// findKnownProviderLocked is findKnownProvider's body, for callers that
// already hold writeMu (in either mode) and would deadlock taking the
// RLock again — UpdateProviderAccount, notably, which needs a catalog
// lookup while already holding the exclusive lock for its clone-and-swap.
func (s *ConfigStore) findKnownProviderLocked(providerID string) *catwalk.Provider {
	for _, p := range s.knownProviders {
		if string(p.ID) == providerID {
			return &p
		}
	}
	return nil
}

// providerConfigFromCatalogLocked builds a fresh ProviderConfig for a
// catalog-known provider that has no config entry yet — the ordinary
// state of any catalog provider (Codex, Copilot, ...) before its first
// sign-in, not an exceptional one. Shared by SetProviderAPIKey and
// UpdateProviderAccount, both of which have to fabricate this entry the
// same way; letting the two copies drift is exactly how one of them
// silently stopped handling "no entry yet" (see UpdateProviderAccount's
// doc comment). Returns an error when providerID isn't a known provider
// at all. Caller must hold writeMu (either mode).
func (s *ConfigStore) providerConfigFromCatalogLocked(providerID string) (ProviderConfig, error) {
	found := s.findKnownProviderLocked(providerID)
	if found == nil {
		return ProviderConfig{}, fmt.Errorf("provider with ID %s not found in known providers", providerID)
	}
	return ProviderConfig{
		ID:           providerID,
		Name:         found.Name,
		BaseURL:      found.APIEndpoint,
		Type:         found.Type,
		Disable:      false,
		ExtraHeaders: make(map[string]string),
		ExtraParams:  make(map[string]string),
		Models:       found.Models,
	}, nil
}

// providerConfigFromCatalog is providerConfigFromCatalogLocked for callers
// that do not already hold writeMu (SetProviderAPIKey, notably, which
// computes this before taking the lock in its persist() closure).
func (s *ConfigStore) providerConfigFromCatalog(providerID string) (ProviderConfig, error) {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	return s.providerConfigFromCatalogLocked(providerID)
}

// SetProviderAPIKey sets the API key for a provider and persists it.
//
// Validation and the full providerConfig are assembled entirely in memory
// before anything is written to disk, so an unknown provider ID leaves no
// trace on disk (previously the api_key/oauth write for the string/token
// case happened first, and only then did the provider lookup that could
// fail).
func (s *ConfigStore) SetProviderAPIKey(scope Scope, providerID string, apiKey any) error {
	cfg := s.Config()
	providerConfig, exists := cfg.Providers.Get(providerID)
	if !exists {
		var err error
		providerConfig, err = s.providerConfigFromCatalog(providerID)
		if err != nil {
			return err
		}
	}

	fields := map[string]any{}
	var isToken bool

	switch v := apiKey.(type) {
	case string:
		providerConfig.APIKey = v
		fields[ProviderFieldKey(providerID, "api_key")] = v
	case *oauth.Token:
		isToken = true
		providerConfig.APIKey = v.AccessToken
		providerConfig.OAuthToken = v
		providerConfig.ApplyPostCredentialSetup(providerID)
		fields[ProviderFieldKey(providerID, "api_key")] = v.AccessToken
		fields[ProviderFieldKey(providerID, "oauth")] = v
	default:
		return fmt.Errorf("unsupported credential type %T for provider %s", apiKey, providerID)
	}

	// Custom providers outside the embedded catalog have nothing else to
	// reconstruct their identity from on reload, so persist it alongside
	// the credential. Catalog providers get it from the embedded list.
	if !s.isCatalogProvider(providerID) {
		fields[ProviderFieldKey(providerID, "type")] = providerConfig.Type
		fields[ProviderFieldKey(providerID, "base_url")] = providerConfig.BaseURL
		fields[ProviderFieldKey(providerID, "name")] = providerConfig.Name
	}

	persist := func() error {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		err := s.updateLocked(scope, func(cfg *Config) map[string]any {
			current, ok := cfg.Providers.Get(providerID)
			if !ok {
				current = providerConfig
			}
			current.APIKey = providerConfig.APIKey
			current.OAuthToken = providerConfig.OAuthToken
			current.ApplyPostCredentialSetup(providerID)
			cfg.Providers.Set(providerID, current)
			return fields
		})
		if err == nil {
			s.credentialVersion.Add(1)
		}
		return err
	}

	if isToken {
		// Hold the refresh lock across the write so a peer's in-flight
		// token exchange cannot land on top of a credential the user just
		// obtained interactively — which would silently invalidate the
		// login they only just completed.
		if err := s.withRefreshLock(providerID, persist); err != nil {
			return fmt.Errorf("failed to save credentials to config file: %w", err)
		}
		s.reloadNewProvider(exists, providerID)
		return nil
	}

	if err := persist(); err != nil {
		return fmt.Errorf("failed to save api key to config file: %w", err)
	}
	s.reloadNewProvider(exists, providerID)
	return nil
}

// reloadNewProvider re-runs the load pipeline after a credential write
// that introduced a provider the config did not have before.
//
// The entry SetProviderAPIKey publishes for a new provider is assembled by
// hand from the catalog record, which skips everything mergeCatalogProviders
// would have added on top: default headers, the Vertex/Azure extra params,
// the API-key template. Until something else happened to trigger a reload,
// the provider ran without them — and nothing here did trigger one, despite
// what the comment in workspace/custom_provider.go claims about every
// mutator reloading. A provider that already existed needs none of this:
// its entry came through the pipeline when it was loaded.
//
// Best-effort by design, exactly as the other autoReload sites are: the
// credential is already on disk, and a reload that could not run leaves the
// in-memory entry no worse than it was before this call.
func (s *ConfigStore) reloadNewProvider(existed bool, providerID string) {
	if existed {
		return
	}
	if err := s.autoReload(context.Background()); err != nil {
		slog.Warn("Failed to reload config after adding a provider", "provider", providerID, "error", err)
	}
}

// PersistRefreshedToken writes a refreshed OAuth token's credential
// fields to disk and republishes the in-memory config. Called by
// credentials.Manager after a successful refresh; kept on ConfigStore
// because it needs isCatalogProvider and update, both store internals
// that credentials must not reach into directly.
func (s *ConfigStore) PersistRefreshedToken(scope Scope, providerID string, cfg ProviderConfig, token *oauth.Token) error {
	fields := map[string]any{
		ProviderFieldKey(providerID, "api_key"): token.AccessToken,
		ProviderFieldKey(providerID, "oauth"):   token,
	}
	// Persist identity fields for providers outside the embedded catalog —
	// see isCatalogProvider. Without this, a custom OAuth provider's
	// type/base_url/name live only in memory: the next reload rebuilds
	// Config from disk (configureProviders), finds no base_url for this
	// provider, and drops it as an invalid custom provider.
	if !s.isCatalogProvider(providerID) {
		fields[ProviderFieldKey(providerID, "type")] = cfg.Type
		fields[ProviderFieldKey(providerID, "base_url")] = cfg.BaseURL
		fields[ProviderFieldKey(providerID, "name")] = cfg.Name
	}

	// Use update() rather than SetConfigFields: applyToken already
	// published the refreshed token in memory via UpdateProviderCredentials's
	// clone-and-swap, so the full disk-reparse-and-reload that
	// SetConfigFields triggers is unnecessary here and would discard
	// anything else this refresh doesn't own.
	if err := s.update(scope, func(*Config) map[string]any { return fields }); err != nil {
		return fmt.Errorf("failed to persist refreshed token: %w", err)
	}
	return nil
}

// withRefreshLock runs fn while holding the per-provider cross-process
// refresh lock, so a credential write cannot interleave with a peer's
// token exchange. Acquisition is best effort: when the lock cannot be
// taken in time, fn runs anyway rather than blocking a write the user is
// waiting on.
func (s *ConfigStore) withRefreshLock(providerID string, fn func() error) error {
	ctx, cancel := context.WithTimeout(context.Background(), credentialWriteLockDeadline)
	defer cancel()
	release, err := lock.File(ctx, s.RefreshLockPath(providerID))
	if err != nil {
		slog.Warn("Writing credentials without the refresh lock", "provider", providerID, "error", err)
		return fn()
	}
	defer release()
	return fn()
}

// RefreshLockPath returns the path to the per-provider cross-process refresh
// lock file. Lock files live under a dedicated locks/ subdirectory of the
// data dir so they do not clutter the config directory. The file is created
// on demand by lock.File and is never removed (flock keys on inode, not
// path).
//
// providerID is untrusted the same way ProviderFieldKey's is (see its doc
// comment): a custom provider's ID is free text, with nothing upstream
// restricting it to filesystem-safe characters. url.PathEscape neutralizes
// '/' (and any other separator) before it reaches filepath.Join, so a
// provider ID like "../../evil" can no longer walk the result out of the
// locks directory — with no literal separator left in the string, a lone
// ".." is just three dots inside one filename, not a parent-directory
// reference. Ordinary IDs (letters, digits, '.', '-', '_') round-trip
// unescaped, so this does not change the lock path for any provider ID in
// normal use.
func (s *ConfigStore) RefreshLockPath(providerID string) string {
	dir := filepath.Join(filepath.Dir(s.globalDataPath), "locks")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, fmt.Sprintf("%s.refresh.lock", url.PathEscape(providerID)))
}
