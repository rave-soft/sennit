package log

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEarlyHandlerBuffersAndReplaysRecords(t *testing.T) {
	early := NewEarlyHandler()
	require.NoError(t, early.Handle(context.Background(), slog.NewRecord(time.Unix(0, 0), slog.LevelWarn, "early warning", 0)))

	var output bytes.Buffer
	early.Replay(slog.New(slog.NewTextHandler(&output, nil)))
	require.Contains(t, output.String(), "early warning")

	output.Reset()
	early.Replay(slog.New(slog.NewTextHandler(&output, nil)))
	require.Empty(t, output.String())
}

func TestEarlyHandlerReplaysNestedAttrsAndGroups(t *testing.T) {
	early := NewEarlyHandler()
	logger := slog.New(early).
		With("request", "startup").
		WithGroup("config").
		With("source", "user").
		WithGroup("validation").
		With("field", "model")
	logger.Warn("invalid setting", "reason", "unknown")

	var output bytes.Buffer
	early.Replay(slog.New(slog.NewTextHandler(&output, nil)))
	line := output.String()
	require.Contains(t, line, "request=startup")
	require.Contains(t, line, "config.source=user")
	require.Contains(t, line, "config.validation.field=model")
	require.Contains(t, line, "config.validation.reason=unknown")
}

func TestEarlyHandlerReplayHonorsFinalHandlerLevel(t *testing.T) {
	early := NewEarlyHandler()
	logger := slog.New(early)
	logger.Debug("discarded debug")
	logger.Info("replayed info")
	logger.Warn("replayed warning")

	handler := &levelFilteringHandler{}
	early.Replay(slog.New(handler))

	require.Equal(t, []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn}, handler.enabledLevels)
	require.Equal(t, []string{"replayed info", "replayed warning"}, handler.messages)
}

func TestEarlyHandlerForwardingRechecksFinalHandlerLevel(t *testing.T) {
	early := NewEarlyHandler()
	ctx := context.Background()

	// slog.Logger may call Enabled before Replay installs the final handler and
	// invoke Handle afterwards. Rechecking the final handler in Handle closes
	// that boundary without relying on slog.Logger to call Enabled again.
	require.True(t, early.Enabled(ctx, slog.LevelDebug))

	handler := &levelFilteringHandler{}
	early.Replay(slog.New(handler))
	require.NoError(t, early.Handle(ctx, slog.NewRecord(time.Unix(0, 0), slog.LevelDebug, "discarded debug", 0)))
	require.NoError(t, early.Handle(ctx, slog.NewRecord(time.Unix(0, 0), slog.LevelInfo, "forwarded info", 0)))

	require.Equal(t, []slog.Level{slog.LevelDebug, slog.LevelInfo}, handler.enabledLevels)
	require.Equal(t, []string{"forwarded info"}, handler.messages)
}

func TestEarlyHandlerForwardsHandleArrivingDuringReplay(t *testing.T) {
	early := NewEarlyHandler()
	slog.New(early).Warn("buffered")

	handler := &blockingHandler{entered: make(chan struct{}), release: make(chan struct{})}
	replayed := make(chan struct{})
	go func() {
		early.Replay(slog.New(handler))
		close(replayed)
	}()
	<-handler.entered

	lateHandled := make(chan struct{})
	go func() {
		slog.New(early).Warn("late")
		close(lateHandled)
	}()

	select {
	case <-lateHandled:
		t.Fatal("Handle completed before replay's buffered record was released")
	case <-time.After(50 * time.Millisecond):
	}

	close(handler.release)
	select {
	case <-replayed:
	case <-time.After(time.Second):
		t.Fatal("Replay did not complete")
	}
	select {
	case <-lateHandled:
	case <-time.After(time.Second):
		t.Fatal("Handle arriving during Replay was not forwarded")
	}

	require.Equal(t, []string{"buffered", "late"}, handler.messages())
	early.Replay(slog.New(handler))
	require.Equal(t, []string{"buffered", "late"}, handler.messages())
}

// levelFilteringHandler intentionally records every Handle call. Its tests verify
// that EarlyHandler invokes Enabled itself instead of relying on Handle to filter records.
type levelFilteringHandler struct {
	enabledLevels []slog.Level
	messages      []string
}

func (h *levelFilteringHandler) Enabled(_ context.Context, level slog.Level) bool {
	h.enabledLevels = append(h.enabledLevels, level)
	return level >= slog.LevelInfo
}

func (h *levelFilteringHandler) Handle(_ context.Context, record slog.Record) error {
	h.messages = append(h.messages, record.Message)
	return nil
}

func (h *levelFilteringHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelFilteringHandler) WithGroup(string) slog.Handler      { return h }

type blockingHandler struct {
	mu       sync.Mutex
	seen     []string
	entered  chan struct{}
	release  chan struct{}
	blocking bool
}

func (h *blockingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *blockingHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	h.seen = append(h.seen, record.Message)
	block := !h.blocking
	h.blocking = true
	h.mu.Unlock()
	if block {
		close(h.entered)
		<-h.release
	}
	return nil
}

func (h *blockingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *blockingHandler) WithGroup(string) slog.Handler      { return h }

func (h *blockingHandler) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}
