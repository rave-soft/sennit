package shell

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	MaxBackgroundJobs            = 50
	CompletedJobRetentionMinutes = 8 * 60
)

type syncBuffer struct {
	buf bytes.Buffer
	mu  sync.RWMutex
}

func (sb *syncBuffer) Write(p []byte) (n int, err error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.Write(p)
}

func (sb *syncBuffer) WriteString(s string) (n int, err error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.WriteString(s)
}

func (sb *syncBuffer) String() string {
	sb.mu.RLock()
	defer sb.mu.RUnlock()
	return sb.buf.String()
}

type BackgroundShell struct {
	ID          string
	Command     string
	Description string
	Shell       *Shell
	WorkingDir  string
	ctx         context.Context
	cancel      context.CancelFunc
	stdout      *syncBuffer
	stderr      *syncBuffer
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
		ctx:   shellCtx, cancel: cancel, stdout: &syncBuffer{}, stderr: &syncBuffer{}, done: make(chan struct{}),
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
