package agent

import (
	"context"
	"sync"

	"golang.org/x/sync/errgroup"
)

type readinessLifecycle struct {
	once   sync.Once
	ctx    context.Context
	cancel context.CancelFunc

	primary errgroup.Group
	mu      sync.Mutex
	closing bool
	group   sync.WaitGroup

	closeOnce sync.Once
	closeDone chan struct{}
}

func (l *readinessLifecycle) context() context.Context {
	l.once.Do(func() {
		l.ctx, l.cancel = context.WithCancel(context.Background())
	})
	return l.ctx
}

func (l *readinessLifecycle) launch(group *errgroup.Group, work func(context.Context) error) bool {
	ctx := l.context()
	l.mu.Lock()
	if l.closing {
		l.mu.Unlock()
		return false
	}
	l.group.Add(1)
	l.mu.Unlock()
	group.Go(func() error {
		defer l.group.Done()
		return work(ctx)
	})
	return true
}

func (l *readinessLifecycle) waitPrimary() error {
	return l.primary.Wait()
}

func (l *readinessLifecycle) close(ctx context.Context) error {
	l.context()
	l.closeOnce.Do(func() {
		l.closeDone = make(chan struct{})
		l.mu.Lock()
		l.closing = true
		l.mu.Unlock()
		l.cancel()
		go func() {
			l.group.Wait()
			close(l.closeDone)
		}()
	})
	select {
	case <-l.closeDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
