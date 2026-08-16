package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// DiskTokens is what an existing Codex CLI login left on disk.
type DiskTokens struct {
	AccessToken  string
	RefreshToken string
	AccountID    string
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
	}, true
}

// authFilePath is where the Codex CLI keeps its login. CODEX_HOME overrides
// the location, the same way the CLI itself honours it.
func authFilePath() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, "auth.json")
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, ".codex", "auth.json")
}
