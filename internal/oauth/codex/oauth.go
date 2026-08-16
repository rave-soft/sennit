package codex

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/callback"
)

// httpTimeout bounds the token-exchange calls. The browser half of the flow
// is bounded by the caller's context instead: a person needs longer than any
// HTTP timeout worth setting.
const httpTimeout = 30 * time.Second

// Flow is one in-progress browser sign-in. StartFlow binds the loopback
// listener up front so the caller can print or open [Flow.URL] knowing the
// redirect cannot arrive before anything is listening for it.
//
// A Flow is single-use and must be closed, whether or not it completed.
type Flow struct {
	verifier string
	state    string
	authURL  string
	proxyURL string

	// mu guards server, which Close clears while the serving goroutine and
	// the callback handler are still live.
	mu      sync.Mutex
	server  *http.Server
	results chan callbackResult
}

// callbackResult is what the loopback handler hands back to Wait: either an
// authorization code, or the error the authorization server redirected with.
type callbackResult struct {
	code string
	err  error
}

// StartFlow begins a PKCE authorization. The returned Flow owns a listener
// on the fixed callback port; if that port is busy — most often another
// Codex or Sennit sign-in already running — the error says so rather than
// silently binding a port the browser will never visit.
//
// proxyURL routes the code exchange, and may be empty for none. It does not
// touch the listener, which is loopback, nor the browser, which uses
// whatever proxy the browser itself is configured with.
func StartFlow(proxyURL string) (*Flow, error) {
	if err := ValidateProxy(proxyURL); err != nil {
		return nil, err
	}

	verifier, err := randomURLSafe(64)
	if err != nil {
		return nil, err
	}
	state, err := randomURLSafe(32)
	if err != nil {
		return nil, err
	}

	challenge := sha256.Sum256([]byte(verifier))
	params := url.Values{
		"response_type":              {"code"},
		"client_id":                  {clientID},
		"redirect_uri":               {RedirectURI()},
		"scope":                      {scopes},
		"code_challenge":             {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method":      {"S256"},
		"state":                      {state},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}

	f := &Flow{
		verifier: verifier,
		state:    state,
		proxyURL: proxyURL,
		authURL:  authorizeURL + "?" + params.Encode(),
		results:  make(chan callbackResult, 1),
	}

	lc := &net.ListenConfig{}
	listener, err := lc.Listen(context.Background(), "tcp", fmt.Sprintf("localhost:%d", callbackPort))
	if err != nil {
		return nil, fmt.Errorf("failed to bind the Codex sign-in callback port %d (another sign-in may be in progress): %w", callbackPort, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handleCallback)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	f.server = server
	// The server is captured in a local rather than read off the Flow: Close
	// clears the field, and a goroutine reading it there would both race
	// with that write and call Serve on a nil server.
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			f.settle(callbackResult{err: err})
		}
	}()

	return f, nil
}

// URL is the authorization URL the user must open.
func (f *Flow) URL() string { return f.authURL }

// Close shuts the loopback listener down, freeing the callback port for the
// next sign-in. It is safe to call more than once.
func (f *Flow) Close() error {
	f.mu.Lock()
	server := f.server
	f.server = nil
	f.mu.Unlock()
	if server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return server.Shutdown(ctx)
}

// Wait blocks until the browser redirect arrives, then exchanges the
// authorization code for a token. The caller's context bounds the wait.
func (f *Flow) Wait(ctx context.Context) (*oauth.Token, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-f.results:
		if res.err != nil {
			return nil, res.err
		}
		return exchangeCode(ctx, f.proxyURL, res.code, f.verifier)
	}
}

// handleCallback receives the redirect, shows the user the outcome, and
// hands the code to Wait.
//
// The page is rendered before the result is published: publishing first
// unblocks Wait, whose caller closes the listener, which would cut this
// response off mid-write and leave the user looking at a browser error
// instead of a sign-in confirmation.
func (f *Flow) handleCallback(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != callbackPath {
		// Browsers ask for extras like /favicon.ico against the same
		// origin; settling on one of those would abort the sign-in with
		// an empty code.
		http.NotFound(w, req)
		return
	}

	query := req.URL.Query()
	result := callback.Result{
		Subject:          ProviderName,
		ErrorCode:        query.Get("error"),
		ErrorDescription: query.Get("error_description"),
	}
	code := query.Get("code")
	switch {
	case result.Failed():
	case query.Get("state") != f.state:
		// A mismatched state means this redirect belongs to some other
		// authorization, not the one we started.
		result.ErrorCode = "invalid_state"
		result.ErrorDescription = "The sign-in response did not match this request."
	case code == "":
		result.ErrorCode = "missing_code"
		result.ErrorDescription = "The sign-in response carried no authorization code."
	}

	if err := callback.Serve(w, result); err != nil {
		slog.Warn("Failed to render the Codex sign-in page", "error", err)
	}

	if result.Failed() {
		f.settle(callbackResult{err: fmt.Errorf("codex sign-in failed: %s %s", result.ErrorCode, result.ErrorDescription)})
		return
	}
	f.settle(callbackResult{code: code})
}

// settle publishes the first result and ignores every later one, so a
// reloaded callback tab cannot disturb a finished sign-in.
func (f *Flow) settle(res callbackResult) {
	select {
	case f.results <- res:
	default:
	}
}

// exchangeCode trades the authorization code for a token pair.
func exchangeCode(ctx context.Context, proxyURL, code, verifier string) (*oauth.Token, error) {
	return postToken(ctx, proxyURL, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"redirect_uri":  {RedirectURI()},
		"code_verifier": {verifier},
	})
}

// RefreshToken exchanges a refresh token for a fresh access token, through
// proxyURL when one is given. The Codex authorization server rotates refresh
// tokens, so the returned token carries the new one and callers must persist
// it.
func RefreshToken(ctx context.Context, proxyURL, refreshToken string) (*oauth.Token, error) {
	if refreshToken == "" {
		return nil, errors.New("no Codex refresh token available; sign in again")
	}
	token, err := postToken(ctx, proxyURL, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
		"scope":         {scopes},
	})
	if err != nil {
		return nil, err
	}
	if token.RefreshToken == "" {
		// Not every refresh rotates the token. Persisting the empty value
		// would leave nothing to refresh with next time, stranding the user
		// on a browser sign-in the moment this access token expires.
		token.RefreshToken = refreshToken
	}
	return token, nil
}

// postToken performs a token endpoint call and converts the response into an
// [oauth.Token]. Non-2xx replies come back as [oauth.TokenExchangeError] so
// the refresh path can tell a revoked grant (re-authenticate) apart from a
// transient failure (retry).
func postToken(ctx context.Context, proxyURL string, form url.Values) (*oauth.Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client, err := httpClient(proxyURL, httpTimeout)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &oauth.TokenExchangeError{StatusCode: resp.StatusCode, Body: string(body)}
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	if result.AccessToken == "" {
		return nil, errors.New("codex token response carried no access token")
	}
	if result.RefreshToken == "" {
		// A refresh-token rotation that returns nothing would strand the
		// caller on the next expiry, so keep the one it already has by
		// leaving the field empty and letting the caller decide.
		slog.Debug("Codex token response carried no refresh token")
	}

	token := &oauth.Token{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresIn:    result.ExpiresIn,
	}
	token.SetExpiresAt()
	return token, nil
}

// randomURLSafe returns n bytes of randomness encoded for use in a URL.
func randomURLSafe(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
