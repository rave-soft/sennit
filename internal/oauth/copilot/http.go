package copilot

import (
	"net/http"
	"time"

	"github.com/rave-soft/sennit/internal/proxyhttp"
)

const (
	userAgent           = "GitHubCopilotChat/0.32.4"
	editorVersion       = "vscode/1.105.1"
	editorPluginVersion = "copilot-chat/0.32.4"
	integrationID       = "vscode-chat"
)

func Headers() map[string]string {
	return map[string]string{
		"User-Agent":             userAgent,
		"Editor-Version":         editorVersion,
		"Editor-Plugin-Version":  editorPluginVersion,
		"Copilot-Integration-Id": integrationID,
	}
}

// httpClient builds the client every call in this package makes.
//
// Sign-in has to honour the proxy just as much as the model calls do: for
// a user who can only reach GitHub through one, a device code request or
// token exchange that ignored it would fail while the provider itself
// looked correctly configured.
func httpClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	if proxyURL == "" {
		return &http.Client{Timeout: timeout}, nil
	}
	return proxyhttp.NewClient(proxyURL, timeout)
}
