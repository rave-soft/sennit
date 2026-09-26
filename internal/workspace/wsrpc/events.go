package wsrpc

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/workspace"
)

// Envelope is an event as it crosses the wire (or, until PR 1.2 wires up a
// real transport, the loopback codec): Type names an entry in
// eventsByName, Payload is that entry's pubsub.Event[T] as JSON. Field
// names are snake_case to match every other wire type in this package.
type Envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// eventEntry is one row of the event registry: the wire name for a
// pubsub.Event[T], its reflect.Type (for EncodeEvent's reverse lookup by
// value), and a closure that unmarshals a payload back into that T.
type eventEntry struct {
	name   string
	typ    reflect.Type
	decode func(json.RawMessage) (any, error)
}

// eventsByType and eventsByName are built once, from the same list of
// registerEvent calls in newEventRegistry, so a type present in one is
// always present in the other.
var (
	eventsByType = map[reflect.Type]eventEntry{}
	eventsByName = map[string]eventEntry{}
)

// registerEvent adds pubsub.Event[T] to the registry under name. Called
// only from newEventRegistry below, at package init.
func registerEvent[T any](name string) {
	typ := reflect.TypeFor[pubsub.Event[T]]()
	entry := eventEntry{
		name: name,
		typ:  typ,
		decode: func(payload json.RawMessage) (any, error) {
			var v pubsub.Event[T]
			if err := json.Unmarshal(payload, &v); err != nil {
				return nil, fmt.Errorf("wsrpc: decoding event %q: %w", name, err)
			}
			return v, nil
		},
	}
	if _, dup := eventsByName[name]; dup {
		panic(fmt.Sprintf("wsrpc: duplicate event name %q", name))
	}
	if _, dup := eventsByType[typ]; dup {
		panic(fmt.Sprintf("wsrpc: duplicate event type %s", typ))
	}
	eventsByName[name] = entry
	eventsByType[typ] = entry
}

// init builds the event registry: the complete list of message types
// Workspace.Subscribe/SubscribeWith can deliver to a frontend, taken from
// the source that actually produces them --
// appws.AppWorkspace.Subscribe/translateEvent (which folds every
// app-internal event shape into one of these, or into nil when it has no
// frontend consumer -- see translateEvent's own doc comment) and
// appws.AppWorkspace.SubscribeWith (same translateEvent, a second
// subscription). Anything translateEvent can return un-nil must have an
// entry here, or Loopback's Subscribe/SubscribeWith wrapper (see
// loopback_manual.go) panics naming the type -- see
// TestTranslateEventOutputsAreRegistered in
// internal/workspace/appws for the coverage check that keeps the two in
// sync.
func init() {
	registerEvent[message.Message]("message")
	registerEvent[session.Session]("session")
	registerEvent[proto.Thread]("thread")
	registerEvent[permission.PermissionRequest]("permission_request")
	registerEvent[permission.PermissionNotification]("permission_notification")
	registerEvent[question.Request]("question_request")
	registerEvent[question.Notification]("question_notification")
	registerEvent[history.File]("history_file")
	registerEvent[skills.Event]("skills")
	registerEvent[workspace.AgentNotification]("agent_notification")
	registerEvent[workspace.LSPEvent]("lsp")
	registerEvent[workspace.MCPEvent]("mcp")
	// workspace.ConnectionEvent has no in-process producer -- see its own
	// doc comment -- but is registered so the DTO gate and Loopback cover
	// it and grpcws's client can build one straight from this registry
	// (CLIENT-SERVER.md, PR 1.2 build step 4).
	registerEvent[workspace.ConnectionEvent]("connection")
	// workspace.ClientState is likewise produced only by grpcws (its
	// per-hub state publisher, not AppWorkspace/translateEvent) -- see
	// BuildClientState and grpcws's eventHub.maybePublishState
	// (CLIENT-SERVER.md, PR 1.4a).
	registerEvent[workspace.ClientState]("client_state")
}

// EncodeEvent marshals a pubsub.Event[T] value delivered to a
// Workspace.Subscribe/SubscribeWith callback into its wire Envelope. err
// names the concrete Go type when v is not in the registry.
func EncodeEvent(v any) (Envelope, error) {
	entry, ok := eventsByType[reflect.TypeOf(v)]
	if !ok {
		return Envelope{}, fmt.Errorf("wsrpc: no event registered for type %T", v)
	}
	payload, err := json.Marshal(v)
	if err != nil {
		return Envelope{}, fmt.Errorf("wsrpc: marshaling event %q: %w", entry.name, err)
	}
	return Envelope{Type: entry.name, Payload: payload}, nil
}

// DecodeEvent reverses EncodeEvent. err names env.Type when it is not in
// the registry.
func DecodeEvent(env Envelope) (any, error) {
	entry, ok := eventsByName[env.Type]
	if !ok {
		return nil, fmt.Errorf("wsrpc: no event registered for name %q", env.Type)
	}
	return entry.decode(env.Payload)
}
