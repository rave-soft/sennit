package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/rave-soft/sennit/internal/oauth"
)

// DiskTokens is what an existing Codex CLI login left on disk.
type DiskTokens struct {
	AccessToken  string
	RefreshToken string
	AccountID    string
	// ProxyURL is the proxy the Codex CLI is configured to use, if any.
	// Someone who needs a proxy to reach OpenAI has already told the CLI
	// about it, and asking them to type it again is asking twice for the
	// same fact.
	ProxyURL string
}

// Token turns an on-disk login into a token Sennit can use directly, and
// reports whether the access token still has enough life left to be worth
// it (see Usable).
//
// This is what keeps an import from spending the CLI's refresh token. That
// token is single-use: exchanging it hands us a new pair and invalidates
// the one the CLI still has on disk, so the CLI is logged out by an import
// it was never asked about. An access token good for another week does the
// same job and costs nobody anything.
func (d DiskTokens) Token() (*oauth.Token, bool) {
	if !Usable(d.AccessToken) {
		return nil, false
	}
	token := &oauth.Token{
		AccessToken:  d.AccessToken,
		RefreshToken: d.RefreshToken,
		ExpiresAt:    ExpiresAt(d.AccessToken).Unix(),
	}
	token.SetExpiresIn()
	return token, true
}

// TokensFromDisk reads the credentials of an already signed-in Codex CLI, so
// a user who has one does not have to run a second browser flow for Sennit.
// It reports ok=false when the file is missing, unreadable, or carries no
// refresh token — every one of which just means "no login to import".
//
// Only the refresh token is load-bearing: the access token there may well be
// expired, and Sennit exchanges the refresh token for its own pair anyway,
// so nothing here is trusted as a live credential.
func TokensFromDisk() (DiskTokens, bool) {
	data, err := os.ReadFile(authFilePath())
	if err != nil {
		return DiskTokens{}, false
	}
	var content struct {
		Tokens struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &content); err != nil {
		return DiskTokens{}, false
	}
	if content.Tokens.RefreshToken == "" {
		return DiskTokens{}, false
	}

	accountID := content.Tokens.AccountID
	if accountID == "" {
		accountID = AccountID(content.Tokens.IDToken)
	}
	return DiskTokens{
		AccessToken:  content.Tokens.AccessToken,
		RefreshToken: content.Tokens.RefreshToken,
		AccountID:    accountID,
		ProxyURL:     ProxyFromDisk(),
	}, true
}

// TokenFromDiskFor returns a token the Codex CLI already holds for the
// given account, when that token is still usable.
//
// It exists for the refresh path. An imported login shares the CLI's
// single-use refresh token, so exchanging it silently logs the CLI out. The
// CLI refreshes on its own schedule, so before spending that token it is
// worth asking whether it has already produced a newer one — which costs a
// file read and saves someone from having to sign in to Codex again.
//
// accountID guards against adopting a token for somebody else: the CLI may
// have been signed in to a different account since the import, and using
// its token would quietly move the session to that account's allowance.
func TokenFromDiskFor(accountID string) (*oauth.Token, bool) {
	disk, ok := TokensFromDisk()
	if !ok {
		return nil, false
	}
	if accountID != "" && AccountID(disk.AccessToken) != accountID {
		return nil, false
	}
	return disk.Token()
}

// ProxyFromDisk reads the proxy out of the Codex CLI's config, returning ""
// when there is none to read.
//
// It scans for the keys rather than parsing TOML properly: the file is the
// CLI's, not ours, and pulling in a TOML dependency to read one string from
// it would be a poor trade. Both plausible table names are accepted because
// the CLI takes either without complaint, and a socks URL counts too — for
// someone behind a SOCKS proxy that is the address that matters.
//
// Getting this wrong is cheap in both directions: an unfound proxy just
// means the field starts empty, and a value found under a key the user
// wrote by hand is the value they meant either way.
func ProxyFromDisk() string {
	data, err := os.ReadFile(configFilePath())
	if err != nil {
		return ""
	}

	var (
		table   string
		proxy   string
		socks   string
		accepts = map[string]bool{"": true, "network": true, "network_proxy": true}
	)
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			table = strings.Trim(line, "[]")
			continue
		}
		if !accepts[table] {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if value == "" {
			continue
		}
		switch strings.TrimSpace(key) {
		case "proxy_url":
			proxy = value
		case "socks_url":
			socks = value
		}
	}

	// An explicit proxy_url wins; socks_url is the fallback for a
	// SOCKS-only setup.
	if proxy != "" {
		return proxy
	}
	return socks
}

// authFilePath is where the Codex CLI keeps its login. CODEX_HOME overrides
// the location, the same way the CLI itself honours it.
func authFilePath() string {
	return codexHomeFile("auth.json")
}

// configFilePath is the Codex CLI's own settings file.
func configFilePath() string {
	return codexHomeFile("config.toml")
}

func codexHomeFile(name string) string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, name)
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, ".codex", name)
}
