package codex

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
