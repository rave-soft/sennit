package log

import (
	"context"
	"log/slog"
	"sync"
)

// EarlyHandler retains records emitted before Setup can create the file logger.
// It deliberately handles every record so startup diagnostics neither leak to
// stderr nor disappear before their normal destination is available.
//
// Handler instances returned by WithAttrs and WithGroup share a core but retain
// their own immutable context. This also means loggers created before Replay
// transparently forward to the final handler afterwards.
type EarlyHandler struct {
	core *earlyHandlerCore
	ops  []handlerOp
}

type earlyHandlerCore struct {
	mu      sync.Mutex
	records []capturedRecord
	final   slog.Handler
}

type capturedRecord struct {
	record slog.Record
	ops    []handlerOp
}

type handlerOp struct {
	attrs []slog.Attr
	group string
}

func NewEarlyHandler() *EarlyHandler {
	return &EarlyHandler{core: &earlyHandlerCore{}}
}

func (h *EarlyHandler) Enabled(ctx context.Context, level slog.Level) bool {
	h.core.mu.Lock()
	defer h.core.mu.Unlock()
	return h.core.final == nil || withOps(h.core.final, h.ops).Enabled(ctx, level)
}

func (h *EarlyHandler) Handle(ctx context.Context, record slog.Record) error {
	h.core.mu.Lock()
	defer h.core.mu.Unlock()

	if h.core.final == nil {
		h.core.records = append(h.core.records, capturedRecord{
			record: record.Clone(),
			ops:    cloneOps(h.ops),
		})
		return nil
	}
	handler := withOps(h.core.final, h.ops)
	if !handler.Enabled(ctx, record.Level) {
		return nil
	}
	return handler.Handle(ctx, record)
}

func (h *EarlyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	ops := cloneOps(h.ops)
	ops = append(ops, handlerOp{attrs: append([]slog.Attr(nil), attrs...)})
	return &EarlyHandler{core: h.core, ops: ops}
}

func (h *EarlyHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	ops := cloneOps(h.ops)
	ops = append(ops, handlerOp{group: name})
	return &EarlyHandler{core: h.core, ops: ops}
}

// Replay atomically changes all handler instances sharing h into forwarding
// mode, then delivers each buffered record exactly once. Handle holds the same
// lock while forwarding, so records logged at the replay boundary are either
// included in this ordered replay or forwarded immediately afterwards; none can
// be appended to a buffer that has already been drained. Repeated calls are
// safe no-ops once a final handler has been installed.
func (h *EarlyHandler) Replay(logger *slog.Logger) {
	if logger == nil {
		return
	}

	h.core.mu.Lock()
	defer h.core.mu.Unlock()
	if h.core.final != nil {
		return
	}

	h.core.final = logger.Handler()
	for _, captured := range h.core.records {
		handler := withOps(h.core.final, captured.ops)
		if handler.Enabled(context.Background(), captured.record.Level) {
			_ = handler.Handle(context.Background(), captured.record)
		}
	}
	h.core.records = nil
}

func withOps(handler slog.Handler, ops []handlerOp) slog.Handler {
	for _, op := range ops {
		if op.group != "" {
			handler = handler.WithGroup(op.group)
		} else {
			handler = handler.WithAttrs(op.attrs)
		}
	}
	return handler
}

func cloneOps(ops []handlerOp) []handlerOp {
	cloned := make([]handlerOp, len(ops))
	for i, op := range ops {
		cloned[i] = handlerOp{group: op.group, attrs: append([]slog.Attr(nil), op.attrs...)}
	}
	return cloned
}
