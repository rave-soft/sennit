package config

import (
	"context"
	"fmt"

	"github.com/rave-soft/sennit/internal/providers/accounts"
)

// AccountsService coordinates account persistence with the configuration
// publication APIs that make the selected account live.
type AccountsService struct {
	store    *ConfigStore
	accounts accounts.Store
	fetch    AccountUsageFetcher
}

// NewAccountsService constructs the account orchestration service from the
// configuration store, persistent account store, and optional usage fetcher.
func NewAccountsService(store *ConfigStore, accountStore accounts.Store, fetch AccountUsageFetcher) *AccountsService {
	return &AccountsService{store: store, accounts: accountStore, fetch: fetch}
}

// Record stores cred as an account and makes it active.
func (s *AccountsService) Record(scope Scope, providerID string, cred accounts.LegacyCredential) (accounts.Account, error) {
	return RecordAccount(s.store, s.accounts, scope, providerID, cred)
}

// List returns accounts for providerID after migrating a legacy credential.
func (s *AccountsService) List(providerID string) ([]accounts.Account, error) {
	if err := EnsureAccountMigrated(s.store, s.accounts, providerID); err != nil {
		return nil, err
	}
	return s.accounts.List(providerID)
}

// Activate makes accountID the active account for providerID.
func (s *AccountsService) Activate(scope Scope, providerID, accountID string) error {
	account, ok, err := s.accounts.Get(providerID, accountID)
	if err != nil {
		return fmt.Errorf("looking up account %s for provider %s: %w", accountID, providerID, err)
	}
	if !ok {
		return fmt.Errorf("account %s not found for provider %s", accountID, providerID)
	}
	return s.store.ActivateAccount(scope, providerID, account)
}

// Update saves account and republishes it when it is active.
func (s *AccountsService) Update(providerID string, account accounts.Account) error {
	return UpdateAccount(s.store, s.accounts, providerID, account)
}

// Remove deletes an account while preserving an active replacement.
func (s *AccountsService) Remove(scope Scope, providerID, accountID string) error {
	return RemoveAccount(s.store, s.accounts, scope, providerID, accountID)
}

// Purge removes every account and clears the active account pointer.
func (s *AccountsService) Purge(scope Scope, providerID string) error {
	return PurgeAccounts(s.store, s.accounts, scope, providerID)
}

// SetProviderProxy updates the provider proxy and republishes its active account.
func (s *AccountsService) SetProviderProxy(providerID, proxy string) error {
	return SetProviderProxy(s.store, s.accounts, providerID, proxy)
}

// RefreshLimits refreshes stored usage snapshots when the provider supports them.
func (s *AccountsService) RefreshLimits(ctx context.Context, providerID string) ([]accounts.Account, error) {
	return RefreshAccountLimits(ctx, s.store, s.accounts, providerID, s.fetch)
}
