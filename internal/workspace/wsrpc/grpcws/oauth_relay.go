package grpcws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// relayShutdownTimeout bounds how long StartOAuthCallbackRelay's stop func
// waits for an in-flight request (the browser's own redirect, almost
// always the only one) to finish before it gives up and closes the
// listener out from under it.
const relayShutdownTimeout = 5 * time.Second

// deliverFunc is what a relay hands each request to, and gets a reply
// from: an RPC to the daemon that actually owns the flow
// (Client.deliverOAuthCallback), stripped down to the pieces an
// http.Handler needs. It exists so StartOAuthCallbackRelay stays
// independent of the OAuth service's wire types, and a test can fake it
// without a real connection.
type deliverFunc func(ctx context.Context, method, path, rawQuery string, body []byte) (status int, headers http.Header, respBody []byte, err error)

// StartOAuthCallbackRelay begins forwarding a provider's browser redirect
// from this machine to whichever daemon actually owns the OAuth flow
// (CLIENT-SERVER.md, PR 3.3): the client-side half of the relay. authURL
// is the authorization URL the caller is about to open in a browser; its
// redirect_uri is parsed out and used verbatim as the listener's own
// address, so the browser's eventual redirect lands on it with no
// rewriting needed on either side.
//
// Only a loopback redirect_uri host is accepted -- authURL names a daemon
// somewhere else, so relaying an arbitrary host here would bind and
// expose a port on every interface for a URL the caller doesn't control.
// A busy port fails outright, naming it, rather than silently listening
// on the wrong address.
//
// Every request the listener receives (expected to be exactly one: the
// browser's redirect) is handed to deliver, and the reply is written back
// to the browser byte for byte, headers included. The returned stop func
// releases the listener; it must be called once the flow this relay
// serves is done with, one way or another, so the port is never held
// longer than the sign-in that needed it.
func StartOAuthCallbackRelay(authURL string, deliver deliverFunc) (stop func(), err error) {
	redirectHost, err := loopbackRedirectHost(authURL)
	if err != nil {
		return nil, err
	}

	lc := &net.ListenConfig{}
	listener, err := lc.Listen(context.Background(), "tcp", redirectHost)
	if err != nil {
		return nil, fmt.Errorf("starting the local OAuth callback relay on %s (is a sign-in already in progress?): %w", redirectHost, err)
	}

	relay := &oauthRelay{deliver: deliver}
	server := &http.Server{Handler: relay, ReadHeaderTimeout: 10 * time.Second}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("OAuth callback relay stopped unexpectedly", "error", err)
		}
	}()

	var stopOnce sync.Once
	stop = func() {
		stopOnce.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), relayShutdownTimeout)
			defer cancel()
			_ = server.Shutdown(ctx)
			wg.Wait()
		})
	}
	return stop, nil
}

// loopbackRedirectHost extracts authURL's redirect_uri query parameter and
// returns its host:port, rejecting anything that doesn't parse as a
// loopback address -- an authorization URL is otherwise caller-supplied
// data, and this is the one place it gets to say "bind a port on this
// machine".
func loopbackRedirectHost(authURL string) (string, error) {
	parsed, err := url.Parse(authURL)
	if err != nil {
		return "", fmt.Errorf("parsing authorization URL: %w", err)
	}
	redirect := parsed.Query().Get("redirect_uri")
	if redirect == "" {
		return "", errors.New("authorization URL carries no redirect_uri to relay")
	}
	redirectURL, err := url.Parse(redirect)
	if err != nil {
		return "", fmt.Errorf("parsing redirect_uri: %w", err)
	}
	host := redirectURL.Hostname()
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return "", fmt.Errorf("refusing to relay a non-loopback redirect_uri host %q", host)
	}
	if redirectURL.Port() == "" {
		return "", errors.New("redirect_uri carries no port to relay")
	}
	return net.JoinHostPort(host, redirectURL.Port()), nil
}

// oauthRelay is the listener's http.Handler: it turns each request into a
// deliver call and writes the reply back byte for byte.
type oauthRelay struct {
	deliver deliverFunc
}

func (r *oauthRelay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, "reading request", http.StatusBadGateway)
			return
		}
	}

	status, headers, respBody, err := r.deliver(req.Context(), req.Method, req.URL.Path, req.URL.RawQuery, body)
	if err != nil {
		slog.Warn("OAuth callback relay could not deliver the callback", "error", err)
		http.Error(w, "could not reach the sign-in server: "+err.Error(), http.StatusBadGateway)
		return
	}
	for k, vs := range headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
}
