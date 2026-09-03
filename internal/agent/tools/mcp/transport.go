package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/env"
	"github.com/rave-soft/sennit/internal/home"
	mcpoauth "github.com/rave-soft/sennit/internal/oauth/mcp"
)

func (r *Registry) buildHTTPTransport(ctx context.Context, cfg ConfigProvider, name string, m config.MCPConfig, gen, attempt uint64, resolver config.VariableResolver) (string, http.RoundTripper, *mcpoauth.Handler, error) {
	url, err := m.ResolvedURL(resolver)
	if err != nil {
		return "", nil, nil, err
	}
	if strings.TrimSpace(url) == "" {
		return "", nil, nil, fmt.Errorf("mcp %s config requires a non-empty 'url' field", m.Type)
	}
	headers, err := m.ResolvedHeaders(resolver)
	if err != nil {
		return "", nil, nil, err
	}
	// The SDK's streamable connection performs its session DELETE before it
	// cancels its own connection context. Bound only DELETE requests here: a
	// client-wide timeout would also terminate the long-lived SSE GET stream.
	transport := http.RoundTripper(&headerRoundTripper{headers: headers, base: newOwnedHTTPTransport()})
	var oauthHandler *mcpoauth.Handler
	if m.OAuth {
		oauthHandler, err = r.oauthSetup(ctx, cfg, name, m, gen, attempt, resolver, url)
		if err != nil {
			return "", nil, nil, err
		}
		transport = newOAuthRoundTripper(oauthHandler, transport)
	}
	return url, transport, oauthHandler, nil
}

func (r *Registry) createTransportFor(ctx context.Context, cfg ConfigProvider, name string, m config.MCPConfig, gen, attempt uint64, resolver config.VariableResolver) (mcp.Transport, *mcpoauth.Handler, error) {
	switch m.Type {
	case config.MCPStdio:
		command, err := resolver.ResolveValue(m.Command)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid mcp command: %w", err)
		}
		if strings.TrimSpace(command) == "" {
			return nil, nil, fmt.Errorf("mcp stdio config requires a non-empty 'command' field")
		}
		args, err := m.ResolvedArgs(resolver)
		if err != nil {
			return nil, nil, err
		}
		envs, err := m.ResolvedEnv(resolver)
		if err != nil {
			return nil, nil, err
		}
		cmd := exec.CommandContext(ctx, home.Long(command), args...)
		// Strip herdr pane-ownership vars: a user-configured stdio MCP
		// server is an arbitrary subprocess and must not be able to
		// attach to the parent pane's agent authority (see
		// env.WithoutHerdrEnv).
		cmd.Env = append(env.WithoutHerdrEnv(os.Environ()), envs...)
		// Run the child in its own process group and kill the whole group when
		// the session context is cancelled. A stdio server often spawns its own
		// children (signal-mcp launches signal-cli); os/exec's default
		// cancellation kills only the direct child, orphaning the rest with
		// PPID 1 — production accumulated 15+ such zombies over two days.
		configureStdioProcess(cmd)
		return &mcp.CommandTransport{
			Command: cmd,
		}, nil, nil
	case config.MCPHttp:
		url, transport, oauthHandler, err := r.buildHTTPTransport(ctx, cfg, name, m, gen, attempt, resolver)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: transport}}, oauthHandler, nil
	case config.MCPSSE:
		url, transport, oauthHandler, err := r.buildHTTPTransport(ctx, cfg, name, m, gen, attempt, resolver)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.SSEClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: transport}}, oauthHandler, nil
	default:
		return nil, nil, fmt.Errorf("unsupported mcp type: %s", m.Type)
	}
}

// streamableCloseTimeout bounds the SDK's best-effort session DELETE. The
// streamable SDK cancels its connection context only after that request returns;
// without this separate request deadline an unresponsive server can prevent
// Close from ever releasing its SSE reader and JSON-RPC workers.
const streamableCloseTimeout = time.Second

type headerRoundTripper struct {
	headers map[string]string
	base    http.RoundTripper
}

type ownedHTTPTransport struct{ *http.Transport }

func newOwnedHTTPTransport() *ownedHTTPTransport {
	return &ownedHTTPTransport{Transport: http.DefaultTransport.(*http.Transport).Clone()}
}

func (t *ownedHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.Transport.RoundTrip(req)
}

func closeIdleTransport(t mcp.Transport) func() {
	var rt http.RoundTripper
	switch v := unwrapTransport(t).(type) {
	case *mcp.StreamableClientTransport:
		rt = v.HTTPClient.Transport
	case *mcp.SSEClientTransport:
		rt = v.HTTPClient.Transport
	}
	for {
		switch v := rt.(type) {
		case *oauthRoundTripper:
			rt = v.base
		case *headerRoundTripper:
			rt = v.base
		case interface{ CloseIdleConnections() }:
			return v.CloseIdleConnections
		default:
			return nil
		}
	}
}

func (rt headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	for k, v := range rt.headers {
		if !strings.EqualFold(k, "Authorization") || clone.Header.Get("Authorization") == "" {
			clone.Header.Set(k, v)
		}
	}
	return roundTripWithCloseDeadline(rt.base, clone)
}

func roundTripWithCloseDeadline(base http.RoundTripper, req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodDelete {
		return base.RoundTrip(req)
	}
	ctx, cancel := context.WithTimeout(req.Context(), streamableCloseTimeout)
	defer cancel()
	return base.RoundTrip(req.Clone(ctx))
}

// oauthRoundTripper wraps an HTTP transport with OAuth bearer token
// injection and 401-triggered authorization. Used for SSE transports
// that don't support the SDK's OAuthHandler natively. Based on Bruno
// Krugel's implementation from PR #3396.
type oauthRoundTripper struct {
	base    http.RoundTripper
	handler auth.OAuthHandler
}

func newOAuthRoundTripper(handler auth.OAuthHandler, base http.RoundTripper) *oauthRoundTripper {
	return &oauthRoundTripper{base: base, handler: handler}
}

func (rt *oauthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodDelete {
		ctx, cancel := context.WithTimeout(req.Context(), streamableCloseTimeout)
		defer cancel()
		req = req.Clone(ctx)
	}
	request := cloneRequest(req)
	resp, err := rt.doRequestWithToken(request)
	if err != nil || (resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden) {
		return resp, err
	}
	// Handlers may inspect or close the failed response. Wrap the body so the
	// transport can guarantee closure without closing it twice.
	resp.Body = &onceCloseReadCloser{ReadCloser: resp.Body}
	defer resp.Body.Close()
	// req.Context() here is mcpCtx (connection.go's createSession), which
	// carries a deadline sized for "the server answered" (mcpTimeout: 10-30s)
	// — not for "the user finished an SSO login with 2FA in their browser".
	// Authorize's wait for the OAuth redirect must not inherit that deadline,
	// so it runs on a context stripped of it and given its own, much longer,
	// interactive bound instead. A hung or abandoned login still terminates
	// promptly: BeginAuth's abortAuthFlow / Registry.teardown close the
	// oauth.Handler on cancellation, and Handler.Close settles the pending
	// callback wait (callbackReceiver.close -> flight.settle) independently
	// of context cancellation, so this detachment does not leak the wait.
	authCtx, cancelAuth := context.WithTimeout(context.WithoutCancel(req.Context()), interactiveAuthTimeout)
	defer cancelAuth()
	if err := rt.handler.Authorize(authCtx, request, resp); err != nil {
		return nil, fmt.Errorf("oauth authorize: %w", err)
	}
	if req.Body != nil && req.GetBody == nil {
		return nil, errors.New("cannot retry OAuth request with a non-replayable body")
	}
	retry := cloneRequest(req)
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("reopen OAuth retry body: %w", err)
		}
		retry.Body = body
	}
	return rt.doRequestWithToken(retry)
}

type onceCloseReadCloser struct {
	io.ReadCloser
	once sync.Once
}

func (c *onceCloseReadCloser) Close() error {
	var err error
	c.once.Do(func() { err = c.ReadCloser.Close() })
	return err
}

func cloneRequest(req *http.Request) *http.Request {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	return clone
}

func (rt *oauthRoundTripper) doRequestWithToken(req *http.Request) (*http.Response, error) {
	ts, err := rt.handler.TokenSource(req.Context())
	if err != nil {
		closeRequestBody(req)
		return nil, fmt.Errorf("oauth token source: %w", err)
	}
	if ts != nil {
		token, err := ts.Token()
		if err != nil {
			closeRequestBody(req)
			return nil, fmt.Errorf("oauth token: %w", err)
		}
		if token != nil {
			req.Header.Set("Authorization", "Bearer "+token.AccessToken)
		}
	}
	return rt.base.RoundTrip(req)
}

func closeRequestBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}

// interactiveAuthTimeout bounds how long an OAuth authorization wait
// (browser opened, waiting for the localhost redirect) may run once the
// server's own connect timeout has been set aside for it. Five minutes
// comfortably covers an SSO login with a second factor or an approval
// step on a corporate identity provider without leaving a forgotten
// browser tab able to hold the flow open indefinitely.
const interactiveAuthTimeout = 5 * time.Minute

func mcpTimeout(m config.MCPConfig) time.Duration {
	if m.Timeout > 0 {
		return time.Duration(m.Timeout) * time.Second
	}
	// OAuth flows require user interaction in a browser, so use a
	// generous default to avoid timing out mid-auth.
	if m.OAuth {
		return 30 * time.Second
	}
	return 10 * time.Second
}

// unwrapTransport peels the wrappers this package puts around a transport
// before handing it to the SDK, so a type switch sees what was actually
// built. Every connection is wrapped in a channelTransport (see
// connection.go), and the two places that ask what kind of transport they
// have — closeIdleTransport and maybeStdioErr — were asking the wrapper.
// Both silently answered "neither": keep-alive connections and their
// http.Transport goroutines were never closed on a renew, teardown or
// Close, and a stdio server that failed to start (the npx-cannot-find-node
// case) never got the diagnostic that exists for it.
func unwrapTransport(t mcp.Transport) mcp.Transport {
	for {
		wrapper, ok := t.(interface{ innerTransport() mcp.Transport })
		if !ok {
			return t
		}
		inner := wrapper.innerTransport()
		if inner == nil {
			return t
		}
		t = inner
	}
}
