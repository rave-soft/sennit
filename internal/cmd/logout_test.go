package cmd

import (
	"context"
	"fmt"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/stretchr/testify/require"
)

// stubConfigAccessor is a minimal workspace.ConfigAccessor fake for
// exercising the logoutXxx helpers without a real config store. Only
// RemoveConfigField is meaningful; every other method is unused by the
// helpers under test and just returns a zero value.
type stubConfigAccessor struct {
	// removed records every key passed to RemoveConfigField, in call
	// order, so tests can assert every field was attempted even when an
	// earlier call errored.
	removed []string
	// errs maps a config key to the error RemoveConfigField should
	// return for it.
	errs map[string]error
}

func (s *stubConfigAccessor) Config() *config.Config { return nil }
func (s *stubConfigAccessor) CurrentPlanUsage(string) (accounts.Usage, bool) {
	return accounts.Usage{}, false
}

func (s *stubConfigAccessor) WorkingDir() string                { return "" }
func (s *stubConfigAccessor) Resolver() config.VariableResolver { return nil }

func (s *stubConfigAccessor) UpdatePreferredModel(config.Scope, config.SelectedModel) error {
	return nil
}

func (s *stubConfigAccessor) OverridePreferredModel(config.SelectedModel) error { return nil }
func (s *stubConfigAccessor) SetCompactMode(config.Scope, bool) error           { return nil }
func (s *stubConfigAccessor) SetProviderAPIKey(config.Scope, string, any) error { return nil }
func (s *stubConfigAccessor) SetConfigField(config.Scope, string, any) error    { return nil }

func (s *stubConfigAccessor) RemoveConfigField(_ config.Scope, key string) error {
	s.removed = append(s.removed, key)
	return s.errs[key]
}

func (s *stubConfigAccessor) RecordAccount(config.Scope, string, accounts.LegacyCredential) (accounts.Account, error) {
	return accounts.Account{}, nil
}

func (s *stubConfigAccessor) ListAccounts(string) ([]accounts.Account, error)    { return nil, nil }
func (s *stubConfigAccessor) ActivateAccount(config.Scope, string, string) error { return nil }
func (s *stubConfigAccessor) UpdateAccount(string, accounts.Account) error       { return nil }
func (s *stubConfigAccessor) RemoveAccount(config.Scope, string, string) error   { return nil }
func (s *stubConfigAccessor) SetProviderProxy(string, string) error              { return nil }

func (s *stubConfigAccessor) RefreshAccountLimits(context.Context, string) ([]accounts.Account, error) {
	return nil, nil
}

func (s *stubConfigAccessor) ImportCopilot() (*oauth.Token, bool) { return nil, false }

func (s *stubConfigAccessor) RefreshOAuthToken(context.Context, config.Scope, string) error {
	return nil
}

// TestLogoutCodex_RemovesAllFieldsAndReturnsFirstError guards the
// cmp.Or -> explicit-checks rewrite in logoutProvider: every field must
// still be removed even when an earlier removal fails (cmp.Or evaluated
// both of its arguments unconditionally, and the rewrite must keep doing
// so), and the first error must be the one returned. It also covers
// logoutCodex's three fields, checking that an error on the middle field
// still lets the third removal run and that the middle field's error wins
// over what would be a later error too.
func TestLogoutCodex_RemovesAllFieldsAndReturnsFirstError(t *testing.T) {
	t.Parallel()

	wantErr := fmt.Errorf("oauth removal failed")
	ws := &stubConfigAccessor{errs: map[string]error{
		"providers.codex.oauth":  wantErr,
		"providers.codex.models": fmt.Errorf("models removal failed"),
	}}

	err := logoutCodex(ws)
	require.ErrorIs(t, err, wantErr)
	require.Equal(t, []string{
		"providers.codex.api_key",
		"providers.codex.oauth",
		"providers.codex.models",
	}, ws.removed)
}

func TestLogoutCmd_Aliases(t *testing.T) {
	t.Parallel()

	require.Equal(t, "signout", logoutCmd.Aliases[0])
}

func TestLogoutCmd_HasForceFlag(t *testing.T) {
	t.Parallel()

	flag := logoutCmd.Flags().Lookup("force")
	require.NotNil(t, flag)
	require.Equal(t, "f", flag.Shorthand)
	require.Equal(t, "false", flag.DefValue)
}

func TestLogoutCmd_ValidArgs(t *testing.T) {
	t.Parallel()

	validPlatforms := map[string]bool{}
	for _, p := range logoutCmd.ValidArgs {
		validPlatforms[p] = true
	}
	require.False(t, validPlatforms["hyper"])
	require.True(t, validPlatforms["copilot"])
	require.True(t, validPlatforms["github"])
	require.True(t, validPlatforms["github-copilot"])
}
