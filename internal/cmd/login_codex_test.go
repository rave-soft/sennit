package cmd

import (
	"errors"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/stretchr/testify/require"
)

// codexProviderConfigAccessor extends stubConfigAccessor with a Config()
// that reports a single Codex provider entry, for configuredCodexProxy's
// tests.
type codexProviderConfigAccessor struct {
	stubConfigAccessor
	provider config.ProviderConfig
}

func (s *codexProviderConfigAccessor) Config() *config.Config {
	return &config.Config{
		Providers: csync.NewMap(map[string]config.ProviderConfig{
			codex.ProviderID: s.provider,
		}),
	}
}

// TestConfiguredCodexProxy_UsesConfiguredNotEffective guards the fix for a
// bug where loginCodex's no-flag proxy fallback read ProxyURL — the
// *effective* proxy, resolved for whichever account happened to be active
// — instead of ConfiguredProxyURL, the provider-level default. Reading the
// effective value would promote one account's proxy (or "none", forcing a
// direct connection) to every account's default on the next `sennit login
// codex` with no --proxy flag.
func TestConfiguredCodexProxy_UsesConfiguredNotEffective(t *testing.T) {
	t.Parallel()

	ws := &codexProviderConfigAccessor{provider: config.ProviderConfig{
		ProxyURL:           "none",
		ConfiguredProxyURL: "socks5://configured-proxy:1080",
	}}

	require.Equal(t, "socks5://configured-proxy:1080", configuredCodexProxy(ws))
}

// TestConfiguredCodexProxy_NoProviderYet covers a first-ever login, where
// the Codex provider entry does not exist yet.
func TestConfiguredCodexProxy_NoProviderYet(t *testing.T) {
	t.Parallel()

	ws := &stubConfigAccessor{}
	require.Empty(t, configuredCodexProxy(ws))
}

// proxyRollbackConfigAccessor extends stubConfigAccessor with a
// SetConfigField that records its calls, which restoreCodexProxyField's
// tests need to assert on but the shared stub (SetConfigField is a no-op
// there) does not track.
type proxyRollbackConfigAccessor struct {
	stubConfigAccessor
	setCalls []setConfigFieldCall
}

type setConfigFieldCall struct {
	key   string
	value any
}

func (s *proxyRollbackConfigAccessor) SetConfigField(_ config.Scope, key string, value any) error {
	s.setCalls = append(s.setCalls, setConfigFieldCall{key, value})
	return s.errs[key]
}

// TestRestoreCodexProxyField_RestoresPreviousValue pins the fix for
// loginCodex's proxy_url write and RecordAccount not being all-or-nothing:
// when a login fails after the proxy field was already persisted, and a
// previous value existed, the rollback must put that value back rather
// than leaving the new one in place with no account recorded to show for
// it.
func TestRestoreCodexProxyField_RestoresPreviousValue(t *testing.T) {
	t.Parallel()

	ws := &proxyRollbackConfigAccessor{}
	restoreCodexProxyField(ws, true, "socks5://old-proxy:1080")

	require.Empty(t, ws.removed)
	require.Equal(t, []setConfigFieldCall{
		{key: "providers.codex.proxy_url", value: "socks5://old-proxy:1080"},
	}, ws.setCalls)
}

// TestRestoreCodexProxyField_RemovesFieldWhenThereWasNone covers the other
// half: a login that set the field where none existed before must remove
// it again on rollback, not leave a stray empty/zero value behind.
func TestRestoreCodexProxyField_RemovesFieldWhenThereWasNone(t *testing.T) {
	t.Parallel()

	ws := &proxyRollbackConfigAccessor{}
	restoreCodexProxyField(ws, false, "")

	require.Empty(t, ws.setCalls)
	require.Equal(t, []string{"providers.codex.proxy_url"}, ws.removed)
}

// TestRestoreCodexProxyField_RollbackFailureDoesNotPanic covers the
// best-effort path: a failure while rolling back must be swallowed (only
// logged), since the caller already has the real login error to return
// and a rollback failure must not replace or mask it.
func TestRestoreCodexProxyField_RollbackFailureDoesNotPanic(t *testing.T) {
	t.Parallel()

	ws := &proxyRollbackConfigAccessor{
		stubConfigAccessor: stubConfigAccessor{
			errs: map[string]error{"providers.codex.proxy_url": errors.New("write failed")},
		},
	}
	require.NotPanics(t, func() {
		restoreCodexProxyField(ws, true, "socks5://old-proxy:1080")
	})
}
