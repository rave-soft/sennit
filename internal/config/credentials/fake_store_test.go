package credentials

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// fakeStore is a minimal in-memory + on-disk implementation of Store for
// tests. It deliberately does not load or parse a full config.Config from
// disk (no config.Load, no config.Init): Manager only ever reads/writes
// individual credential fields, so a fake that tracks just those fields
// is enough to exercise the real refresh/lock/adopt logic without dragging
// in the rest of the config package's disk-loading machinery.
type fakeStore struct {
	mu         sync.Mutex
	cfg        *config.Config
	configPath string
	lockDir    string

	// persistFailures, when non-zero, makes the next N calls to
	// PersistRefreshedToken fail with errPersistFailed instead of
	// writing, then lets subsequent calls succeed normally. Used to
	// exercise refreshOAuthTokenLocked's persist-retry path.
	persistFailures  int
	persistCallCount int
}

var errPersistFailed = fmt.Errorf("simulated persist failure")

// newFakeStore builds a fakeStore backed by a real (but hand-written)
// config file on disk at configPath, so loadTokenFromDisk / gjson reads
// behave exactly as they do against a real ConfigStore. lockDir holds the
// per-provider refresh lock files, mirroring RefreshLockPath's own
// dedicated locks/ subdirectory.
func newFakeStore(cfg *config.Config, configPath, lockDir string) *fakeStore {
	return &fakeStore{cfg: cfg, configPath: configPath, lockDir: lockDir}
}

func (f *fakeStore) Config() *config.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

func (f *fakeStore) ConfigPath(config.Scope) (string, error) {
	return f.configPath, nil
}

func (f *fakeStore) RefreshLockPath(providerID string) string {
	_ = os.MkdirAll(f.lockDir, 0o755)
	return filepath.Join(f.lockDir, fmt.Sprintf("%s.refresh.lock", providerID))
}

func (f *fakeStore) HasConfigField(_ config.Scope, key string) bool {
	data, err := os.ReadFile(f.configPath)
	if err != nil {
		return false
	}
	return gjson.GetBytes(data, key).Exists()
}

func (f *fakeStore) UpdateProviderCredentials(providerID, apiKey string, token *oauth.Token) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.cfg.Providers.Get(providerID); !ok {
		return fmt.Errorf("provider %s not found", providerID)
	}
	rp, ok := f.cfg.RuntimeProvider(providerID)
	if !ok {
		return fmt.Errorf("provider %s not found", providerID)
	}
	rp.APIKey = apiKey
	rp.OAuthToken = token
	f.cfg.SetRuntimeProvider(providerID, rp)
	return nil
}

func (f *fakeStore) SetProviderAPIKey(_ config.Scope, providerID string, apiKey any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.cfg.Providers.Get(providerID)
	if !ok {
		return fmt.Errorf("provider %s not found", providerID)
	}
	rp, ok := f.cfg.RuntimeProvider(providerID)
	if !ok {
		return fmt.Errorf("provider %s not found", providerID)
	}
	switch v := apiKey.(type) {
	case string:
		p.APIKey = v
		rp.APIKey = v
	case *oauth.Token:
		p.APIKey = v.AccessToken
		p.OAuthToken = v
		rp.APIKey = v.AccessToken
		rp.OAuthToken = v
	default:
		return fmt.Errorf("unsupported credential type %T for provider %s", apiKey, providerID)
	}
	f.cfg.Providers.Set(providerID, p)
	f.cfg.SetRuntimeProvider(providerID, rp)
	return f.writeFields(providerID, p)
}

func (f *fakeStore) PersistRefreshedToken(_ config.Scope, providerID string, _ config.ProviderConfig, token *oauth.Token) error {
	f.mu.Lock()
	f.persistCallCount++
	fail := f.persistCallCount <= f.persistFailures
	f.mu.Unlock()
	if fail {
		return errPersistFailed
	}
	return f.writeFields(providerID, config.ProviderConfig{APIKey: token.AccessToken, OAuthToken: token})
}

// writeFields persists the api_key/oauth fields to the fake's config file.
// Callers (RefreshOAuthToken, via the per-provider refresh lock held across
// refreshOAuthTokenLocked) already serialise concurrent access for the same
// provider, so no extra locking is needed here.
func (f *fakeStore) writeFields(providerID string, p config.ProviderConfig) error {
	data, err := os.ReadFile(f.configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		data = []byte("{}")
	}
	v := string(data)
	v, err = sjson.Set(v, config.ProviderFieldKey(providerID, "api_key"), p.APIKey)
	if err != nil {
		return err
	}
	v, err = sjson.Set(v, config.ProviderFieldKey(providerID, "oauth"), p.OAuthToken)
	if err != nil {
		return err
	}
	return os.WriteFile(f.configPath, []byte(v), 0o600)
}
