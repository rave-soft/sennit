package config

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/stretchr/testify/require"
)

// codexJWTWithEmail builds an unsigned token carrying both the account id
// and the profile email, the way a real Codex token does — the two live in
// different claim namespaces, which is the thing worth getting right here.
func codexJWTWithEmail(t *testing.T, accountID, email string) string {
	t.Helper()

	claims := map[string]any{
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": accountID},
		"https://api.openai.com/profile": map[string]any{"email": email},
		"exp":                            time.Now().Add(10 * 24 * time.Hour).Unix(),
	}
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

// TestBackfillCodexIdentity_FillsEmailAndReplacesTheUUIDLabel is the case
// this exists for: accounts recorded before the email was read show a UUID,
// while the address has been sitting in the token on disk all along.
func TestBackfillCodexIdentity_FillsEmailAndReplacesTheUUIDLabel(t *testing.T) {
	t.Parallel()

	accStore := newTestAccountsStore(t)
	const accountID = "932d69c9-ed10-4079-8fb9-abbe8e572979"
	require.NoError(t, accStore.Upsert(codex.ProviderID, accounts.Account{
		ID:        "acc_1",
		Label:     accountID, // the automatic fallback, not a person's choice
		AccountID: accountID,
		Token:     &oauth.Token{AccessToken: codexJWTWithEmail(t, accountID, "someone@example.com")},
	}))

	require.NoError(t, backfillCodexIdentity(accStore, codex.ProviderID))

	list, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "someone@example.com", list[0].Email)
	require.Equal(t, "someone@example.com", list[0].Label, "a label that was only the UUID is replaced")
}

// TestBackfillCodexIdentity_KeepsAChosenLabel: renaming an account is a
// deliberate act and the backfill must not undo it. The email is still
// recorded — nothing keys off the label, so both can be true.
func TestBackfillCodexIdentity_KeepsAChosenLabel(t *testing.T) {
	t.Parallel()

	accStore := newTestAccountsStore(t)
	require.NoError(t, accStore.Upsert(codex.ProviderID, accounts.Account{
		ID:        "acc_1",
		Label:     "Work",
		AccountID: "acct-1",
		Token:     &oauth.Token{AccessToken: codexJWTWithEmail(t, "acct-1", "someone@example.com")},
	}))

	require.NoError(t, backfillCodexIdentity(accStore, codex.ProviderID))

	list, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)
	require.Equal(t, "Work", list[0].Label, "a label the person chose is theirs")
	require.Equal(t, "someone@example.com", list[0].Email)
}

// TestBackfillCodexIdentity_IsIdempotent: it runs on every read of the
// accounts list, so a second pass must write nothing and change nothing.
func TestBackfillCodexIdentity_IsIdempotent(t *testing.T) {
	t.Parallel()

	accStore := newTestAccountsStore(t)
	require.NoError(t, accStore.Upsert(codex.ProviderID, accounts.Account{
		ID:        "acc_1",
		Label:     "acct-1",
		AccountID: "acct-1",
		Token:     &oauth.Token{AccessToken: codexJWTWithEmail(t, "acct-1", "someone@example.com")},
	}))

	require.NoError(t, backfillCodexIdentity(accStore, codex.ProviderID))
	first, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)

	require.NoError(t, backfillCodexIdentity(accStore, codex.ProviderID))
	second, err := accStore.List(codex.ProviderID)
	require.NoError(t, err)

	require.Equal(t, first, second)
}

// TestBackfillCodexIdentity_LeavesOtherProvidersAndTokenlessAccounts: the
// claim is Codex's, and an API-key account has no token to read it from.
func TestBackfillCodexIdentity_LeavesOtherProvidersAndTokenlessAccounts(t *testing.T) {
	t.Parallel()

	accStore := newTestAccountsStore(t)
	require.NoError(t, accStore.Upsert("openai", accounts.Account{ID: "acc_1", Label: "k", APIKey: "$KEY"}))
	require.NoError(t, accStore.Upsert(codex.ProviderID, accounts.Account{ID: "acc_1", Label: "k", APIKey: "$KEY"}))

	require.NoError(t, backfillCodexIdentity(accStore, "openai"))
	require.NoError(t, backfillCodexIdentity(accStore, codex.ProviderID))

	for _, provider := range []string{"openai", codex.ProviderID} {
		list, err := accStore.List(provider)
		require.NoError(t, err)
		require.Empty(t, list[0].Email)
		require.Equal(t, "k", list[0].Label)
	}
}
