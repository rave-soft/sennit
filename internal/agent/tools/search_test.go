package tools

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// serveSearchStub points the DuckDuckGo endpoint at a local server
// returning the given status and body, and restores it on cleanup.
func serveSearchStub(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	orig := ddgLiteEndpoint
	ddgLiteEndpoint = srv.URL + "/lite/?q="
	t.Cleanup(func() { ddgLiteEndpoint = orig })
}

// loadAnomalyPage reads the real bot-check payload captured from
// lite.duckduckgo.com on 2026-07-29 (HTTP 202, captcha modal).
func loadAnomalyPage(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/ddg_anomaly_202.html")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return string(data)
}

// TestSearchRateLimitedOn202 verifies the 202 anomaly-challenge
// interstitial is reported as throttling, not parsed into an empty
// result set.
func TestSearchRateLimitedOn202(t *testing.T) {
	serveSearchStub(t, http.StatusAccepted, loadAnomalyPage(t))
	_, err := searchDuckDuckGo(context.Background(), http.DefaultClient, "anything", 10)
	if !errors.Is(err, errSearchRateLimited) {
		t.Fatalf("expected errSearchRateLimited, got %v", err)
	}
}

// TestSearchRateLimitedOnAnomalyPage verifies the captcha modal is
// detected by content as well: DuckDuckGo also serves the same page
// with HTTP 200 once a client is flagged.
func TestSearchRateLimitedOnAnomalyPage(t *testing.T) {
	serveSearchStub(t, http.StatusOK, loadAnomalyPage(t))
	_, err := searchDuckDuckGo(context.Background(), http.DefaultClient, "anything", 10)
	if !errors.Is(err, errSearchRateLimited) {
		t.Fatalf("expected errSearchRateLimited, got %v", err)
	}
}

// TestSearchParsesNormalResults guards against false positives: a
// genuine results page still parses.
func TestSearchParsesNormalResults(t *testing.T) {
	page := `<html><body><table>
<tr><td><a class="result-link" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fpost">Example Post</a></td></tr>
<tr><td class="result-snippet">A snippet about the example post.</td></tr>
</table></body></html>`
	serveSearchStub(t, http.StatusOK, page)
	results, err := searchDuckDuckGo(context.Background(), http.DefaultClient, "example", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].Link != "https://example.com/post" {
		t.Fatalf("unexpected results: %+v", results)
	}
}

// TestMaybeDelaySearchRespectsContext verifies that a cancelled ctx
// interrupts maybeDelaySearch immediately instead of always waiting out
// the full (up to 2s) inter-search gap.
func TestMaybeDelaySearchRespectsContext(t *testing.T) {
	lastSearchMu.Lock()
	saved := lastSearchTime
	// Force a wait: the next call falls within minGap of "now".
	lastSearchTime = time.Now()
	lastSearchMu.Unlock()
	t.Cleanup(func() {
		lastSearchMu.Lock()
		lastSearchTime = saved
		lastSearchMu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := maybeDelaySearch(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("maybeDelaySearch took %v after ctx cancellation; want it to return promptly", elapsed)
	}
}

// TestMaybeDelaySearchDoesNotHoldLockDuringWait verifies the mutex is
// released before the wait, so a second caller can reserve its own slot
// (and, if its own wait is short or its ctx is cancelled, return) without
// blocking behind the first caller's full delay.
func TestMaybeDelaySearchDoesNotHoldLockDuringWait(t *testing.T) {
	lastSearchMu.Lock()
	saved := lastSearchTime
	lastSearchTime = time.Now()
	lastSearchMu.Unlock()
	t.Cleanup(func() {
		lastSearchMu.Lock()
		lastSearchTime = saved
		lastSearchMu.Unlock()
	})

	done := make(chan struct{})
	go func() {
		_ = maybeDelaySearch(context.Background())
		close(done)
	}()

	// Give the goroutine a moment to enter its wait, then verify the lock
	// is free: acquiring it here would block for ~2s under the old
	// implementation, which slept while holding lastSearchMu.
	time.Sleep(10 * time.Millisecond)
	lockAcquired := make(chan struct{})
	go func() {
		// TryLock rather than Lock: it makes the assertion "the mutex is
		// not held across the wait" directly, and avoids parking this
		// goroutine for the full delay if the old behaviour ever returns.
		for !lastSearchMu.TryLock() {
			time.Sleep(time.Millisecond)
		}
		lastSearchMu.Unlock()
		close(lockAcquired)
	}()

	select {
	case <-lockAcquired:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("lastSearchMu is still held while another call is waiting out its delay")
	}

	<-done
}
