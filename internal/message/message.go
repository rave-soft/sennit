package message

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/pubsub"
)

// defaultUpdateDebounce is the default debounce window for [Service.Update].
// Streaming deltas that arrive within the window are coalesced into a
// single SQL write and a single pubsub event. Terminal updates
// (finish/error/cancel/tool-call structural changes) bypass the
// debounce and flush synchronously.
const defaultUpdateDebounce = 33 * time.Millisecond

type CreateMessageParams struct {
	Role             MessageRole
	Parts            []ContentPart
	Model            string
	Provider         string
	IsSummaryMessage bool
	Origin           Origin
}

// Service is the public interface to the message store.
//
// [Service.Update] is eventually consistent: it accepts new state into
// an in-memory buffer and writes it to SQLite plus publishes a
// [pubsub.UpdatedEvent] on the next debounce tick (default
// [defaultUpdateDebounce]) or on the next terminal-state update,
// whichever comes first. Terminal-state updates — those that finish
// the message, add or finish a tool call, or end a reasoning section —
// flush synchronously before [Service.Update] returns.
//
// Callers that need stronger ordering (e.g. tests, shutdown,
// session-switch reads) must use [Service.Flush] or [Service.FlushAll]
// before reading via [Service.Get] / [Service.List]. Without an
// explicit flush, a read can race the debounce timer and miss the
// most recent in-memory state.
type Service interface {
	pubsub.Subscriber[Message]
	Create(ctx context.Context, sessionID string, params CreateMessageParams) (Message, error)
	Update(ctx context.Context, message Message) error
	Get(ctx context.Context, id string) (Message, error)
	List(ctx context.Context, sessionID string) ([]Message, error)
	ListBySessionIDs(ctx context.Context, sessionIDs []string) (map[string][]Message, error)
	ListUserMessages(ctx context.Context, sessionID string) ([]Message, error)
	ListAllUserMessages(ctx context.Context) ([]Message, error)
	// ListUnfinishedAssistantMessages returns the assistant messages in
	// projectPath that never recorded a Finish, oldest first. Every path
	// that ends a turn writes one, and every such path runs inside the
	// process that owns the turn, so what this returns are the turns a
	// previous process was killed in the middle of. See
	// app.finalizeInterruptedTurns, its only caller.
	ListUnfinishedAssistantMessages(ctx context.Context, projectPath string) ([]Message, error)
	Delete(ctx context.Context, id string) error
	DeleteSessionMessages(ctx context.Context, sessionID string) error

	// Flush synchronously drains any pending debounced state for the
	// given message ID, performs the SQL write, and publishes the
	// resulting [pubsub.UpdatedEvent]. Idempotent; cheap no-op if no
	// updates are pending. Use this before any read that must observe
	// the latest [Service.Update].
	Flush(ctx context.Context, id string) error

	// FlushAll synchronously drains pending debounced state for every
	// message known to the service. Intended for shutdown and
	// session-switch paths.
	FlushAll(ctx context.Context) error
}

// pendingState holds the in-memory coalescing buffer for a single
// message ID. All fields except where noted are guarded by
// service.mu. The flushing flag serializes concurrent flushers for
// the same ID so SQL writes never reorder.
type pendingState struct {
	// latest is the most recent [Message] passed to [Service.Update]
	// that has not yet been flushed.
	latest Message

	// generation identifies the Update that supplied latest. It lets a
	// completed flush distinguish its snapshot from a concurrent update
	// without comparing Message values, whose Parts may contain slices.
	generation uint64

	// dirty is true when latest contains state that has not been
	// written to SQL since the last successful flush.
	dirty bool

	// flushing is true while a goroutine is performing the SQL write
	// for this ID. New updates are still accepted (and re-mark dirty)
	// but other flushers must back off.
	flushing bool

	// timer is the active debounce timer, or nil if no flush is
	// scheduled. Stopped and reset when a terminal update preempts
	// the debounce window.
	timer *time.Timer

	// lastFlushed is the snapshot most recently written to SQL. Used
	// as the baseline for terminal-state detection.
	lastFlushed Message

	// hasFlushed is false until the first successful write for this
	// ID; until then lastFlushed is the zero value and must not be
	// treated as a real prior state.
	hasFlushed bool

	// flushDone is created fresh each time flushing flips true and
	// closed when that flush attempt ends (success or error). A
	// waiter that finds flushing == true grabs a reference to this
	// channel while holding service.mu, releases the lock, and blocks
	// on it instead of polling — see flushOne. Nil whenever flushing
	// is false.
	flushDone chan struct{}
}

type service struct {
	*pubsub.Broker[Message]
	q        db.Querier
	debounce time.Duration

	mu      sync.Mutex
	pending map[string]*pendingState
}

// ServiceOption configures a [Service] at construction.
type ServiceOption func(*service)

// WithDebounce overrides the debounce window for [Service.Update]. A
// zero or negative value disables debouncing entirely (every update
// flushes synchronously). Intended primarily for tests.
func WithDebounce(d time.Duration) ServiceOption {
	return func(s *service) {
		s.debounce = d
	}
}

func NewService(q db.Querier, opts ...ServiceOption) Service {
	s := &service{
		Broker:   pubsub.NewBroker[Message](),
		q:        q,
		debounce: defaultUpdateDebounce,
		pending:  make(map[string]*pendingState),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *service) Delete(ctx context.Context, id string) error {
	message, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	err = s.q.DeleteMessage(ctx, message.ID)
	if err != nil {
		return err
	}
	// Drop any pending coalesced state for this ID. We never want to
	// flush back over a deleted row.
	s.mu.Lock()
	if p, ok := s.pending[id]; ok {
		if p.timer != nil {
			p.timer.Stop()
		}
		delete(s.pending, id)
	}
	s.mu.Unlock()
	// Clone the message before publishing to avoid race conditions with
	// concurrent modifications to the Parts slice.
	s.Publish(pubsub.DeletedEvent, message.Clone())
	return nil
}

func (s *service) Create(ctx context.Context, sessionID string, params CreateMessageParams) (Message, error) {
	if params.Role != Assistant {
		params.Parts = append(params.Parts, Finish{
			Reason: "stop",
		})
	}
	partsJSON, err := MarshalParts(params.Parts)
	if err != nil {
		return Message{}, err
	}
	isSummary := int64(0)
	if params.IsSummaryMessage {
		isSummary = 1
	}
	origin := params.Origin
	if origin == "" {
		origin = OriginPerson
	}
	dbMessage, err := s.q.CreateMessage(ctx, db.CreateMessageParams{
		ID:               uuid.New().String(),
		SessionID:        sessionID,
		Role:             string(params.Role),
		Parts:            string(partsJSON),
		Model:            sql.NullString{String: params.Model, Valid: true},
		Provider:         sql.NullString{String: params.Provider, Valid: params.Provider != ""},
		IsSummaryMessage: isSummary,
		Origin:           string(origin),
	})
	if err != nil {
		return Message{}, err
	}
	message, err := s.fromDBItem(dbMessage)
	if err != nil {
		return Message{}, err
	}
	// Clone the message before publishing to avoid race conditions with
	// concurrent modifications to the Parts slice.
	s.Publish(pubsub.CreatedEvent, message.Clone())
	return message, nil
}

func (s *service) DeleteSessionMessages(ctx context.Context, sessionID string) error {
	messages, err := s.List(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, message := range messages {
		if message.SessionID == sessionID {
			err = s.Delete(ctx, message.ID)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Update accepts a new state for a message and either flushes
// synchronously (terminal updates, debounce <= 0) or buffers it until
// the next debounce tick. See [Service] for the contract.
func (s *service) Update(ctx context.Context, msg Message) error {
	cloned := msg.Clone()

	// Zero or negative debounce: flush every update synchronously. This
	// preserves the pre-coalescing behaviour for tests and any caller
	// that explicitly opted out via [WithDebounce].
	if s.debounce <= 0 {
		s.mu.Lock()
		p, ok := s.pending[msg.ID]
		if !ok {
			p = &pendingState{}
			s.pending[msg.ID] = p
		}
		p.latest = cloned
		p.generation++
		p.dirty = true
		s.mu.Unlock()
		return s.flushOne(ctx, msg.ID, true)
	}

	s.mu.Lock()
	p, ok := s.pending[msg.ID]
	if !ok {
		p = &pendingState{}
		s.pending[msg.ID] = p
	}
	p.latest = cloned
	p.generation++
	p.dirty = true

	var prev *Message
	if p.hasFlushed {
		prev = &p.lastFlushed
	}
	terminal := shouldFlushNow(prev, &cloned)

	if terminal {
		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
		s.mu.Unlock()
		return s.flushOne(ctx, msg.ID, true)
	}

	// Debounce: schedule a single flush per pending state. If a flush
	// is already running we let it finish; the trailing dirty bit will
	// be picked up by the next Update or by Flush.
	if p.timer == nil && !p.flushing {
		id := msg.ID
		p.timer = time.AfterFunc(s.debounce, func() {
			// Detached from caller ctx so a cancelled stream context
			// does not thread the buffered write.
			_ = s.flushOne(context.Background(), id, false)
		})
	}
	s.mu.Unlock()
	return nil
}

// Flush implements [Service.Flush].
func (s *service) Flush(ctx context.Context, id string) error {
	return s.flushOne(ctx, id, true)
}

// FlushAll implements [Service.FlushAll]. It snapshots every ID with
// outstanding work — either dirty buffered state or a flush already in
// flight — then drains each one. Picking up in-flight IDs ensures
// FlushAll cannot return while a timer-fired write is still mid-SQL,
// which is what shutdown and session-switch callers rely on.
func (s *service) FlushAll(ctx context.Context) error {
	s.mu.Lock()
	ids := make([]string, 0, len(s.pending))
	for id, p := range s.pending {
		if p.dirty || p.flushing {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	var firstErr error
	for _, id := range ids {
		if err := s.flushOne(ctx, id, true); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// flushOne drains a single message ID. When syncCaller is true the
// caller is willing to wait through a concurrent in-flight flush so
// that, on return, lastFlushed equals latest at the moment of return.
// When false (timer-fired path) we bail if another flusher is already
// running; that flusher will pick up the trailing dirty bit.
//
// Order matters: a sync caller must wait for any in-flight flush to
// drain even when the buffer is currently clean — that in-flight
// write has not yet updated the SQL row, so returning early would
// violate the contract that on success lastFlushed reflects the most
// recent state.
func (s *service) flushOne(ctx context.Context, id string, syncCaller bool) error {
	for {
		s.mu.Lock()
		p, ok := s.pending[id]
		if !ok {
			s.mu.Unlock()
			return nil
		}
		if p.flushing {
			if !syncCaller {
				s.mu.Unlock()
				return nil
			}
			// Grab a reference to this flush attempt's completion
			// signal while still holding the lock, then wait on it
			// instead of polling. flushOne closes it exactly once,
			// right after clearing p.flushing below, so this can
			// never miss a signal or double-close.
			waitCh := p.flushDone
			s.mu.Unlock()
			select {
			case <-waitCh:
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		if !p.dirty {
			s.mu.Unlock()
			return nil
		}

		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
		snap := p.latest
		generation := p.generation
		// Decide whether this snapshot represents a terminal event
		// against the prior baseline. We must do this before resetting
		// dirty/flushing because shouldFlushNow looks at p.lastFlushed
		// (which is what was on disk before this write).
		var prev *Message
		if p.hasFlushed {
			prev = &p.lastFlushed
		}
		isTerminal := shouldFlushNow(prev, &snap)
		p.flushing = true
		p.dirty = false
		done := make(chan struct{})
		p.flushDone = done
		s.mu.Unlock()

		err := s.write(ctx, snap)

		s.mu.Lock()
		p.flushing = false
		// Wake any waiters queued behind this attempt, then clear the
		// field so the next flush attempt (if any) creates its own.
		close(done)
		p.flushDone = nil
		if err == nil {
			p.lastFlushed = snap
			p.hasFlushed = true
		} else {
			// Restore dirty so the next caller retries.
			p.dirty = true
		}
		// A delta that arrived during the SQL write must not be left
		// stranded: an Update while flushing intentionally does not
		// schedule another timer. Sync callers drain it inline (the
		// caller expects that delta to land too); the timer-fired path
		// re-arms the debounce timer below instead — looping here would
		// collapse the debounce into back-to-back writes for as long as
		// the stream outpaces SQL write latency.
		wasDirty := p.dirty
		if err == nil && snap.IsFinished() && s.pending[id] == p &&
			!p.flushing && !p.dirty && p.generation == generation {
			// A finished message needs no coalescing state after its final
			// snapshot has landed. Stop defensively even though the flush
			// normally cleared the timer before issuing SQL.
			if p.timer != nil {
				p.timer.Stop()
				p.timer = nil
			}
			delete(s.pending, id)
		}
		s.mu.Unlock()

		if err != nil {
			// dirty was restored above so the next flush retries — but
			// for a message whose stream has already ended there is no
			// next flush to piggyback on, and the delta sat unwritten
			// until something unrelated touched the message. Give the
			// retry a timer of its own.
			s.rearmFlushTimer(id, p)
			return err
		}

		// Terminal events — message finished, tool call added or
		// finished, reasoning ended — use the bounded must-deliver
		// path so they never get dropped under channel contention.
		if isTerminal {
			s.PublishMustDeliver(ctx, pubsub.UpdatedEvent, snap)
		} else {
			s.Publish(pubsub.UpdatedEvent, snap)
		}

		if wasDirty && syncCaller {
			continue
		}
		if wasDirty {
			// Timer-fired path: give the leftover dirty state a fresh
			// debounce window instead of rewriting immediately.
			s.rearmFlushTimer(id, p)
		}
		return nil
	}
}

// rearmFlushTimer gives id's pending state a fresh debounce window, if it
// is still the same state, still dirty, and has no timer of its own. Used
// both after a flush that left a delta behind and after one that failed —
// in either case something has to come back for the write, and an Update
// that happens to arrive later is not something to count on.
func (s *service) rearmFlushTimer(id string, p *pendingState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.pending[id]; ok && cur == p && cur.dirty && !cur.flushing && cur.timer == nil {
		cur.timer = time.AfterFunc(s.debounce, func() {
			_ = s.flushOne(context.Background(), id, false)
		})
	}
}

// write performs the unguarded SQL write + UpdatedAt stamp. Caller
// owns publishing.
func (s *service) write(ctx context.Context, msg Message) error {
	parts, err := MarshalParts(msg.Parts)
	if err != nil {
		return err
	}
	finishedAt := sql.NullInt64{}
	if f := msg.FinishPart(); f != nil {
		finishedAt.Int64 = f.Time
		finishedAt.Valid = true
	}
	if err := s.q.UpdateMessage(ctx, db.UpdateMessageParams{
		ID:         msg.ID,
		Parts:      string(parts),
		FinishedAt: finishedAt,
	}); err != nil {
		return err
	}
	return nil
}

// shouldFlushNow returns true when next represents a structural
// change that must not be silently coalesced: the message just
// finished, the tool-call set grew, a tool call transitioned to
// finished, or reasoning just finished. prev is the last-flushed
// snapshot (or nil if no write has landed yet).
func shouldFlushNow(prev, next *Message) bool {
	if next.IsFinished() {
		return true
	}

	var prevCalls []ToolCall
	var prevReasoningFinishedAt int64
	if prev != nil {
		prevCalls = prev.ToolCalls()
		prevReasoningFinishedAt = prev.ReasoningContent().FinishedAt
	}
	nextCalls := next.ToolCalls()
	if len(nextCalls) != len(prevCalls) {
		return true
	}
	for i := range nextCalls {
		// Bounds-safe: lengths are equal here.
		if nextCalls[i].Finished != prevCalls[i].Finished {
			return true
		}
		// A tool call's input only matters once it has landed (Finished
		// flips true). Earlier deltas to Input are debounced with the
		// rest of the streaming state.
	}
	if next.ReasoningContent().FinishedAt > 0 && prevReasoningFinishedAt == 0 {
		return true
	}
	return false
}

func (s *service) Get(ctx context.Context, id string) (Message, error) {
	dbMessage, err := s.q.GetMessage(ctx, id)
	if err != nil {
		return Message{}, err
	}
	return s.fromDBItem(dbMessage)
}

func (s *service) List(ctx context.Context, sessionID string) ([]Message, error) {
	dbMessages, err := s.q.ListMessagesBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	messages := make([]Message, len(dbMessages))
	for i, dbMessage := range dbMessages {
		messages[i], err = s.fromDBItem(dbMessage)
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func (s *service) ListUnfinishedAssistantMessages(ctx context.Context, projectPath string) ([]Message, error) {
	rows, err := s.q.ListUnfinishedAssistantMessages(ctx, projectPath)
	if err != nil {
		return nil, err
	}
	messages := make([]Message, len(rows))
	for i, row := range rows {
		messages[i], err = s.fromDBItem(db.Message{
			ID:         row.ID,
			SessionID:  row.SessionID,
			Role:       row.Role,
			Parts:      row.Parts,
			Model:      row.Model,
			Provider:   row.Provider,
			CreatedAt:  row.CreatedAt,
			UpdatedAt:  row.UpdatedAt,
			FinishedAt: row.FinishedAt,
		})
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func (s *service) ListUserMessages(ctx context.Context, sessionID string) ([]Message, error) {
	dbMessages, err := s.q.ListUserMessagesBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	messages := make([]Message, len(dbMessages))
	for i, dbMessage := range dbMessages {
		messages[i], err = s.fromDBItem(dbMessage)
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func (s *service) ListAllUserMessages(ctx context.Context) ([]Message, error) {
	dbMessages, err := s.q.ListAllUserMessages(ctx)
	if err != nil {
		return nil, err
	}
	messages := make([]Message, len(dbMessages))
	for i, dbMessage := range dbMessages {
		messages[i], err = s.fromDBItem(dbMessage)
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func (s *service) ListBySessionIDs(ctx context.Context, sessionIDs []string) (map[string][]Message, error) {
	if len(sessionIDs) == 0 {
		return nil, nil
	}

	idsJSON, err := json.Marshal(sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("marshal session IDs: %w", err)
	}

	rows, err := s.q.ListMessagesBySessionIDs(ctx, string(idsJSON))
	if err != nil {
		return nil, err
	}

	result := make(map[string][]Message)
	for _, row := range rows {
		msg, err := s.fromDBItem(row)
		if err != nil {
			slog.Warn("Failed to list batch messages", "message_id", row.ID, "error", err)
			continue
		}
		result[msg.SessionID] = append(result[msg.SessionID], msg)
	}
	return result, nil
}

func (s *service) fromDBItem(item db.Message) (Message, error) {
	parts, err := UnmarshalParts([]byte(item.Parts), item.ID)
	if err != nil {
		return Message{}, err
	}
	return Message{
		ID:               item.ID,
		SessionID:        item.SessionID,
		Role:             MessageRole(item.Role),
		Parts:            parts,
		Model:            item.Model.String,
		Provider:         item.Provider.String,
		CreatedAt:        item.CreatedAt,
		UpdatedAt:        item.UpdatedAt,
		IsSummaryMessage: item.IsSummaryMessage != 0,
		Origin:           Origin(item.Origin),
	}, nil
}

type partType string

const (
	// metaType marks the synthetic version-marker element that
	// MarshalParts always prepends. It is never surfaced as a
	// [ContentPart] to callers.
	metaType         partType = "_meta"
	reasoningType    partType = "reasoning"
	textType         partType = "text"
	imageURLType     partType = "image_url"
	binaryType       partType = "binary"
	toolCallType     partType = "tool_call"
	toolResultType   partType = "tool_result"
	finishType       partType = "finish"
	shellCommandType partType = "shell_command"
)

// partsFormatVersion identifies the shape of the parts blob stored in
// SQLite. It is carried as a synthetic "_meta" wrapper element (see
// formatMeta) rather than a schema change, so old rows without it are
// still valid (implicitly version 0/1 — see below).
//
// Compatibility note: this is a one-time, unavoidable break. A binary
// built before this version marker existed treats ANY unrecognized
// wrapper type — including "_meta" — as fatal, so it cannot read a
// blob written by this or a later binary, exactly as it could not read
// a blob containing any other new part type. Binaries at or after this
// change tolerate unknown wrapper types (see UnmarshalParts) and so
// remain forward-compatible with each other from here on: a future new
// part type, or a future version bump gated on this field, will not
// break reads.
const partsFormatVersion = 1

// formatMeta is the payload of the synthetic "_meta" wrapper element.
// It exists only to satisfy [ContentPart] for marshaling; it never
// appears in a [Message]'s Parts slice.
type formatMeta struct {
	Version int `json:"version"`
}

func (formatMeta) isPart() {}

type partWrapper struct {
	Type partType    `json:"type"`
	Data ContentPart `json:"data"`
}

// MarshalParts marshals content parts to JSON, wrapping each in a
// type-tagged envelope plus a synthetic "_meta" version marker (see
// [partsFormatVersion]). Exported so [github.com/rave-soft/sennit/internal/proto]
// can reuse it for its own JSON encoding, which shares the exact same
// shape as the SQLite storage format.
func MarshalParts(parts []ContentPart) ([]byte, error) {
	wrappedParts := make([]partWrapper, 0, len(parts)+1)
	wrappedParts = append(wrappedParts, partWrapper{
		Type: metaType,
		Data: formatMeta{Version: partsFormatVersion},
	})

	for _, part := range parts {
		var typ partType

		switch part.(type) {
		case ReasoningContent:
			typ = reasoningType
		case TextContent:
			typ = textType
		case ImageURLContent:
			typ = imageURLType
		case BinaryContent:
			typ = binaryType
		case ToolCall:
			typ = toolCallType
		case ToolResult:
			typ = toolResultType
		case Finish:
			typ = finishType
		case ShellCommand:
			typ = shellCommandType
		default:
			return nil, fmt.Errorf("unknown part type: %T", part)
		}

		wrappedParts = append(wrappedParts, partWrapper{
			Type: typ,
			Data: part,
		})
	}
	return json.Marshal(wrappedParts)
}

// UnmarshalParts decodes a parts blob. msgID is included in warning
// logs for unrecognized part types; it is not otherwise used and may
// be empty (e.g. before the message has an ID).
//
// Unknown wrapper types are skipped rather than treated as fatal: a
// session written by a newer binary may contain a part type this
// binary doesn't know about, and failing the whole message would make
// an otherwise-readable session unreadable. A malformed envelope or a
// known type with a payload that fails to unmarshal is still a real
// error (data corruption), so those remain fatal.
func UnmarshalParts(data []byte, msgID string) ([]ContentPart, error) {
	temp := []json.RawMessage{}

	if err := json.Unmarshal(data, &temp); err != nil {
		return nil, err
	}

	parts := make([]ContentPart, 0)

	for _, rawPart := range temp {
		var wrapper struct {
			Type partType        `json:"type"`
			Data json.RawMessage `json:"data"`
		}

		if err := json.Unmarshal(rawPart, &wrapper); err != nil {
			return nil, err
		}

		switch wrapper.Type {
		case metaType:
			// Version marker; nothing to branch on yet since this is
			// the first version. Not appended to parts.
		case reasoningType:
			part := ReasoningContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case textType:
			part := TextContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case imageURLType:
			part := ImageURLContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case binaryType:
			part := BinaryContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case toolCallType:
			part := ToolCall{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case toolResultType:
			part := ToolResult{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case finishType:
			part := Finish{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case shellCommandType:
			part := ShellCommand{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		default:
			slog.Warn("Skipping unrecognized message part type",
				"type", wrapper.Type, "message_id", msgID)
		}
	}

	return parts, nil
}
