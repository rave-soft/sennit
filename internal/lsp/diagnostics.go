package lsp

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/rave-soft/sennit/internal/csync"
)

// DiagnosticCounts holds the count of diagnostics by severity.
type DiagnosticCounts struct {
	Error       int
	Warning     int
	Information int
	Hint        int
}

// diagnosticsStore owns the published diagnostics: the versioned map, the
// per-generation event queue that orders store mutations against
// generation swaps, and the change callback. All mutations of the store
// happen in a single dispatcher goroutine, which is what makes
// "reset on restart" atomic with respect to in-flight publishes from a
// dying server.
//
// Generation publication and shutdown quiescence are explicit:
//
//   - the dispatcher tracks the event it is currently executing (inFlight)
//     rather than inferring it from the queue length, so shutdown can wait
//     for exactly the in-flight callback to finish;
//   - publishGeneration makes the new generation active and retires the
//     old one under the shared publication gate (runtime.genMu, held by
//     runtime.publishSwap around this call and by every
//     currentGeneration() reader), so no observer can see the new runtime
//     generation while diagnostics still treats the old one as active;
//   - requestShutdown marks the store stopped under d.mu; the Client then
//     waits for the dispatcher to terminate when external quiescence is
//     required.
type diagnosticsStore struct {
	mu   sync.Mutex
	cond *sync.Cond

	events        []diagnosticEvent
	inFlight      *diagnosticEvent // event currently executing, nil between events
	active        *clientGeneration
	stop          bool
	done          chan struct{}
	hook          func() // test seam: lets a test step the dispatch loop deterministically
	beforeEnqueue func() // test-only deterministic publish/shutdown interleaving
	store         *csync.VersionedMap[protocol.DocumentURI, []protocol.Diagnostic]
	counts        DiagnosticCounts
	version       uint64

	name     string
	cbMu     sync.RWMutex
	onChange func(name string, count int)
}

type diagnosticEvent struct {
	generation *clientGeneration
	prepare    func() func()
	run        func()
}

func newDiagnosticsStore(name string, active *clientGeneration) *diagnosticsStore {
	d := &diagnosticsStore{
		active: active,
		store:  csync.NewVersionedMap[protocol.DocumentURI, []protocol.Diagnostic](),
		name:   name,
		done:   make(chan struct{}),
	}
	d.cond = sync.NewCond(&d.mu)
	go d.dispatch()
	return d
}

// dispatch runs the single-writer loop. Events are executed in FIFO order;
// an event carrying a generation that is no longer active is dropped, which
// is what discards a dying server's late publishes after a restart.
//
// Quiescence is explicit: while an event executes, inFlight names it. A
// store marked stop while an event is running becomes quiescent only when
// that event returns and no further events remain; until then the
// dispatcher keeps running the final events (e.g. the zero-count callback
// for data present at shutdown) so the UI never keeps showing pre-shutdown
// totals, and then exits, closing done.
func (d *diagnosticsStore) dispatch() {
	defer close(d.done)
	for {
		d.mu.Lock()
		if d.inFlight != nil {
			panic("diagnosticsStore: dispatch loop re-entered with an in-flight event")
		}
		for len(d.events) == 0 && !d.stop {
			d.cond.Wait()
		}
		if len(d.events) == 0 && d.stop {
			d.mu.Unlock()
			return
		}
		event := d.events[0]
		d.events[0] = diagnosticEvent{}
		d.events = d.events[1:]
		d.inFlight = &event
		hook := d.hook
		d.mu.Unlock()
		if hook != nil {
			hook()
		}
		// prepare runs under d.mu so the store mutation is atomic with
		// the generation check. The returned run closure is called with
		// d.mu released, so the change callback can call back into the
		// store (e.g. Restart or Shutdown from a callback) without
		// deadlocking.
		d.mu.Lock()
		if event.generation != nil && d.active != event.generation && d.active != nil {
			d.clearInFlightLocked()
			d.mu.Unlock()
			continue
		}
		run := event.run
		if event.prepare != nil {
			run = event.prepare()
		}
		d.mu.Unlock()
		if run != nil {
			run()
		}
		d.mu.Lock()
		d.clearInFlightLocked()
		d.cond.Broadcast()
		d.mu.Unlock()
	}
}

// clearInFlightLocked forgets the in-flight event. Callers hold d.mu.
func (d *diagnosticsStore) clearInFlightLocked() {
	d.inFlight = nil
}

// enqueue queues an event for the dispatcher. It reports whether the event
// was accepted (a stopped store, or a stale generation, rejects it).
func (d *diagnosticsStore) enqueue(event diagnosticEvent) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stop || event.generation != nil && d.active != event.generation {
		return false
	}
	d.events = append(d.events, event)
	d.cond.Signal()
	return true
}

// publishGeneration swaps the active generation, clears the store, and
// drops every queued event that still belongs to a previous generation.
// If the store held data, a zero-count callback is queued so the UI never
// shows the old totals under the new server.
//
// The swap must run under the runtime's publication gate (genMu): it is
// taken around this call by runtime.publishSwap together with the runtime
// generation assignment, which is what makes the two half-swaps atomic
// with respect to every currentGeneration() reader. A diagnostics
// notification for the new generation arriving immediately after
// publication is therefore accepted (active is already the new
// generation), never dropped as "active old".
func (d *diagnosticsStore) publishGeneration(oldGen, newGen *clientGeneration) {
	d.mu.Lock()
	d.swapGenerationLocked(oldGen, newGen)
	d.mu.Unlock()
}

// swapGenerationLocked performs the atomic part of a generation swap.
// Callers hold d.mu.
func (d *diagnosticsStore) swapGenerationLocked(oldGen, newGen *clientGeneration) {
	if oldGen != nil {
		oldGen.markRetired()
	}
	hadDiagnostics := d.store.Version() != 0
	d.resetLocked()
	d.active = newGen
	d.purgeLocked()
	if hadDiagnostics {
		d.events = append(d.events, diagnosticEvent{run: func() {
			d.invokeCallback(0)
		}})
		d.cond.Signal()
	}
}

// requestShutdown establishes the diagnostics shutdown linearization point.
// It sets stop while holding d.mu, purges queued generation events, and
// appends the one permitted terminal zero-count event when diagnostics were
// present. It deliberately does not wait for done: the dispatcher may be
// executing the diagnostics callback that requested shutdown.
//
// The Client owns the external quiescence barrier by waiting on d.done after
// this method returns. Repeated calls are idempotent.
func (d *diagnosticsStore) requestShutdown() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stop {
		return
	}

	hadDiagnostics := d.store.Version() != 0
	if d.active != nil {
		d.active.markRetired()
	}
	d.resetLocked()
	d.active = nil
	d.purgeLocked()
	if hadDiagnostics {
		d.events = append(d.events, diagnosticEvent{run: func() {
			d.invokeCallback(0)
		}})
	}
	d.stop = true
	d.cond.Broadcast()
}

func (d *diagnosticsStore) purgeLocked() {
	kept := d.events[:0]
	for _, event := range d.events {
		if event.generation == nil {
			kept = append(kept, event)
		}
	}
	d.events = kept
}

func (d *diagnosticsStore) resetLocked() {
	d.store = csync.NewVersionedMap[protocol.DocumentURI, []protocol.Diagnostic]()
	d.counts = DiagnosticCounts{}
	d.version = 0
}

func (d *diagnosticsStore) invokeCallback(count int) {
	d.cbMu.RLock()
	callback := d.onChange
	d.cbMu.RUnlock()
	if callback != nil {
		callback(d.name, count)
	}
}

// publish handles a textDocument/publishDiagnostics notification from the
// given generation: the map mutation and the count callback are queued as a
// single event so they land as one ordered step.
func (d *diagnosticsStore) publish(gen *clientGeneration, params json.RawMessage) {
	var diagParams protocol.PublishDiagnosticsParams
	if err := json.Unmarshal(params, &diagParams); err != nil {
		slog.Error("Error unmarshaling diagnostics params", "error", err)
		return
	}
	d.publishDiag(gen, diagParams, nil)
}

// clearURI clears any diagnostics recorded for uri, as if the server had
// itself published an empty diagnostics list — the same event-ordering
// path publishDiag uses, so this can never race a real publish for the
// same generation into landing out of order. Used when a file that was
// open vanishes from disk: its stale diagnostics must not keep showing up
// in project_diagnostics for the rest of the session.
func (d *diagnosticsStore) clearURI(gen *clientGeneration, uri protocol.DocumentURI) {
	d.publishDiag(gen, protocol.PublishDiagnosticsParams{URI: uri}, nil)
}

// publishDiag is like publish but with an explicit before-callback that
// runs inside the event, just before the change callback. The store
// mutation happens under d.mu (held by the dispatcher), so no separate
// locking is needed inside the prepare closure.
func (d *diagnosticsStore) publishDiag(gen *clientGeneration, diagParams protocol.PublishDiagnosticsParams, beforeCallback func()) {
	if hook := d.beforeEnqueue; hook != nil {
		hook()
	}
	// enqueue rechecks stop while holding d.mu. Thus parsing that began
	// before shutdown cannot admit an event after shutdown's reset/purge
	// linearization point.
	d.enqueue(diagnosticEvent{generation: gen, prepare: func() func() {
		// d.mu is held by the dispatcher when prepare runs.
		d.store.Set(diagParams.URI, diagParams.Diagnostics)
		totalCount := 0
		for _, diagnostics := range d.store.Seq2() {
			totalCount += len(diagnostics)
		}
		return func() {
			if beforeCallback != nil {
				beforeCallback()
			}
			d.invokeCallback(totalCount)
		}
	}})
}

// SetDiagnosticsCallback sets the callback function for diagnostic changes.
func (d *diagnosticsStore) SetDiagnosticsCallback(callback func(name string, count int)) {
	d.cbMu.Lock()
	d.onChange = callback
	d.cbMu.Unlock()
}

// waitForDrain blocks until the dispatcher has executed every event queued
// before the call, so a test (or caller) can observe settled state.
//
// test seam: only client_test.go calls this today.
func (d *diagnosticsStore) waitForDrain() {
	done := make(chan struct{})
	if !d.enqueue(diagnosticEvent{run: func() { close(done) }}) {
		return
	}
	select {
	case <-done:
	case <-d.done:
	}
}

// GetFileDiagnostics returns diagnostics for a specific file.
func (d *diagnosticsStore) getFileDiagnostics(uri protocol.DocumentURI) []protocol.Diagnostic {
	d.mu.Lock()
	defer d.mu.Unlock()
	diags, _ := d.store.Get(uri)
	// Clone: the slice we hand back aliases the store's backing array, so
	// a caller mutating or appending to it (within capacity) would
	// silently corrupt or observe changes to internal state. getDiagnostics
	// copies for the same reason.
	return slices.Clone(diags)
}

// GetDiagnostics returns all diagnostics for all files.
func (d *diagnosticsStore) getDiagnostics() map[protocol.DocumentURI][]protocol.Diagnostic {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.store.Copy()
}

// reset clears all diagnostics. It must only be called as part of a
// generation swap, never concurrently with publishes.
//
// test seam: production code reaches the same effect through resetLocked
// under its own already-held mu (see publishSwap and the constructor);
// reset is the mutex-acquiring wrapper client_test.go uses directly since
// its tests don't already hold d.mu.
func (d *diagnosticsStore) reset() {
	d.mu.Lock()
	d.resetLocked()
	d.mu.Unlock()
}

// GetDiagnosticCounts returns cached diagnostic counts by severity.
// Uses the VersionedMap version to avoid recomputing on every call.
//
// The content and its version come from one Snapshot rather than from a
// Version() call followed by a separate read: taken apart, a writer
// landing between the two would stamp the cache with a version older than
// the content it was computed from, so the next call would recompute
// counts it already had.
func (d *diagnosticsStore) getDiagnosticCounts() DiagnosticCounts {
	d.mu.Lock()
	defer d.mu.Unlock()
	content, currentVersion := d.store.Snapshot()
	if currentVersion == d.version {
		return d.counts
	}

	// Recompute counts.
	counts := DiagnosticCounts{}
	for _, diags := range content {
		for _, diag := range diags {
			switch diag.Severity {
			case protocol.SeverityError:
				counts.Error++
			case protocol.SeverityWarning:
				counts.Warning++
			case protocol.SeverityInformation:
				counts.Information++
			case protocol.SeverityHint:
				counts.Hint++
			}
		}
	}

	d.counts = counts
	d.version = currentVersion
	return counts
}

// waitForDiagnostics waits until diagnostics stop changing for a settling
// period, indicating the LSP server has finished processing. If no
// diagnostics change within firstChangeDuration, it returns early since the
// server likely isn't going to republish.
func (d *diagnosticsStore) waitForDiagnostics(
	ctx context.Context,
	timeout, firstChangeDuration, settleDuration, pollInterval time.Duration,
) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	firstChangeTimer := time.NewTimer(min(timeout, firstChangeDuration))
	defer firstChangeTimer.Stop()
	previousVersion := d.versionLocked()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-firstChangeTimer.C:
			// No change arrived quickly — server isn't republishing.
			return
		case <-ticker.C:
			currentVersion := d.versionLocked()
			if currentVersion != previousVersion {
				// Diagnostics changed — now wait for them to settle.
				d.waitForDiagnosticsToSettle(ctx, deadline.C, settleDuration, pollInterval/2)
				return
			}
		}
	}
}

func (d *diagnosticsStore) versionLocked() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.store.Version()
}

// waitForDiagnosticsToSettle waits until diagnostics version stays the same
// for settleDuration, indicating the LSP server has finished publishing.
func (d *diagnosticsStore) waitForDiagnosticsToSettle(
	ctx context.Context,
	deadline <-chan time.Time,
	settleDuration, pollInterval time.Duration,
) {
	lastVersion := d.versionLocked()
	settleTicker := time.NewTicker(pollInterval)
	defer settleTicker.Stop()

	// Track how long the version has been stable.
	stableStart := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case <-settleTicker.C:
			currentVersion := d.versionLocked()
			if currentVersion != lastVersion {
				// New change detected — reset the stable timer.
				lastVersion = currentVersion
				stableStart = time.Now()
			} else if time.Since(stableStart) >= settleDuration {
				// Diagnostics have been stable for the settle duration.
				return
			}
		}
	}
}
