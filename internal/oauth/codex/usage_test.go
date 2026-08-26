package codex

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/providers/accounts"
)

// liveHeaders mirrors what the Codex backend actually returns, captured
// from a real response.
func liveHeaders() http.Header {
	h := http.Header{}
	h.Set("X-Codex-Plan-Type", "plus")
	h.Set("X-Codex-Active-Limit", "premium")
	h.Set("X-Codex-Primary-Used-Percent", "6")
	h.Set("X-Codex-Primary-Window-Minutes", "10080")
	h.Set("X-Codex-Primary-Reset-At", "1787204853")
	h.Set("X-Codex-Primary-Reset-After-Seconds", "298494")
	h.Set("X-Codex-Secondary-Used-Percent", "0")
	h.Set("X-Codex-Secondary-Window-Minutes", "0")
	h.Set("X-Codex-Secondary-Reset-At", "")
	return h
}

func TestParseUsage(t *testing.T) {
	t.Parallel()

	usage, ok := ParseUsage(liveHeaders())
	require.True(t, ok)
	require.Equal(t, "plus", usage.Plan)
	require.Equal(t, 6, usage.Primary.UsedPercent)
	require.Equal(t, 10080, usage.Primary.WindowMinutes)
	require.Equal(t, int64(1787204853), usage.Primary.ResetsAt.Unix())
	require.True(t, usage.Primary.Known())
	require.False(t, usage.Secondary.Known(),
		"a zero-length window is one the account does not have, not an empty one")
	require.WithinDuration(t, time.Now(), usage.CapturedAt, time.Minute)
}

// TestParseUsageIgnoresOtherResponses: every other provider's responses go
// through the same code path, and none of them carry these headers.
func TestParseUsageIgnoresOtherResponses(t *testing.T) {
	t.Parallel()

	_, ok := ParseUsage(http.Header{})
	require.False(t, ok)

	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	_, ok = ParseUsage(h)
	require.False(t, ok)
}

// TestParseUsageToleratesGarbage: a header that is missing or unparseable
// must not take the whole snapshot down with it.
func TestParseUsageToleratesGarbage(t *testing.T) {
	t.Parallel()

	h := http.Header{}
	h.Set("X-Codex-Plan-Type", "pro")
	h.Set("X-Codex-Primary-Used-Percent", "not-a-number")
	h.Set("X-Codex-Primary-Reset-At", "")

	usage, ok := ParseUsage(h)
	require.True(t, ok)
	require.Equal(t, "pro", usage.Plan)
	require.Zero(t, usage.Primary.UsedPercent)
	require.True(t, usage.Primary.ResetsAt.IsZero())
}

// TestUsageTransportRecords proves the figures reach the store from an
// ordinary response, which is the only way they are ever collected — there
// is no separate poll.
func TestUsageTransportRecords(t *testing.T) {
	resetUsage()
	t.Cleanup(resetUsage)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range liveHeaders() {
			w.Header()[k] = v
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Transport: NewUsageTransport(nil)}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	usage, ok := LatestUsage()
	require.True(t, ok)
	require.Equal(t, "plus", usage.Plan)
	require.Equal(t, 6, usage.Primary.UsedPercent)
}

// TestUsageTransportKeepsLastKnown: a response without the headers (an
// error page, another host behind the same client) must not wipe what is
// already known.
func TestUsageTransportKeepsLastKnown(t *testing.T) {
	resetUsage()
	t.Cleanup(resetUsage)

	RecordUsage(Usage{Plan: "pro", Primary: UsageWindow{UsedPercent: 40, WindowMinutes: 300}})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Transport: NewUsageTransport(nil)}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	usage, ok := LatestUsage()
	require.True(t, ok)
	require.Equal(t, "pro", usage.Plan)
	require.Equal(t, 40, usage.Primary.UsedPercent)
}

// TestUsageForKeepsAccountsSeparate proves one account's snapshot does not
// clobber another's, which is the whole point of moving off a singleton.
func TestUsageForKeepsAccountsSeparate(t *testing.T) {
	resetUsage()
	t.Cleanup(resetUsage)

	RecordUsageFor("a", Usage{Plan: "plus", Primary: UsageWindow{UsedPercent: 10, WindowMinutes: 60}})
	RecordUsageFor("b", Usage{Plan: "pro", Primary: UsageWindow{UsedPercent: 90, WindowMinutes: 60}})

	a, ok := UsageFor("a")
	require.True(t, ok)
	require.Equal(t, "plus", a.Plan)
	require.Equal(t, 10, a.Primary.UsedPercent)

	b, ok := UsageFor("b")
	require.True(t, ok)
	require.Equal(t, "pro", b.Plan)
	require.Equal(t, 90, b.Primary.UsedPercent)
}

// TestUsageForUnknownAccount: an account that never made a request has no
// snapshot to report.
func TestUsageForUnknownAccount(t *testing.T) {
	resetUsage()
	t.Cleanup(resetUsage)

	u, ok := UsageFor("nobody")
	require.False(t, ok)
	require.Equal(t, Usage{}, u)
}

// TestLatestUsageFollowsMostRecentAccount proves LatestUsage keeps its old
// singleton semantics — the most recent snapshot from any account — even
// though snapshots are now stored per account.
func TestLatestUsageFollowsMostRecentAccount(t *testing.T) {
	resetUsage()
	t.Cleanup(resetUsage)

	RecordUsageFor("a", Usage{Plan: "plus", Primary: UsageWindow{UsedPercent: 1, WindowMinutes: 60}})
	RecordUsageFor("b", Usage{Plan: "pro", Primary: UsageWindow{UsedPercent: 2, WindowMinutes: 60}})

	latest, ok := LatestUsage()
	require.True(t, ok)
	require.Equal(t, "pro", latest.Plan)
	require.Equal(t, 2, latest.Primary.UsedPercent)
}

// TestUsageTransportRecordsPerAccount proves the transport reads the
// chatgpt-account-id header off the outgoing request and files the snapshot
// under it, so concurrent accounts on the same process do not share one
// allowance.
func TestUsageTransportRecordsPerAccount(t *testing.T) {
	resetUsage()
	t.Cleanup(resetUsage)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range liveHeaders() {
			w.Header()[k] = v
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Transport: NewUsageTransport(nil)}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	req.Header.Set(AccountIDHeader, "acct-42")
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	usage, ok := UsageFor("acct-42")
	require.True(t, ok)
	require.Equal(t, "plus", usage.Plan)
	require.Equal(t, 6, usage.Primary.UsedPercent)

	_, ok = UsageFor("")
	require.False(t, ok)
}

// TestUsageTransportRecordsWithoutAccountHeader: personal plans never send
// chatgpt-account-id, and their snapshot still has to land somewhere — the
// empty accountID.
func TestUsageTransportRecordsWithoutAccountHeader(t *testing.T) {
	resetUsage()
	t.Cleanup(resetUsage)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range liveHeaders() {
			w.Header()[k] = v
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Transport: NewUsageTransport(nil)}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	usage, ok := UsageFor("")
	require.True(t, ok)
	require.Equal(t, "plus", usage.Plan)

	latest, ok := LatestUsage()
	require.True(t, ok)
	require.Equal(t, "plus", latest.Plan)
}

// TestUsageSnapshot proves the conversion to accounts.Usage is a straight
// field copy, both windows and CapturedAt included, and that a zero Usage
// converts to a zero accounts.Usage.
func TestUsageSnapshot(t *testing.T) {
	t.Parallel()

	captured := time.Unix(1700000000, 0)
	resets1 := time.Unix(1700003600, 0)
	resets2 := time.Unix(1700007200, 0)
	u := Usage{
		Plan: "pro",
		Primary: UsageWindow{
			UsedPercent:   42,
			WindowMinutes: 10080,
			ResetsAt:      resets1,
		},
		Secondary: UsageWindow{
			UsedPercent:   7,
			WindowMinutes: 60,
			ResetsAt:      resets2,
		},
		CapturedAt: captured,
	}

	want := accounts.Usage{
		Plan: "pro",
		Primary: accounts.UsageWindow{
			UsedPercent:   42,
			WindowMinutes: 10080,
			ResetsAt:      resets1,
		},
		Secondary: accounts.UsageWindow{
			UsedPercent:   7,
			WindowMinutes: 60,
			ResetsAt:      resets2,
		},
		CapturedAt: captured,
	}
	require.Equal(t, want, u.Snapshot())

	require.Equal(t, accounts.Usage{}, Usage{}.Snapshot())
}

// withTestAPIBase points apiBaseURL at srv for the duration of a test and
// restores it afterward, so FetchUsage's request lands on a fixture instead
// of the real Codex backend.
func withTestAPIBase(t *testing.T, srv *httptest.Server) {
	t.Helper()
	original := apiBaseURL
	apiBaseURL = srv.URL
	t.Cleanup(func() { apiBaseURL = original })
}

// TestFetchUsageHeadersPresent: when the response carries the X-Codex-*
// headers, FetchUsage reports them and does not treat their presence as an
// error.
func TestFetchUsageHeadersPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/models", r.URL.Path)
		require.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		for k, v := range liveHeaders() {
			w.Header()[k] = v
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	withTestAPIBase(t, srv)

	usage, ok, err := FetchUsage(t.Context(), "", "tok", "acct-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "plus", usage.Plan)
	require.Equal(t, 6, usage.Primary.UsedPercent)
}

// TestFetchUsageHeadersAbsent: no X-Codex-* headers is a plain "unknown",
// not an error — the endpoint answering without them is expected until the
// assumption in FetchUsage's doc comment is checked against production.
func TestFetchUsageHeadersAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	withTestAPIBase(t, srv)

	usage, ok, err := FetchUsage(t.Context(), "", "tok", "acct-1")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, Usage{}, usage)
}

// TestFetchUsageErrorStatus: a non-200 response is an error, same as
// FetchModels treats one.
func TestFetchUsageErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("account suspended"))
	}))
	t.Cleanup(srv.Close)
	withTestAPIBase(t, srv)

	_, ok, err := FetchUsage(t.Context(), "", "tok", "acct-1")
	require.Error(t, err)
	require.False(t, ok)
	require.Contains(t, err.Error(), "account suspended")
}

// TestFetchUsageDoesNotRecord guards the requirement that FetchUsage never
// writes into the shared store: it is asked about accounts that may not be
// the active one, and recording would silently make one of them "latest"
// and hand the sidebar someone else's numbers.
func TestFetchUsageDoesNotRecord(t *testing.T) {
	resetUsage()
	t.Cleanup(resetUsage)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range liveHeaders() {
			w.Header()[k] = v
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	withTestAPIBase(t, srv)

	usage, ok, err := FetchUsage(t.Context(), "", "tok", "acct-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "plus", usage.Plan)

	_, ok = LatestUsage()
	require.False(t, ok, "FetchUsage must not write into the shared store")
	_, ok = UsageFor("acct-1")
	require.False(t, ok, "FetchUsage must not write into the shared store")
}
