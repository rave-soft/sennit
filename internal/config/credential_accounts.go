package config

import "github.com/rave-soft/sennit/internal/providers/accounts"

// ListAccounts returns the provider's stored accounts from the global
// accounts file.
func (s *ConfigStore) ListAccounts(providerID string) ([]accounts.Account, error) {
	return accounts.NewFileStore(GlobalAccountsFile()).List(providerID)
}

// UpsertAccount inserts or replaces one account in the global accounts
// file.
func (s *ConfigStore) UpsertAccount(providerID string, a accounts.Account) error {
	return accounts.NewFileStore(GlobalAccountsFile()).Upsert(providerID, a)
}
