package shell

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	MaxBackgroundJobs            = 50
	CompletedJobRetentionMinutes = 8 * 60

	// MaxSyncBufferHead is the number of bytes retained from the start of
	// a SyncBuffer's stream. Bytes beyond this are still counted (so the
	// dropped-byte marker is accurate) but never copied into the head.
	MaxSyncBufferHead = 256 * 1024

	// MaxSyncBufferTail is the number of bytes retained from the end of a
	// SyncBuffer's stream, via a fixed-size ring buffer that always holds
	// the most recently written bytes. Together with MaxSyncBufferHead
	// this bounds a single stream to 512 KiB regardless of how much is
	// written — MaxBackgroundJobs background shells each hold two streams
	// (stdout, stderr), so the process-wide ceiling is ~50 MiB instead of
	// unbounded.
	MaxSyncBufferTail = 256 * 1024
)

// SyncBuffer is a mutex-protected io.Writer with a bounded memory footprint:
// it retains the first MaxSyncBufferHead bytes written and the last
// MaxSyncBufferTail bytes, dropping whatever falls in between. This mirrors
// the head+tail truncation [tools.TruncateOutput] already applies when
// rendering bash output, so a chatty background command (a build, a test
// run, `tail -f`) can no longer grow this buffer without bound while the
// caller still sees the start and the most recent progress/error.
//
// The zero value is ready to use — [BackgroundShell] and RunAndCapture both
// rely on that, so initialization happens lazily inside Write under the same
// lock rather than through a constructor.
type SyncBuffer struct {
	mu sync.RWMutex

	// bounded-buffer: writeLocked only ever appends up to the
	// MaxSyncBufferHead room left in it, so this stops growing at 256 KiB.
	head bytes.Buffer // first min(total, MaxSyncBufferHead) bytes, never overwritten

	tail    []byte // ring buffer of capacity MaxSyncBufferTail, lazily allocated
	tailPos int    // index in tail where the next byte will be written
	tailLen int    // valid bytes currently held in tail (<= MaxSyncBufferTail)

	total int64 // total bytes ever written, including dropped ones
}

// Write appends p to the buffer, retaining it subject to the head/tail
// caps. It always reports len(p) written with a nil error — dropping the
// middle of the stream must never look like a short write to callers such
// as mvdan.cc/sh's interp, which treats one as fatal.
func (sb *SyncBuffer) Write(p []byte) (n int, err error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.writeLocked(p)
	return len(p), nil
}

// WriteString is the string equivalent of Write, avoiding a redundant
// []byte(s) round trip through a second lock acquisition.
func (sb *SyncBuffer) WriteString(s string) (n int, err error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.writeLocked([]byte(s))
	return len(s), nil
}

// writeLocked applies p to both the head buffer and the tail ring. Callers
// must hold sb.mu.
//
// The tail ring is left unallocated until the stream first grows past
// MaxSyncBufferHead: up to that point every byte already lives in head, so
// a ring would only duplicate it, and the common case — a short command's
// small output — must not pay for a 256 KiB allocation it never needs.
func (sb *SyncBuffer) writeLocked(p []byte) {
	prevTotal := sb.total
	sb.total += int64(len(p))

	if room := MaxSyncBufferHead - sb.head.Len(); room > 0 {
		sb.head.Write(p[:min(room, len(p))])
	}

	if sb.total <= MaxSyncBufferHead {
		return
	}
	if sb.tail == nil {
		// Crossing the cap for the first time: seed the ring with
		// everything written before this call. All of it is still sitting
		// in head (prevTotal <= MaxSyncBufferHead here, since this is the
		// first crossing), so head.Bytes() is exactly those prevTotal
		// bytes. Seed before appending p so nothing is double-counted.
		if prevTotal > 0 {
			sb.writeTailLocked(sb.head.Bytes()[:prevTotal])
		}
	}
	sb.writeTailLocked(p)
}

// writeTailLocked copies p into the fixed-size tail ring buffer, keeping
// only the most recently written MaxSyncBufferTail bytes. Callers must hold
// sb.mu.
func (sb *SyncBuffer) writeTailLocked(p []byte) {
	if len(p) == 0 {
		return
	}
	if sb.tail == nil {
		sb.tail = make([]byte, MaxSyncBufferTail)
	}

	// A single write already exceeding the tail capacity fully replaces
	// it; only its own last MaxSyncBufferTail bytes can survive.
	if len(p) >= MaxSyncBufferTail {
		copy(sb.tail, p[len(p)-MaxSyncBufferTail:])
		sb.tailPos = 0
		sb.tailLen = MaxSyncBufferTail
		return
	}

	end := sb.tailPos + len(p)
	if end <= MaxSyncBufferTail {
		copy(sb.tail[sb.tailPos:end], p)
	} else {
		first := MaxSyncBufferTail - sb.tailPos
		copy(sb.tail[sb.tailPos:], p[:first])
		copy(sb.tail[:end-MaxSyncBufferTail], p[first:])
	}
	sb.tailPos = end % MaxSyncBufferTail
	sb.tailLen = min(sb.tailLen+len(p), MaxSyncBufferTail)
}

// tailBytesLocked returns the tail ring's contents in write order (oldest
// first). Callers must hold sb.mu.
func (sb *SyncBuffer) tailBytesLocked() []byte {
	if sb.tailLen < MaxSyncBufferTail {
		// No wraparound yet: the ring has been written from index 0, so
		// tailPos already equals tailLen and the data is contiguous.
		return sb.tail[:sb.tailLen]
	}
	out := make([]byte, MaxSyncBufferTail)
	n := copy(out, sb.tail[sb.tailPos:])
	copy(out[n:], sb.tail[:sb.tailPos])
	return out
}

// droppedMarker formats the marker String inserts between head and tail
// once bytes have actually been discarded, matching the style of
// [tools.TruncateOutput]'s "... [N lines truncated] ..." marker.
func droppedMarker(dropped int64) string {
	return fmt.Sprintf("\n\n... [%d bytes dropped] ...\n\n", dropped)
}

// String returns the retained head, a dropped-byte marker if anything was
// discarded, and the retained tail — reconstructing the exact original
// content whenever total writes fit within the combined head+tail caps.
func (sb *SyncBuffer) String() string {
	sb.mu.RLock()
	defer sb.mu.RUnlock()

	headLen := int64(sb.head.Len())
	tailLen := int64(sb.tailLen)
	tail := sb.tailBytesLocked()
	dropped := sb.total - headLen - tailLen

	var b strings.Builder
	if dropped <= 0 {
		// Head and tail overlap or touch: everything fits, so skip the
		// prefix of tail that head already covers and reassemble exactly.
		skip := min(-dropped, int64(len(tail)))
		b.Grow(sb.head.Len() + len(tail) - int(skip))
		b.Write(sb.head.Bytes())
		b.Write(tail[skip:])
		return b.String()
	}

	marker := droppedMarker(dropped)
	b.Grow(sb.head.Len() + len(marker) + len(tail))
	b.Write(sb.head.Bytes())
	b.WriteString(marker)
	b.Write(tail)
	return b.String()
}

// Len returns the length String() would return — the retained (possibly
// truncated) length, not the total number of bytes ever written — computed
// arithmetically from head.Len(), tailLen, and total so it stays O(1)
// instead of building the (up to head+tail-sized) string just to measure
// it. The one existing caller (RunAndCapture) only compares this against
// zero, a comparison this definition preserves exactly.
func (sb *SyncBuffer) Len() int {
	sb.mu.RLock()
	defer sb.mu.RUnlock()

	headLen := int64(sb.head.Len())
	tailLen := int64(sb.tailLen)
	dropped := sb.total - headLen - tailLen
	if dropped <= 0 {
		// No truncation: String() reassembles the full stream.
		return int(sb.total)
	}
	return sb.head.Len() + len(droppedMarker(dropped)) + sb.tailLen
}

type BackgroundShell struct {
	ID          string
	Command     string
	Description string
	Shell       *Shell
	WorkingDir  string
	ctx         context.Context
	cancel      context.CancelFunc
	stdout      *SyncBuffer
	stderr      *SyncBuffer
	done        chan struct{}
	exitErr     error
	completedAt atomic.Int64
}

type BackgroundShellManager struct {
	mu           sync.RWMutex
	shells       map[string]*BackgroundShell
	idCounter    uint64
	shuttingDown bool
	shutdownOnce sync.Once
	shutdownDone chan struct{}
	startHook    func()
}

func NewBackgroundShellManager() *BackgroundShellManager {
	return &BackgroundShellManager{shells: make(map[string]*BackgroundShell)}
}

func (m *BackgroundShellManager) Start(ctx context.Context, workingDir string, blockFuncs []BlockFunc, command string, description string) (*BackgroundShell, error) {
	shellCtx, cancel := context.WithCancel(ctx)
	bgShell := &BackgroundShell{
		Command: command, Description: description, WorkingDir: workingDir,
		Shell: NewShell(&Options{WorkingDir: workingDir, BlockFuncs: blockFuncs}),
		ctx:   shellCtx, cancel: cancel, stdout: &SyncBuffer{}, stderr: &SyncBuffer{}, done: make(chan struct{}),
	}

	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("background shell manager is shut down")
	}
	if len(m.shells) >= MaxBackgroundJobs {
		m.removeCompletedLocked()
		if len(m.shells) >= MaxBackgroundJobs {
			m.mu.Unlock()
			cancel()
			return nil, fmt.Errorf("maximum number of background jobs (%d) reached. Please terminate or wait for some jobs to complete", MaxBackgroundJobs)
		}
	}
	if m.startHook != nil {
		m.startHook()
	}
	m.idCounter++
	bgShell.ID = fmt.Sprintf("%03X", m.idCounter)
	m.shells[bgShell.ID] = bgShell
	m.mu.Unlock()

	go func() {
		defer close(bgShell.done)
		bgShell.exitErr = bgShell.Shell.ExecStream(shellCtx, command, bgShell.stdout, bgShell.stderr)
		bgShell.completedAt.Store(time.Now().Unix())
	}()
	return bgShell, nil
}

func (m *BackgroundShellManager) Get(id string) (*BackgroundShell, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	shell, ok := m.shells[id]
	return shell, ok
}

func (m *BackgroundShellManager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.shells[id]; !ok {
		return fmt.Errorf("background shell not found: %s", id)
	}
	delete(m.shells, id)
	return nil
}

func (m *BackgroundShellManager) Kill(id string) error {
	m.mu.Lock()
	shell, ok := m.shells[id]
	if ok {
		delete(m.shells, id)
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("background shell not found: %s", id)
	}
	shell.cancel()
	<-shell.done
	return nil
}

type BackgroundShellInfo struct {
	ID          string
	Command     string
	Description string
}

func (m *BackgroundShellManager) List() []string {
	m.mu.RLock()
	ids := make([]string, 0, len(m.shells))
	for id := range m.shells {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	sort.Strings(ids)
	return ids
}

type BackgroundJobCounts struct{ Active, Completed int }

func (m *BackgroundShellManager) Counts() BackgroundJobCounts {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var counts BackgroundJobCounts
	for _, shell := range m.shells {
		if shell.IsDone() {
			counts.Completed++
		} else {
			counts.Active++
		}
	}
	return counts
}

func (m *BackgroundShellManager) Cleanup() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().Unix()
	retention := int64(CompletedJobRetentionMinutes * 60)
	removed := 0
	for id, shell := range m.shells {
		if completedAt := shell.completedAt.Load(); completedAt > 0 && now-completedAt > retention {
			delete(m.shells, id)
			removed++
		}
	}
	return removed
}

func (m *BackgroundShellManager) removeCompletedLocked() {
	for id, shell := range m.shells {
		if shell.IsDone() {
			delete(m.shells, id)
		}
	}
}

func (m *BackgroundShellManager) Shutdown(ctx context.Context) error {
	m.shutdownOnce.Do(func() {
		m.mu.Lock()
		m.shuttingDown = true
		shells := make([]*BackgroundShell, 0, len(m.shells))
		for _, shell := range m.shells {
			shells = append(shells, shell)
		}
		m.shells = make(map[string]*BackgroundShell)
		m.shutdownDone = make(chan struct{})
		m.mu.Unlock()
		for _, shell := range shells {
			shell.cancel()
		}
		go func() {
			var wg sync.WaitGroup
			for _, shell := range shells {
				wg.Go(func() { <-shell.done })
			}
			wg.Wait()
			close(m.shutdownDone)
		}()
	})
	m.mu.RLock()
	done := m.shutdownDone
	m.mu.RUnlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (bs *BackgroundShell) GetOutput() (stdout string, stderr string, done bool, err error) {
	select {
	case <-bs.done:
		return bs.stdout.String(), bs.stderr.String(), true, bs.exitErr
	default:
		return bs.stdout.String(), bs.stderr.String(), false, nil
	}
}

// Done returns a channel that is closed exactly once the shell's command has
// finished (successfully, with an error, or via cancellation) and its exit
// state (exitErr, completedAt) has been fully recorded. Callers can select on
// it instead of polling GetOutput.
func (bs *BackgroundShell) Done() <-chan struct{} { return bs.done }

func (bs *BackgroundShell) IsDone() bool {
	select {
	case <-bs.done:
		return true
	default:
		return false
	}
}
func (bs *BackgroundShell) Wait() { <-bs.done }
func (bs *BackgroundShell) WaitContext(ctx context.Context) bool {
	select {
	case <-bs.done:
		return true
	case <-ctx.Done():
		return false
	}
}
