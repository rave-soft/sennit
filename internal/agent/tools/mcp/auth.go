package mcp

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	mcpoauth "github.com/rave-soft/sennit/internal/oauth/mcp"
	"golang.org/x/oauth2"
)

// suppressBrowserKey marks a context as requesting the OAuth handler not
// open a local browser; the caller surfaces the authorization URL itself.
type suppressBrowserKey struct{}

// PendingAuthServer describes an MCP server awaiting OAuth.
type PendingAuthServer struct {
	Name string
	URL  string
}

type ownedAuthHandler struct {
	handler   *mcpoauth.Handler
	closeOnce sync.Once
	closeFn   func()
}

func newOwnedAuthHandler(handler *mcpoauth.Handler) *ownedAuthHandler {
	return &ownedAuthHandler{handler: handler, closeFn: handler.Close}
}

func (h *ownedAuthHandler) Close() {
	if h != nil {
		h.closeOnce.Do(h.closeFn)
	}
}

type authPublication struct {
	auth    *ownedAuthHandler
	gen     uint64
	attempt uint64
}

// authFlow is the auth-coordinator's bookkeeping for one in-flight
// BeginAuth call: owner is the attempt that owns the server for the
// duration of the flow, and finishOnce/done separate "the caller's wait
// bounded out" (abortAuthFlow) from "the worker actually returned"
// (completeAuthFlow) — see the doc on each in authcoordinator.go.
type authFlow struct {
	cancel     context.CancelFunc
	done       chan struct{}
	workerDone chan struct{}
	owner      attemptID
	lock       *sync.Mutex
	err        error
	finishOnce sync.Once
}

// usesOAuth is deliberately transport-agnostic: both HTTP transports share the
// same startup, renewal and explicit-auth policy.
func usesOAuth(m config.MCPConfig) bool {
	return m.OAuth && (m.Type == config.MCPHttp || m.Type == config.MCPSSE)
}

// hasUsableToken returns true if the saved OAuth token has an access
// token that can be used or refreshed. A token with an empty access
// token is structurally invalid and should be treated as missing.
func hasUsableToken(tok *oauth.Token) bool {
	return tok != nil && tok.AccessToken != ""
}

// isOAuthInitErr returns true if the error indicates the OAuth token
// is missing, no longer valid, or cannot be refreshed. This covers:
//   - invalid_grant: expired or revoked refresh tokens
//   - invalid_client: deleted or deactivated client registrations
//   - "no token available": the handler had no cached token to use
//   - interactive authorization was required but withheld during startup
func isOAuthInitErr(err error) bool {
	if errors.Is(err, mcpoauth.ErrInteractiveAuthRequired) {
		return true
	}
	var rErr *oauth2.RetrieveError
	if errors.As(err, &rErr) {
		return rErr.ErrorCode == "invalid_grant" || rErr.ErrorCode == "invalid_client"
	}
	msg := err.Error()
	return strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "invalid_client") ||
		strings.Contains(msg, "no token available")
}
