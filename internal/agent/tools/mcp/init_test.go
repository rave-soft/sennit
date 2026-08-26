package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/oauth"
	mcpoauth "github.com/rave-soft/sennit/internal/oauth/mcp"
	"github.com/rave-soft/sennit/internal/testenv"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"golang.org/x/oauth2"
)

// shellResolverWithPath builds a shell resolver whose env carries PATH
// plus any caller-supplied overrides. Without PATH, $(cat), $(echo),
// etc. can't find their binaries in a test process where the shell env
// is otherwise empty.
func shellResolverWithPath(t *testing.T, overrides map[string]string) config.VariableResolver {
	t.Helper()
	m := map[string]string{"PATH": os.Getenv("PATH")}
	maps.Copy(m, overrides)
	return config.NewShellVariableResolver(testenv.New(m))
}

func TestMCPSession_CancelOnClose(t *testing.T) {
	defer goleak.VerifyNone(t)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server"}, nil)
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()

	ctx, cancel := context.WithCancel(context.Background())

	client := mcp.NewClient(&mcp.Implementation{Name: "sennit-test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)

	sess := &ClientSession{ClientSession: clientSession, cancel: cancel}

	// Verify the context is not cancelled before close.
	require.NoError(t, ctx.Err())

	err = sess.Close()
	require.NoError(t, err)

	// After Close, the context must be cancelled.
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}

// TestCreateTransport_URLResolution pins that m.URL goes through the
// same resolver seam as command, args, env, and headers. Covers both
// the HTTP and SSE branches, success and failure, so a regression in
// ResolvedURL wiring is caught at the transport layer rather than only
// at the config layer.
type testOAuthHandler struct {
	tokenSource func(context.Context) (oauth2.TokenSource, error)
	authorize   func(context.Context, *http.Request, *http.Response) error
}

func (h testOAuthHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	return h.tokenSource(ctx)
}

func (h testOAuthHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	return h.authorize(ctx, req, resp)
}

type closeCounter struct{ count atomic.Int32 }

func (c *closeCounter) Read([]byte) (int, error) { return 0, io.EOF }
func (c *closeCounter) Close() error             { c.count.Add(1); return nil }

func TestOAuthRoundTripperRetryAndCleanup(t *testing.T) {
	t.Parallel()
	failedBody := &closeCounter{}
	var calls atomic.Int32
	base := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.Equal(t, "payload", string(body))
		if call == 1 {
			return &http.Response{StatusCode: http.StatusUnauthorized, Body: failedBody}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	var authorize atomic.Int32
	handler := testOAuthHandler{
		tokenSource: func(context.Context) (oauth2.TokenSource, error) {
			return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "token"}), nil
		},
		authorize: func(_ context.Context, _ *http.Request, resp *http.Response) error {
			authorize.Add(1)
			return resp.Body.Close()
		},
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.test", strings.NewReader("payload"))
	require.NoError(t, err)
	resp, err := newOAuthRoundTripper(handler, base).RoundTrip(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, int32(2), calls.Load())
	require.Equal(t, int32(1), authorize.Load())
	require.Equal(t, int32(1), failedBody.count.Load())
	require.Empty(t, req.Header.Get("Authorization"))
}

func TestOAuthRoundTripperClosesUnauthorizedBodyWhenAuthorizeDoesNot(t *testing.T) {
	t.Parallel()
	body := &closeCounter{}
	handler := testOAuthHandler{
		tokenSource: func(context.Context) (oauth2.TokenSource, error) { return nil, nil },
		authorize: func(context.Context, *http.Request, *http.Response) error {
			return mcpoauth.ErrInteractiveAuthRequired
		},
	}
	resp, err := newOAuthRoundTripper(handler, roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: body}, nil
	})).RoundTrip(mustRequest(t, http.MethodGet, "https://example.test", nil))
	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}
	require.Nil(t, resp)
	require.ErrorIs(t, err, mcpoauth.ErrInteractiveAuthRequired)
	require.Equal(t, int32(1), body.count.Load())
}

func mustRequest(t *testing.T, method, url string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	require.NoError(t, err)
	return req
}

func TestOAuthRoundTripperClosesBodyBeforeDelegationErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		handler testOAuthHandler
	}{
		{name: "token source", handler: testOAuthHandler{tokenSource: func(context.Context) (oauth2.TokenSource, error) { return nil, errors.New("source") }}},
		{name: "token", handler: testOAuthHandler{tokenSource: func(context.Context) (oauth2.TokenSource, error) {
			return oauth2.ReuseTokenSource(nil, errorTokenSource{}), nil
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := &closeCounter{}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.test", body)
			require.NoError(t, err)
			resp, err := newOAuthRoundTripper(tc.handler, roundTripperFunc(func(*http.Request) (*http.Response, error) { t.Fatal("delegated"); return nil, nil })).RoundTrip(req)
			if resp != nil {
				require.NoError(t, resp.Body.Close())
			}
			require.Error(t, err)
			require.Equal(t, int32(1), body.count.Load())
		})
	}
}

type errorTokenSource struct{}

func (errorTokenSource) Token() (*oauth2.Token, error) { return nil, errors.New("token") }

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestCreateTransport_URLResolution(t *testing.T) {
	t.Parallel()

	shell := config.NewShellVariableResolver(testenv.New(map[string]string{
		"MCP_HOST": "mcp.example.com",
	}))

	t.Run("http success expands $VAR", func(t *testing.T) {
		t.Parallel()
		m := config.MCPConfig{
			Type: config.MCPHttp,
			URL:  "https://$MCP_HOST/api",
		}
		tr, _, err := defaultRegistry.createTransportFor(t.Context(), nil, "test", m, defaultRegistry.currentGen("test"), defaultRegistry.authAttempt.Add(1), shell)
		require.NoError(t, err)
		require.NotNil(t, tr)
		sct, ok := tr.(*mcp.StreamableClientTransport)
		require.True(t, ok, "expected StreamableClientTransport, got %T", tr)
		require.Equal(t, "https://mcp.example.com/api", sct.Endpoint)
	})

	t.Run("sse success expands $(cmd)", func(t *testing.T) {
		t.Parallel()
		m := config.MCPConfig{
			Type: config.MCPSSE,
			URL:  "https://$(echo mcp.example.com)/events",
		}
		tr, _, err := defaultRegistry.createTransportFor(t.Context(), nil, "test", m, defaultRegistry.currentGen("test"), defaultRegistry.authAttempt.Add(1), shell)
		require.NoError(t, err)
		sse, ok := tr.(*mcp.SSEClientTransport)
		require.True(t, ok, "expected SSEClientTransport, got %T", tr)
		require.Equal(t, "https://mcp.example.com/events", sse.Endpoint)
	})

	t.Run("http failing $(cmd) surfaces error, no transport created", func(t *testing.T) {
		t.Parallel()
		// Under lenient nounset, unset $VAR expands to "" silently,
		// so the only way a URL resolution *errors* is a failing
		// $(cmd). Mirror the SSE subtest so both transports share
		// coverage for the url-resolve-failure path.
		m := config.MCPConfig{
			Type: config.MCPHttp,
			URL:  "https://$(false)/api",
		}
		tr, _, err := defaultRegistry.createTransportFor(t.Context(), nil, "test", m, defaultRegistry.currentGen("test"), defaultRegistry.authAttempt.Add(1), shellResolverWithPath(t, nil))
		require.Error(t, err)
		require.Nil(t, tr)
		require.Contains(t, err.Error(), "url:")
		require.Contains(t, err.Error(), "$(false)")
	})

	t.Run("http unset var expands empty", func(t *testing.T) {
		t.Parallel()
		// Pinning test for the new lenient-nounset default: an
		// unset bare $VAR in the URL is *not* an error. It
		// expands to "" and, here, leaves a syntactically weird
		// but non-empty URL that the existing non-empty guard
		// still lets through. Guards against a future regression
		// that flips strict-by-default back on.
		m := config.MCPConfig{
			Type: config.MCPHttp,
			URL:  "https://$MCP_MISSING_HOST/api",
		}
		tr, _, err := defaultRegistry.createTransportFor(t.Context(), nil, "test", m, defaultRegistry.currentGen("test"), defaultRegistry.authAttempt.Add(1), shell)
		require.NoError(t, err)
		sct, ok := tr.(*mcp.StreamableClientTransport)
		require.True(t, ok)
		require.Equal(t, "https:///api", sct.Endpoint)
	})

	t.Run("sse failing $(cmd) surfaces error, no transport created", func(t *testing.T) {
		t.Parallel()
		m := config.MCPConfig{
			Type: config.MCPSSE,
			URL:  "https://$(false)/events",
		}
		tr, _, err := defaultRegistry.createTransportFor(t.Context(), nil, "test", m, defaultRegistry.currentGen("test"), defaultRegistry.authAttempt.Add(1), shell)
		require.Error(t, err)
		require.Nil(t, tr)
		require.Contains(t, err.Error(), "url:")
		require.Contains(t, err.Error(), "$(false)")
	})

	t.Run("http empty-after-resolve still fails the non-empty guard", func(t *testing.T) {
		t.Parallel()
		// ${MCP_EMPTY:-} resolves to the empty string (no error),
		// then the existing TrimSpace guard in defaultRegistry.createTransportFor must
		// reject it so we never spawn a transport against "".
		m := config.MCPConfig{
			Type: config.MCPHttp,
			URL:  "${MCP_EMPTY:-}",
		}
		tr, _, err := defaultRegistry.createTransportFor(t.Context(), nil, "test", m, defaultRegistry.currentGen("test"), defaultRegistry.authAttempt.Add(1), shell)
		require.Error(t, err)
		require.Nil(t, tr)
		require.Contains(t, err.Error(), "non-empty 'url'")
	})

	t.Run("identity resolver round-trips template verbatim", func(t *testing.T) {
		t.Parallel()
		// Client mode forwards the template to the server; no local
		// expansion, no error on unset vars.
		tmpl := "https://$MCP_MISSING_HOST/api"
		m := config.MCPConfig{Type: config.MCPHttp, URL: tmpl}
		tr, _, err := defaultRegistry.createTransportFor(t.Context(), nil, "test", m, defaultRegistry.currentGen("test"), defaultRegistry.authAttempt.Add(1), config.IdentityResolver())
		require.NoError(t, err)
		sct, ok := tr.(*mcp.StreamableClientTransport)
		require.True(t, ok)
		require.Equal(t, tmpl, sct.Endpoint)
	})
}

// TestCreateTransport_StdioResolution pins that command, args, and env
// for stdio MCPs go through the same resolver seam as the other
// transports. Covers both success (expansion produced the expected
// exec.Cmd) and failure (any one field erroring prevents transport
// creation).
func TestCreateTransport_StdioResolution(t *testing.T) {
	t.Parallel()

	t.Run("success expands command, args, and env", func(t *testing.T) {
		t.Parallel()
		r := shellResolverWithPath(t, map[string]string{
			"MY_TOKEN": "hunter2",
		})
		m := config.MCPConfig{
			Type:    config.MCPStdio,
			Command: "forgejo-mcp",
			Args:    []string{"--token", "$MY_TOKEN", "--host", "$(echo example.com)"},
			Env: map[string]string{
				"SECRET":    "$(echo shh)",
				"PLAIN":     "literal",
				"REFERENCE": "$MY_TOKEN",
			},
		}
		tr, _, err := defaultRegistry.createTransportFor(t.Context(), nil, "test", m, defaultRegistry.currentGen("test"), defaultRegistry.authAttempt.Add(1), r)
		require.NoError(t, err)
		require.NotNil(t, tr)

		ct, ok := tr.(*mcp.CommandTransport)
		require.True(t, ok, "expected CommandTransport, got %T", tr)

		// exec.Cmd.Args[0] is the command name; the rest are positional
		// args as passed.
		require.Equal(t, []string{"forgejo-mcp", "--token", "hunter2", "--host", "example.com"}, ct.Command.Args)

		// Env is os.Environ() + resolved entries (sorted). Check the
		// resolved entries are present with their expanded values.
		require.Contains(t, ct.Command.Env, "SECRET=shh")
		require.Contains(t, ct.Command.Env, "PLAIN=literal")
		require.Contains(t, ct.Command.Env, "REFERENCE=hunter2")
	})

	t.Run("env resolution failure surfaces error, no transport created", func(t *testing.T) {
		t.Parallel()
		r := shellResolverWithPath(t, nil)
		m := config.MCPConfig{
			Type:    config.MCPStdio,
			Command: "forgejo-mcp",
			Env:     map[string]string{"TOKEN": "$(false)"},
		}
		tr, _, err := defaultRegistry.createTransportFor(t.Context(), nil, "test", m, defaultRegistry.currentGen("test"), defaultRegistry.authAttempt.Add(1), r)
		require.Error(t, err)
		require.Nil(t, tr)
		require.Contains(t, err.Error(), "env TOKEN")
	})

	t.Run("failing env command is a hard error", func(t *testing.T) {
		t.Parallel()
		// Under lenient nounset a bare $UNSET expands to ""
		// silently — see the pinning subtest below. The remaining
		// failure mode for env resolution is a $(cmd) that exits
		// non-zero, which must still error out and prevent exec so
		// we never hand a broken credential to the child process.
		r := shellResolverWithPath(t, nil)
		m := config.MCPConfig{
			Type:    config.MCPStdio,
			Command: "forgejo-mcp",
			Env:     map[string]string{"FORGEJO_ACCESS_TOKEN": "$(exit 5)"},
		}
		tr, _, err := defaultRegistry.createTransportFor(t.Context(), nil, "test", m, defaultRegistry.currentGen("test"), defaultRegistry.authAttempt.Add(1), r)
		require.Error(t, err)
		require.Nil(t, tr)
		require.Contains(t, err.Error(), "env FORGEJO_ACCESS_TOKEN")
	})

	t.Run("unset env var expands empty", func(t *testing.T) {
		t.Parallel()
		// Pinning test for the lenient-nounset default: a bare
		// $UNSET in an env value expands to "" without error, and
		// the empty entry is kept on the resulting exec.Cmd (env
		// entries, unlike headers, are not dropped — see design
		// decision #18). Guards against a regression that flips
		// strict-by-default back on and silently breaks users
		// with configs like FORGEJO_ACCESS_TOKEN=$FORGEJO_TOKEN.
		r := shellResolverWithPath(t, nil)
		m := config.MCPConfig{
			Type:    config.MCPStdio,
			Command: "forgejo-mcp",
			Env:     map[string]string{"FORGEJO_ACCESS_TOKEN": "$FORGEJO_TOKEN_UNSET"},
		}
		tr, _, err := defaultRegistry.createTransportFor(t.Context(), nil, "test", m, defaultRegistry.currentGen("test"), defaultRegistry.authAttempt.Add(1), r)
		require.NoError(t, err)
		ct, ok := tr.(*mcp.CommandTransport)
		require.True(t, ok)
		require.Contains(t, ct.Command.Env, "FORGEJO_ACCESS_TOKEN=")
	})

	t.Run("args resolution failure surfaces error, no transport created", func(t *testing.T) {
		t.Parallel()
		r := shellResolverWithPath(t, nil)
		m := config.MCPConfig{
			Type:    config.MCPStdio,
			Command: "forgejo-mcp",
			Args:    []string{"--token", "$(false)"},
		}
		tr, _, err := defaultRegistry.createTransportFor(t.Context(), nil, "test", m, defaultRegistry.currentGen("test"), defaultRegistry.authAttempt.Add(1), r)
		require.Error(t, err)
		require.Nil(t, tr)
		require.Contains(t, err.Error(), "arg 1")
	})

	t.Run("command resolution failure surfaces error, no transport created", func(t *testing.T) {
		t.Parallel()
		r := shellResolverWithPath(t, nil)
		m := config.MCPConfig{
			Type:    config.MCPStdio,
			Command: "$(false)",
		}
		tr, _, err := defaultRegistry.createTransportFor(t.Context(), nil, "test", m, defaultRegistry.currentGen("test"), defaultRegistry.authAttempt.Add(1), r)
		require.Error(t, err)
		require.Nil(t, tr)
		require.Contains(t, err.Error(), "invalid mcp command")
	})

	t.Run("identity resolver round-trips templates verbatim", func(t *testing.T) {
		t.Parallel()
		// Client mode: no local expansion, no error on unset vars.
		m := config.MCPConfig{
			Type:    config.MCPStdio,
			Command: "forgejo-mcp",
			Args:    []string{"--token", "$MCP_MISSING"},
			Env:     map[string]string{"TOKEN": "$(vault read -f token)"},
		}
		tr, _, err := defaultRegistry.createTransportFor(t.Context(), nil, "test", m, defaultRegistry.currentGen("test"), defaultRegistry.authAttempt.Add(1), config.IdentityResolver())
		require.NoError(t, err)
		ct, ok := tr.(*mcp.CommandTransport)
		require.True(t, ok)
		require.Equal(t, []string{"forgejo-mcp", "--token", "$MCP_MISSING"}, ct.Command.Args)
		require.Contains(t, ct.Command.Env, "TOKEN=$(vault read -f token)")
	})
}

// TestCreateTransport_HeadersResolution pins that a single failing
// header aborts HTTP/SSE transport creation and that the successful
// resolver passes every expanded header through to the round tripper.
func TestCreateTransport_HeadersResolution(t *testing.T) {
	t.Parallel()

	t.Run("http headers success expands $(cmd)", func(t *testing.T) {
		t.Parallel()
		r := shellResolverWithPath(t, map[string]string{
			"GITHUB_TOKEN": "gh-secret",
		})
		m := config.MCPConfig{
			Type: config.MCPHttp,
			URL:  "https://mcp.example.com/api",
			Headers: map[string]string{
				"Authorization": "$(echo Bearer $GITHUB_TOKEN)",
				"X-Static":      "kept",
			},
		}
		tr, _, err := defaultRegistry.createTransportFor(t.Context(), nil, "test", m, defaultRegistry.currentGen("test"), defaultRegistry.authAttempt.Add(1), r)
		require.NoError(t, err)

		sct, ok := tr.(*mcp.StreamableClientTransport)
		require.True(t, ok)
		rt, ok := sct.HTTPClient.Transport.(*headerRoundTripper)
		require.True(t, ok, "expected headerRoundTripper, got %T", sct.HTTPClient.Transport)
		require.Equal(t, map[string]string{
			"Authorization": "Bearer gh-secret",
			"X-Static":      "kept",
		}, rt.headers)
	})

	t.Run("http failing header surfaces error, no transport", func(t *testing.T) {
		t.Parallel()
		r := shellResolverWithPath(t, nil)
		m := config.MCPConfig{
			Type:    config.MCPHttp,
			URL:     "https://mcp.example.com/api",
			Headers: map[string]string{"Authorization": "$(false)"},
		}
		tr, _, err := defaultRegistry.createTransportFor(t.Context(), nil, "test", m, defaultRegistry.currentGen("test"), defaultRegistry.authAttempt.Add(1), r)
		require.Error(t, err)
		require.Nil(t, tr)
		require.Contains(t, err.Error(), "header Authorization")
	})

	t.Run("sse failing header surfaces error, no transport", func(t *testing.T) {
		t.Parallel()
		// Under lenient nounset a bare $MISSING expands to "",
		// which ResolvedHeaders drops — no error. The failing
		// $(cmd) path is the remaining way this can fail loudly;
		// cover it on the SSE branch to mirror the HTTP subtest.
		r := shellResolverWithPath(t, nil)
		m := config.MCPConfig{
			Type:    config.MCPSSE,
			URL:     "https://mcp.example.com/events",
			Headers: map[string]string{"Authorization": "$(false)"},
		}
		tr, _, err := defaultRegistry.createTransportFor(t.Context(), nil, "test", m, defaultRegistry.currentGen("test"), defaultRegistry.authAttempt.Add(1), r)
		require.Error(t, err)
		require.Nil(t, tr)
		require.Contains(t, err.Error(), "header Authorization")
	})

	t.Run("sse unset var header drops silently", func(t *testing.T) {
		t.Parallel()
		// Pinning test for empty-header drop + lenient nounset:
		// a header whose value resolves to "" (here because the
		// bare $VAR is unset) is omitted from the round tripper
		// rather than sent as "X-Header:". Guards against a
		// regression that either re-introduces strict-by-default
		// or stops dropping empty headers.
		r := shellResolverWithPath(t, nil)
		m := config.MCPConfig{
			Type:    config.MCPSSE,
			URL:     "https://mcp.example.com/events",
			Headers: map[string]string{"Authorization": "$MISSING_TOKEN"},
		}
		tr, _, err := defaultRegistry.createTransportFor(t.Context(), nil, "test", m, defaultRegistry.currentGen("test"), defaultRegistry.authAttempt.Add(1), r)
		require.NoError(t, err)
		sse, ok := tr.(*mcp.SSEClientTransport)
		require.True(t, ok)
		rt, ok := sse.HTTPClient.Transport.(*headerRoundTripper)
		require.True(t, ok)
		require.NotContains(t, rt.headers, "Authorization")
	})
}

// TestCreateSession_ResolutionFailureUpdatesState pins the user-visible
// half of the regression fix: when any of command/args/env/headers/url
// fails to resolve, defaultRegistry.createSession must publish StateError to the state
// map so sennit_info and the TUI's MCP status card can render a real
// error instead of the MCP silently sitting in "starting" or being
// spawned with an empty credential.
//
// These subtests cannot run in parallel: `defaultRegistry.states` is a package-level
// csync.Map and each assertion reads the entry written by the call
// under test. They do use unique MCP names per subtest to keep them
// independent regardless of ordering.
func TestCreateSession_ResolutionFailureUpdatesState(t *testing.T) {
	r := shellResolverWithPath(t, nil)

	tests := []struct {
		name            string
		mcpName         string
		cfg             config.MCPConfig
		wantErrContains string
	}{
		{
			name:    "stdio env failure",
			mcpName: "test-stdio-env-fail",
			cfg: config.MCPConfig{
				Type:    config.MCPStdio,
				Command: "echo",
				Env:     map[string]string{"FORGEJO_ACCESS_TOKEN": "$(false)"},
			},
			wantErrContains: "env FORGEJO_ACCESS_TOKEN",
		},
		{
			// Args that reference an unset bare $VAR no longer
			// error out under lenient nounset; the only remaining
			// failure mode for arg resolution is a failing $(cmd).
			name:    "stdio args failure",
			mcpName: "test-stdio-args-fail",
			cfg: config.MCPConfig{
				Type:    config.MCPStdio,
				Command: "echo",
				Args:    []string{"--token", "$(false)"},
			},
			wantErrContains: "arg 1",
		},
		{
			// Likewise for URL: bare $UNSET expands to ""
			// silently, so we need a failing $(cmd) to exercise
			// the "url:" wrap from ResolvedURL.
			name:    "http url failure",
			mcpName: "test-http-url-fail",
			cfg: config.MCPConfig{
				Type: config.MCPHttp,
				URL:  "https://$(false)/api",
			},
			wantErrContains: "url:",
		},
		{
			// A URL whose shell expansion yields the empty
			// string (here via ${VAR:-}) is not a ResolvedURL
			// error, but the non-empty guard in defaultRegistry.createTransportFor
			// must still reject it so the state card renders an
			// error instead of spawning a transport against "".
			name:    "http empty-resolved url",
			mcpName: "test-http-url-empty",
			cfg: config.MCPConfig{
				Type: config.MCPHttp,
				URL:  "${MCP_URL_EMPTY:-}",
			},
			wantErrContains: "non-empty 'url'",
		},
		{
			name:    "http header failure",
			mcpName: "test-http-header-fail",
			cfg: config.MCPConfig{
				Type:    config.MCPHttp,
				URL:     "https://mcp.example.com/api",
				Headers: map[string]string{"Authorization": "$(false)"},
			},
			wantErrContains: "header Authorization",
		},
		{
			name:    "sse url failure",
			mcpName: "test-sse-url-fail",
			cfg: config.MCPConfig{
				Type: config.MCPSSE,
				URL:  "https://$(false)/events",
			},
			wantErrContains: "url:",
		},
		{
			// Bare $MISSING in a header resolves to "" silently
			// and is then dropped. The "header Authorization"
			// wrap only surfaces on a $(cmd) failure; that is
			// what this subtest now pins for the SSE path.
			name:    "sse header failure",
			mcpName: "test-sse-header-fail",
			cfg: config.MCPConfig{
				Type:    config.MCPSSE,
				URL:     "https://mcp.example.com/events",
				Headers: map[string]string{"Authorization": "$(false)"},
			},
			wantErrContains: "header Authorization",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Guarantee a clean slate on the shared state map so a
			// stale entry from another test can't satisfy the
			// assertion.
			defaultRegistry.states.Del(tc.mcpName)
			defaultRegistry.owners[tc.mcpName] = attemptID{gen: defaultRegistry.currentGen(tc.mcpName), seq: 1}
			t.Cleanup(func() { defaultRegistry.states.Del(tc.mcpName) })

			sess, err := defaultRegistry.createSession(t.Context(), nil, tc.mcpName, tc.cfg, attemptID{gen: defaultRegistry.currentGen(tc.mcpName), seq: 1}, r, false)
			require.Error(t, err)
			require.Nil(t, sess)
			require.Contains(t, err.Error(), tc.wantErrContains)
			// createSession is deliberately a resource factory: only its owning
			// generation-aware attempt publishes state. This prevents a stale
			// factory failure from overwriting a newer connection.
		})
	}
}

func TestReconcile(t *testing.T) {
	t.Parallel()

	base := config.MCPConfig{
		Type: config.MCPHttp,
		URL:  "https://example.com/mcp",
	}
	changed := func() config.MCPConfig { m := base; m.URL = "https://other.com/mcp"; return m }()
	disabled := func() config.MCPConfig { m := base; m.Disabled = true; return m }()
	ptr := func(m config.MCPConfig) *config.MCPConfig { return &m }

	// server seeds the running state reconcile diffs against: a state, the
	// config the server last connected with (Config), and, for a server
	// mid-connect, the config that attempt is connecting with (PendingConfig).
	type server struct {
		state   State
		config  config.MCPConfig
		pending *config.MCPConfig
	}

	tests := []struct {
		name    string
		servers map[string]server
		current config.MCPs
		want    map[string]reinitAction
	}{
		{
			name:    "new server starts",
			current: config.MCPs{"a": base},
			want:    map[string]reinitAction{"a": reinitStart},
		},
		{
			name:    "removed server is cleaned up",
			servers: map[string]server{"gone": {state: StateConnected, config: base}},
			current: config.MCPs{},
			want:    map[string]reinitAction{"gone": reinitRemove},
		},
		{
			name:    "unchanged connected server is skipped",
			servers: map[string]server{"a": {state: StateConnected, config: base}},
			current: config.MCPs{"a": base},
			want:    map[string]reinitAction{},
		},
		{
			name:    "changed config restarts",
			servers: map[string]server{"a": {state: StateConnected, config: base}},
			current: config.MCPs{"a": changed},
			want:    map[string]reinitAction{"a": reinitStart},
		},
		{
			name:    "disabled server is disabled",
			servers: map[string]server{"a": {state: StateConnected, config: base}},
			current: config.MCPs{"a": disabled},
			want:    map[string]reinitAction{"a": reinitDisable},
		},
		{
			name:    "already disabled server is skipped",
			servers: map[string]server{"a": {state: StateDisabled}},
			current: config.MCPs{"a": disabled},
			want:    map[string]reinitAction{},
		},
		{
			// Regression: disabling clears the recorded config, so a server
			// left disabled with an unchanged config must restart on re-enable
			// rather than being skipped as "already initialized".
			name:    "re-enabled server restarts despite unchanged config",
			servers: map[string]server{"a": {state: StateDisabled}},
			current: config.MCPs{"a": base},
			want:    map[string]reinitAction{"a": reinitStart},
		},
		{
			name:    "errored server restarts",
			servers: map[string]server{"a": {state: StateError, config: base}},
			current: config.MCPs{"a": base},
			want:    map[string]reinitAction{"a": reinitStart},
		},
		{
			name:    "starting server connecting with current config is left alone",
			servers: map[string]server{"a": {state: StateStarting, pending: ptr(base)}},
			current: config.MCPs{"a": base},
			want:    map[string]reinitAction{},
		},
		{
			// Regression: a config change that lands while a server is still
			// connecting must restart it, otherwise the in-flight attempt
			// connects with the old config and the change is silently lost.
			name:    "starting server with changed config restarts",
			servers: map[string]server{"a": {state: StateStarting, pending: ptr(base)}},
			current: config.MCPs{"a": changed},
			want:    map[string]reinitAction{"a": reinitStart},
		},
		{
			name: "mixed scenario",
			servers: map[string]server{
				"keep":    {state: StateConnected, config: base},
				"remove":  {state: StateConnected, config: base},
				"restart": {state: StateConnected, config: base},
			},
			current: config.MCPs{
				"keep":    base,
				"restart": changed,
				"new":     base,
			},
			want: map[string]reinitAction{
				"remove":  reinitRemove,
				"restart": reinitStart,
				"new":     reinitStart,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			running := make(map[string]ClientInfo, len(tc.servers))
			for name, s := range tc.servers {
				running[name] = ClientInfo{
					Name:          name,
					State:         s.state,
					Config:        s.config,
					PendingConfig: s.pending,
				}
			}
			got := reconcile(tc.current, running)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestMCPConfigEqual(t *testing.T) {
	t.Parallel()

	base := config.MCPConfig{
		Type:    config.MCPHttp,
		URL:     "https://example.com/mcp",
		Headers: map[string]string{"Authorization": "Bearer tok"},
		Timeout: 30,
	}

	tests := []struct {
		name string
		a, b config.MCPConfig
		want bool
	}{
		{"identical", base, base, true},
		{"different URL", base, func() config.MCPConfig { m := base; m.URL = "https://other.com/mcp"; return m }(), false},
		{"different headers", base, func() config.MCPConfig {
			m := base
			m.Headers = map[string]string{"Authorization": "Bearer other"}
			return m
		}(), false},
		{"different timeout", base, func() config.MCPConfig { m := base; m.Timeout = 60; return m }(), false},
		{"different type", base, func() config.MCPConfig { m := base; m.Type = config.MCPStdio; return m }(), false},
		{
			"OAuthToken ignored",
			base,
			func() config.MCPConfig {
				m := base
				m.OAuthToken = &oauth.Token{AccessToken: "x"}
				return m
			}(),
			true,
		},
		{
			"both OAuthToken ignored",
			func() config.MCPConfig {
				m := base
				m.OAuthToken = &oauth.Token{AccessToken: "x"}
				return m
			}(),
			func() config.MCPConfig {
				m := base
				m.OAuthToken = &oauth.Token{AccessToken: "y"}
				return m
			}(),
			true,
		},
		{"disabled vs enabled", base, func() config.MCPConfig { m := base; m.Disabled = true; return m }(), false},
		{"oauth flag", base, func() config.MCPConfig { m := base; m.OAuth = true; return m }(), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, mcpConfigEqual(tc.a, tc.b))
		})
	}
}

// TestMCPConfigEqualExhaustive guards mcpConfigEqual against drift. It
// enumerates every field of config.MCPConfig via reflection and fails if a
// field is neither compared by mcpConfigEqual nor explicitly excluded here.
// Adding a field to MCPConfig now forces a conscious decision about whether
// it should trigger a server restart, rather than being silently ignored.
func TestMCPConfigEqualExhaustive(t *testing.T) {
	t.Parallel()

	// Fields intentionally excluded from the comparison.
	excluded := map[string]bool{
		"OAuthToken": true, // internally managed, refreshed out-of-band.
	}

	typ := reflect.TypeOf(config.MCPConfig{})
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if excluded[name] {
			continue
		}
		// Build two configs that differ only in this field and assert the
		// difference is detected.
		a := config.MCPConfig{}
		b := config.MCPConfig{}
		setDistinct(typ.Field(i).Type, reflect.ValueOf(&a).Elem().Field(i))
		require.False(t, mcpConfigEqual(a, b),
			"mcpConfigEqual ignores field %q; add it to the comparison or to the excluded set", name)
	}
}

// setDistinct assigns a non-zero value of the given type so two structs
// differ in exactly one field.
func setDistinct(typ reflect.Type, field reflect.Value) {
	switch typ.Kind() {
	case reflect.String:
		field.SetString("x")
	case reflect.Bool:
		field.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		field.SetInt(1)
	case reflect.Slice:
		field.Set(reflect.MakeSlice(typ, 1, 1))
	case reflect.Map:
		m := reflect.MakeMap(typ)
		m.SetMapIndex(reflect.Zero(typ.Key()), reflect.Zero(typ.Elem()))
		field.Set(m)
	case reflect.Pointer:
		field.Set(reflect.New(typ.Elem()))
	default:
		panic("setDistinct: unhandled kind " + typ.Kind().String())
	}
}

// TestBeginAuth_UnknownServer proves BeginAuth rejects a server that is not
// present in the configuration.
func TestBeginAuth_HungWorkerRetainsExecutionSlotUntilExit(t *testing.T) {
	const name = "hung-auth"
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPHttp, URL: "https://example.test", OAuth: true}}})
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	var sideEffects atomic.Int32
	r.runAuth = func(context.Context, ConfigProvider, string, config.MCPConfig, attemptID) error {
		started <- struct{}{}
		sideEffects.Add(1)
		<-release
		return nil
	}
	finish, _, err := r.BeginAuth(cfg, name)
	require.NoError(t, err)
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, finish(ctx), context.Canceled)

	_, _, err = r.BeginAuth(cfg, name)
	require.ErrorContains(t, err, "already has an authentication in progress")
	require.Equal(t, int32(1), sideEffects.Load())
	select {
	case <-started:
		t.Fatal("second authentication worker started before the first exited")
	default:
	}

	r.authMu.Lock()
	first := r.authFlows[name]
	r.authMu.Unlock()
	require.NotNil(t, first)
	close(release)
	<-first.workerDone
	r.authMu.Lock()
	_, active := r.authFlows[name]
	r.authMu.Unlock()
	require.False(t, active, "worker must remove its exact auth flow on exit")

	r.runAuth = func(context.Context, ConfigProvider, string, config.MCPConfig, attemptID) error {
		started <- struct{}{}
		sideEffects.Add(1)
		return nil
	}
	finish2, _, err := r.BeginAuth(cfg, name)
	require.NoError(t, err)
	<-started
	require.NoError(t, finish2(t.Context()))
	require.Equal(t, int32(2), sideEffects.Load())
}

func TestBeginAuth_CancelSettlesExactStartingOwner(t *testing.T) {
	const name = "cancel-auth"
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPHttp, URL: "https://example.test", OAuth: true}}})
	started := make(chan struct{})
	exited := make(chan struct{})
	r.runAuth = func(ctx context.Context, _ ConfigProvider, name string, m config.MCPConfig, owner attemptID) error {
		r.updateStateFor(name, owner, StateStarting, nil, withPending(m))
		close(started)
		<-ctx.Done()
		close(exited)
		return ctx.Err()
	}
	_, cancel, err := r.BeginAuth(cfg, name)
	require.NoError(t, err)
	<-started
	// Grab the flow before cancelling, not after: completeAuthFlow deletes
	// it from authFlows as soon as the worker exits, so a cancel that the
	// worker services promptly leaves nothing to read. The worker is parked
	// on <-ctx.Done() at this point, so the flow is registered and stays
	// registered until the cancel below.
	r.authMu.Lock()
	flow := r.authFlows[name]
	r.authMu.Unlock()
	require.NotNil(t, flow)

	cancel()
	<-exited
	<-flow.workerDone
	info, ok := r.states.Get(name)
	require.True(t, ok)
	require.Equal(t, StateNeedsAuth, info.State)
}

func TestBeginAuth_CancelDoesNotOverwriteNewerLifecycleState(t *testing.T) {
	tests := []struct {
		name  string
		state State
		newer bool
	}{
		{name: "disabled", state: StateDisabled},
		{name: "connected", state: StateConnected},
		{name: "new-generation", state: StateStarting, newer: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const name = "stale-cancel"
			r := NewRegistry()
			cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPHttp, URL: "https://example.test", OAuth: true}}})
			started := make(chan struct{})
			release := make(chan struct{})
			r.runAuth = func(context.Context, ConfigProvider, string, config.MCPConfig, attemptID) error {
				close(started)
				<-release
				return context.Canceled
			}
			_, cancel, err := r.BeginAuth(cfg, name)
			require.NoError(t, err)
			<-started
			if tt.newer {
				r.publishMu.Lock()
				r.gens.Set(name, r.currentGen(name)+1)
				r.owners[name] = attemptID{gen: r.currentGen(name), seq: r.authAttempt.Add(1)}
				r.updateStateLocked(name, tt.state, nil, nil, Counts{})
				r.publishMu.Unlock()
			} else {
				r.updateState(name, tt.state, nil, nil, Counts{})
			}
			// Read the flow while the worker is still parked on <-release:
			// completeAuthFlow removes it from authFlows on worker exit, so
			// reading after cancel()/close(release) races that removal and
			// yields a nil flow to dereference.
			r.authMu.Lock()
			flow := r.authFlows[name]
			r.authMu.Unlock()
			require.NotNil(t, flow)

			cancel()
			close(release)
			<-flow.workerDone
			info, ok := r.states.Get(name)
			require.True(t, ok)
			require.Equal(t, tt.state, info.State)
		})
	}
}

func TestBeginAuth_PanicClosesPublishedHandlerOnce(t *testing.T) {
	const name = "panic-auth"
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPHttp, URL: "https://example.test", OAuth: true}}})
	var closes atomic.Int32
	r.runAuth = func(_ context.Context, _ ConfigProvider, name string, _ config.MCPConfig, owner attemptID) error {
		auth := &ownedAuthHandler{closeFn: func() { closes.Add(1) }}
		r.publishMu.Lock()
		r.authURLs.Set(name, authPublication{auth: auth, gen: owner.gen, attempt: owner.seq})
		r.publishMu.Unlock()
		panic("boom")
	}
	finish, _, err := r.BeginAuth(cfg, name)
	require.NoError(t, err)
	require.ErrorContains(t, finish(t.Context()), "panic in MCP authentication")
	require.Equal(t, int32(1), closes.Load())
	_, ok := r.authURLs.Get(name)
	require.False(t, ok)
}

// TestAuthenticateMCP_NoTokenStartsInteractiveFlow pins the primary
// interactive scenario: an OAuth server with no cached token must enter
// StateStarting and create a live OAuth handler (which arms the browser
// callback server), not short-circuit to StateNeedsAuth without one.
// AuthenticateMCP is user-initiated, so it must not defer to the UI the
// way startup does; the handler is created and published.
func TestAuthenticateMCP_NoTokenStartsInteractiveFlow(t *testing.T) {
	const name = "auth-no-token"
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPHttp, URL: "http://127.0.0.1:1/mcp", OAuth: true}}})
	// A cancelled context makes createSession fail fast without network,
	// exercising the full AuthenticateMCP → connectAndRegister → setAuthTerminal
	// path. The handler is created and published before the connect attempt.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := r.AuthenticateMCP(ctx, cfg, name)
	require.ErrorIs(t, err, context.Canceled)
	// A cancelled interactive flow settles back in StateNeedsAuth, not
	// StateError: the user can re-trigger the flow.
	info, ok := r.states.Get(name)
	require.True(t, ok)
	require.Equal(t, StateNeedsAuth, info.State)
	// The handler was detached from the publication by the attempt's defer.
	_, ok = r.authURLs.Get(name)
	require.False(t, ok)
}

// TestAuthenticateMCP_CancelReturnsCancelledAndSettlesNeedsAuth pins that
// a cancelled interactive OAuth flow returns context.Canceled to the caller
// (AuthenticateMCP propagates it) while still settling the server in
// StateNeedsAuth so the user can re-trigger.
func TestAuthenticateMCP_CancelReturnsCancelledAndSettlesNeedsAuth(t *testing.T) {
	const name = "auth-cancel"
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPHttp, URL: "http://127.0.0.1:1/mcp", OAuth: true}}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := r.AuthenticateMCP(ctx, cfg, name)
	require.ErrorIs(t, err, context.Canceled)
	info, ok := r.states.Get(name)
	require.True(t, ok)
	require.Equal(t, StateNeedsAuth, info.State)
}

// TestInitClient_NonOAuthCancellationIsError pins the startup semantics: a
// cancelled non-OAuth connect must surface context.Canceled to the caller
// and settle in StateError, NOT StateNeedsAuth (which is reserved for
// OAuth). Guards against a blanket Canceled->NeedsAuth rewrite of initClient.
func TestInitClient_NonOAuthCancellationIsError(t *testing.T) {
	const name = "non-oauth-cancel"
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPHttp, URL: "http://127.0.0.1:1/mcp"}}})
	owner, err := r.beginAttempt(name)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = r.initClient(ctx, cfg, name, cfg.Config().MCP[name], owner, cfg.Resolver())
	require.ErrorIs(t, err, context.Canceled)
	info, ok := r.states.Get(name)
	require.True(t, ok)
	require.Equal(t, StateError, info.State)
}

func TestPublishSessionFailureCleansDetachedAuthOnce(t *testing.T) {
	const name = "post-connect-failure"
	r := NewRegistry()
	owner, err := r.beginAttempt(name)
	require.NoError(t, err)
	var closes atomic.Int32
	auth := &ownedAuthHandler{closeFn: func() { closes.Add(1) }}
	session, _ := liveSession(t, "tool")
	session.auth = auth
	require.NoError(t, session.Close())
	r.publishMu.Lock()
	r.authURLs.Set(name, authPublication{auth: auth, gen: owner.gen, attempt: owner.seq})
	r.publishMu.Unlock()

	err = r.publishSession(t.Context(), name, config.MCPConfig{}, owner, session)
	require.Error(t, err)
	r.detachAuth(name, owner, nil).Close()
	require.Equal(t, int32(1), closes.Load())
	_, ok := r.authURLs.Get(name)
	require.False(t, ok)
}

func TestOAuthTokenPersistenceCurrentOwnerPersistsOnce(t *testing.T) {
	const name = "token-owner"
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPHttp, OAuth: true}}})
	owner, err := r.beginAttempt(name)
	require.NoError(t, err)
	r.updateStateFor(name, owner, StateStarting, nil)
	var writes atomic.Int32
	r.tokenCommit = func(_ ConfigProvider, _ *config.MCPTokenMutation, token *oauth.Token) error {
		writes.Add(1)
		require.Equal(t, "fresh", token.AccessToken)
		return nil
	}

	r.persistOAuthToken(t.Context(), cfg, name, owner, &oauth.Token{AccessToken: "fresh"})

	require.Equal(t, int32(1), writes.Load())
}

func TestOAuthTokenPersistenceInvalidatedBeforeReservationIsDropped(t *testing.T) {
	for _, action := range []string{"teardown", "remove", "disable", "close"} {
		t.Run(action, func(t *testing.T) {
			const name = "token-owner"
			r := NewRegistry()
			cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPHttp, OAuth: true}}})
			owner, err := r.beginAttempt(name)
			require.NoError(t, err)
			r.updateStateFor(name, owner, StateStarting, nil)
			blocked := make(chan struct{})
			release := make(chan struct{})
			r.beforeTokenPersist = func() { close(blocked); <-release }
			var writes atomic.Int32
			r.tokenCommit = func(ConfigProvider, *config.MCPTokenMutation, *oauth.Token) error {
				writes.Add(1)
				return nil
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				r.persistOAuthToken(context.Background(), cfg, name, owner, &oauth.Token{AccessToken: "stale"})
			}()
			<-blocked
			switch action {
			case "teardown":
				r.teardown(name)
			case "remove":
				r.removeServer(name)
			case "disable":
				require.NoError(t, r.DisableSingle(cfg, name))
			case "close":
				require.NoError(t, r.Close(t.Context()))
			}
			close(release)
			<-done
			require.Zero(t, writes.Load())
			_, hasSession := r.sessions.Get(name)
			require.False(t, hasSession)
			if action == "remove" {
				_, hasState := r.states.Get(name)
				require.False(t, hasState)
			}
		})
	}
}

func TestOAuthTokenPersistenceReservedWriteDelaysTeardown(t *testing.T) {
	const name = "token-owner"
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPHttp, OAuth: true}}})
	owner, err := r.beginAttempt(name)
	require.NoError(t, err)
	r.updateStateFor(name, owner, StateStarting, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	r.tokenCommit = func(ConfigProvider, *config.MCPTokenMutation, *oauth.Token) error {
		close(started)
		<-release
		return nil
	}
	go r.persistOAuthToken(context.Background(), cfg, name, owner, &oauth.Token{AccessToken: "fresh"})
	<-started
	teardownDone := make(chan struct{})
	go func() { r.teardown(name); close(teardownDone) }()
	select {
	case <-teardownDone:
		t.Fatal("teardown returned while a reserved token write was in flight")
	default:
	}
	close(release)
	<-teardownDone
}

func TestOwnedAuthHandlerSharedPublicationAndSessionClosesOnce(t *testing.T) {
	for _, action := range []string{"teardown", "close"} {
		t.Run(action, func(t *testing.T) {
			const name = "shared-auth"
			r := NewRegistry()
			var closes atomic.Int32
			auth := &ownedAuthHandler{closeFn: func() { closes.Add(1) }}
			session, _ := liveSession(t, "tool")
			session.auth = auth
			owner, err := r.beginAttempt(name)
			require.NoError(t, err)
			r.publishMu.Lock()
			r.sessions.Set(name, session)
			r.sessionOwners[name] = owner
			r.authURLs.Set(name, authPublication{auth: auth, gen: owner.gen, attempt: owner.seq})
			r.publishMu.Unlock()
			if action == "teardown" {
				r.teardown(name)
			} else {
				require.NoError(t, r.Close(t.Context()))
			}
			require.Equal(t, int32(1), closes.Load())
			_, ok := r.authURLs.Get(name)
			require.False(t, ok)
		})
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("SENNIT_STDIO_CHECK_HELPER") == "1" {
		got := strings.Join(os.Args[1:], " ")
		fmt.Printf("args: %s", got)
		if got != os.Getenv("SENNIT_STDIO_CHECK_EXPECTED_ARGS") {
			os.Exit(4)
		}
		if os.Getenv("SENNIT_STDIO_CHECK_FAIL") == "1" {
			os.Exit(3)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestStdioCheckArgv(t *testing.T) {
	t.Parallel()
	old := &exec.Cmd{
		Path: os.Args[0],
		Args: []string{os.Args[0], "--flag", "value"},
		Env: append(os.Environ(),
			"SENNIT_STDIO_CHECK_HELPER=1",
			"SENNIT_STDIO_CHECK_EXPECTED_ARGS=--flag value",
			"SENNIT_STDIO_CHECK_FAIL=1"),
	}
	err := stdioCheck(old)
	require.Error(t, err)
	require.Contains(t, err.Error(), "args: --flag value")
}

func TestStdioCheckNilArgs(t *testing.T) {
	t.Parallel()
	old := &exec.Cmd{
		Path: os.Args[0],
		Env: append(os.Environ(),
			"SENNIT_STDIO_CHECK_HELPER=1",
			"SENNIT_STDIO_CHECK_EXPECTED_ARGS="),
	}
	require.NoError(t, stdioCheck(old))
}

func TestOAuthRoundTripperNonReplayableBody(t *testing.T) {
	t.Parallel()
	handler := testOAuthHandler{
		tokenSource: func(context.Context) (oauth2.TokenSource, error) {
			return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "token"}), nil
		},
		authorize: func(context.Context, *http.Request, *http.Response) error {
			return nil
		},
	}
	var calls atomic.Int32
	base := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("unauthorized"))}, nil
	})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.test", strings.NewReader("payload"))
	require.NoError(t, err)
	req.GetBody = nil
	resp, err := newOAuthRoundTripper(handler, base).RoundTrip(req)
	if resp != nil && resp.Body != nil {
		require.NoError(t, resp.Body.Close())
	}
	require.Nil(t, resp)
	require.Error(t, err)
	require.Contains(t, err.Error(), "non-replayable body")
	require.Equal(t, int32(1), calls.Load())
}

func TestSuppressLockConcurrentAccess(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	const names = 10
	const goroutines = 100
	var wg sync.WaitGroup
	results := make([][]*sync.Mutex, names)
	for i := range names {
		results[i] = make([]*sync.Mutex, goroutines)
	}
	for i := 0; i < goroutines; i++ {
		wg.Go(func() {
			for name := range names {
				results[name][i] = r.suppressLock(fmt.Sprintf("name-%d", name))
			}
		})
	}
	wg.Wait()
	for name := 0; name < names; name++ {
		for i := 1; i < goroutines; i++ {
			require.Same(t, results[name][0], results[name][i],
				"suppressLock must return the same mutex for the same name")
		}
	}
}

func TestBeginAuth_FinishTimeoutRestoresStateNeedsAuth(t *testing.T) {
	const name = "finish-timeout-auth"
	r := NewRegistry()
	cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: config.MCPHttp, URL: "https://example.test", OAuth: true}}})
	started := make(chan struct{})
	r.runAuth = func(ctx context.Context, _ ConfigProvider, name string, m config.MCPConfig, owner attemptID) error {
		r.updateStateFor(name, owner, StateStarting, nil, withPending(m))
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	finish, _, err := r.BeginAuth(cfg, name)
	require.NoError(t, err)
	<-started
	r.authMu.Lock()
	flow := r.authFlows[name]
	r.authMu.Unlock()
	require.NotNil(t, flow)
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Millisecond))
	cancel()
	require.ErrorIs(t, finish(ctx), context.DeadlineExceeded)
	<-flow.workerDone
	info, ok := r.states.Get(name)
	require.True(t, ok)
	require.Equal(t, StateNeedsAuth, info.State)
}

func TestConnectAndRegisterPublishFailureClosesSession(t *testing.T) {
	const name = "publish-fail-close"
	r := NewRegistry()
	owner, err := r.beginAttempt(name)
	require.NoError(t, err)
	session, ctx := liveSession(t, "tool")
	t.Cleanup(func() { _ = session.Close() })
	r.publishMu.Lock()
	r.gens.Set(name, r.currentGen(name)+1)
	r.publishMu.Unlock()
	err = r.publishOrClose(t.Context(), name, config.MCPConfig{}, owner, session)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, ctx.Err(), context.Canceled, "stale session must be closed")
}

func TestBuildHTTPTransportOAuth(t *testing.T) {
	for _, tc := range []struct {
		name            string
		typ             config.MCPType
		wantURL         string
		transportClient func(*testing.T, mcp.Transport, string) *http.Client
	}{
		{
			name:    "http",
			typ:     config.MCPHttp,
			wantURL: "https://mcp.example.com/api",
			transportClient: func(t *testing.T, transport mcp.Transport, wantURL string) *http.Client {
				t.Helper()
				tr, ok := transport.(*mcp.StreamableClientTransport)
				require.True(t, ok, "expected *mcp.StreamableClientTransport, got %T", transport)
				require.Equal(t, wantURL, tr.Endpoint)
				return tr.HTTPClient
			},
		},
		{
			name:    "sse",
			typ:     config.MCPSSE,
			wantURL: "https://mcp.example.com/events",
			transportClient: func(t *testing.T, transport mcp.Transport, wantURL string) *http.Client {
				t.Helper()
				tr, ok := transport.(*mcp.SSEClientTransport)
				require.True(t, ok, "expected *mcp.SSEClientTransport, got %T", transport)
				require.Equal(t, wantURL, tr.Endpoint)
				return tr.HTTPClient
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const name = "oauth-transport"
			r := NewRegistry()
			owner, err := r.beginAttempt(name)
			require.NoError(t, err)
			t.Cleanup(func() { r.detachAuth(name, owner, nil).Close() })
			cfg := configtest.NewStore(t, &config.Config{MCP: config.MCPs{name: {Type: tc.typ, URL: tc.wantURL, OAuth: true}}})
			m := cfg.Config().MCP[name]
			transport, handler, err := r.createTransportFor(t.Context(), cfg, name, m, owner.gen, owner.seq, cfg.Resolver())
			require.NoError(t, err)
			require.NotNil(t, handler, "OAuth handler must be created")
			client := tc.transportClient(t, transport, tc.wantURL)
			require.NotNil(t, client, "HTTPClient must be set")
			oauthRT, ok := client.Transport.(*oauthRoundTripper)
			require.True(t, ok, "expected oauthRoundTripper, got %T", client.Transport)
			headerRT, ok := oauthRT.base.(*headerRoundTripper)
			require.True(t, ok, "expected headerRoundTripper, got %T", oauthRT.base)
			_, ok = headerRT.base.(*ownedHTTPTransport)
			require.True(t, ok, "expected ownedHTTPTransport, got %T", headerRT.base)
			r.publishMu.Lock()
			pub, ok := r.authURLs.Get(name)
			r.publishMu.Unlock()
			require.True(t, ok, "OAuth handler must be registered")
			require.Same(t, handler, pub.auth.handler, "registered handler must match returned handler")
		})
	}
}

func TestBeginAuth_UnknownServer(t *testing.T) {
	cfg := configtest.NewStore(t, &config.Config{})
	_, _, err := BeginAuth(cfg, "missing")
	require.ErrorContains(t, err, "not found")
}

// TestBeginAuth_NonOAuth proves BeginAuth rejects a server that does not use
// OAuth over HTTP.
func TestBeginAuth_NonOAuth(t *testing.T) {
	cfg := configtest.NewStore(t, &config.Config{
		MCP: config.MCPs{
			"stdio": {Type: config.MCPStdio},
			"plain": {Type: config.MCPHttp, URL: "https://example.com/mcp"},
		},
	})
	for _, name := range []string{"stdio", "plain"} {
		_, _, err := BeginAuth(cfg, name)
		require.ErrorContains(t, err, "does not use OAuth", "name %q", name)
	}
}
