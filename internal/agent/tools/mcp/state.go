package mcp

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/pubsub"
)

// parseLevel converts an MCP logging level string to a slog.Level. The
// entire MCP logging feature is deprecated per SEP-2577 but remains
// functional; servers may still send log notifications during the
// deprecation window.
func parseLevel(level string) slog.Level {
	switch level {
	case "info":
		return slog.LevelInfo
	case "notice":
		return slog.LevelInfo
	case "warning":
		return slog.LevelWarn
	default:
		return slog.LevelDebug
	}
}

// State represents the current state of an MCP client
type State int

const (
	StateDisabled State = iota
	StateStarting
	StateConnected
	StateError
	StateNeedsAuth
)

func (s State) String() string {
	switch s {
	case StateDisabled:
		return "disabled"
	case StateStarting:
		return "starting"
	case StateConnected:
		return "connected"
	case StateError:
		return "error"
	case StateNeedsAuth:
		return "needs auth"
	default:
		return "unknown"
	}
}

// EventType represents the type of MCP event
type EventType uint

const (
	EventStateChanged EventType = iota
	EventToolsListChanged
	EventPromptsListChanged
	EventResourcesListChanged
	// EventChannelMessage is published when a channel server pushes a
	// notifications/claude/channel event. ChannelMessage carries the rendered,
	// escaped <channel> element ready for injection into the session.
	EventChannelMessage
)

// Event represents an event in the MCP system
type Event struct {
	Type   EventType
	Name   string
	State  State
	Error  error
	Counts Counts
	// ChannelMessage is set only for EventChannelMessage: the fully rendered
	// and escaped <channel>...</channel> element to inject into the session.
	ChannelMessage string
}

// Counts number of available tools, prompts, etc.
type Counts struct {
	Tools     int
	Prompts   int
	Resources int
}

// ClientInfo holds information about an MCP client's state.
type ClientInfo struct {
	Name        string
	State       State
	Error       error
	Client      *ClientSession
	Counts      Counts
	ConnectedAt time.Time

	// Config is the configuration the server last successfully connected
	// with. Reconcile compares it against the live config to decide whether
	// a connected server needs a restart. It is recorded by updateState on
	// StateConnected and cleared on StateDisabled, so it never has to be
	// synced by hand.
	Config config.MCPConfig

	// PendingConfig is the configuration an in-flight initialization is
	// connecting with. It is set on StateStarting so a config change that
	// arrives mid-connect is compared against the attempt actually in
	// progress rather than the last successful one, which would leave the
	// server skipped as "starting" and never restarted for the new config.
	PendingConfig *config.MCPConfig
}

func (r *Registry) updateStateForSession(name string, owner attemptID, session *ClientSession, state State, err error, counts Counts) {
	r.publishMu.Lock()
	if !r.ownsSessionLocked(name, owner, session) {
		r.publishMu.Unlock()
		return
	}
	cleanup := r.updateStateLocked(name, state, err, nil, counts)
	r.publishMu.Unlock()
	r.runStateCleanup(name, cleanup)
}

func (r *Registry) setAuthTerminal(name string, owner attemptID, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, context.Canceled) || isOAuthInitErr(err) {
		r.updateStateFor(name, owner, StateNeedsAuth, nil)
		return
	}
	r.updateStateFor(name, owner, StateError, err)
}

// stateOpt mutates the ClientInfo a transition is about to publish. Config
// recording is opt-in: only the sites that actually own a config (the connect
// and starting paths) pass one, so the many error and count-refresh call sites
// can't accidentally clobber the recorded config by passing a zero value.
type stateOpt func(*ClientInfo)

// withConfig records the config now in effect. Used on StateConnected.
func withConfig(m config.MCPConfig) stateOpt {
	return func(i *ClientInfo) {
		i.Config = m
		i.PendingConfig = nil
	}
}

// withPending records the config an in-flight attempt is connecting with.
// Used on StateStarting.
func withPending(m config.MCPConfig) stateOpt {
	return func(i *ClientInfo) {
		mc := m
		i.PendingConfig = &mc
	}
}

// updateState updates the state of an MCP client and publishes an event.
//
// Config bookkeeping is split between the caller and the state machine:
//   - Callers that own a config opt in via withConfig (StateConnected) or
//     withPending (StateStarting). Everyone else leaves the recorded config
//     untouched, so an error or count refresh can't wipe it.
//   - The state machine owns the transitions with fixed semantics:
//     StateDisabled clears both so a later re-enable with an unchanged config
//     is seen as new rather than skipped as already initialized.
//
// updateStateFor publishes a transition only while its attempt still owns the
// server. Callers that do not belong to an asynchronous attempt use
// updateState; lifecycle paths bump generation before publishing instead.
func (r *Registry) updateStateFor(name string, owner attemptID, state State, err error, opts ...stateOpt) {
	r.publishMu.Lock()
	if !r.ownsLocked(name, owner) {
		r.publishMu.Unlock()
		return
	}
	cleanup := r.updateStateLocked(name, state, err, nil, Counts{}, opts...)
	r.publishMu.Unlock()
	r.runStateCleanup(name, cleanup)
}

func (r *Registry) updateState(name string, state State, err error, client *ClientSession, counts Counts, opts ...stateOpt) {
	r.publishMu.Lock()
	cleanup := r.updateStateLocked(name, state, err, client, counts, opts...)
	r.publishMu.Unlock()
	r.runStateCleanup(name, cleanup)
}

type stateCleanup struct {
	session *ClientSession
}

func (r *Registry) runStateCleanup(name string, cleanup stateCleanup) {
	if cleanup.session != nil {
		r.closeSession(name, cleanup.session)
	}
}

func (r *Registry) updateStateLocked(name string, state State, err error, client *ClientSession, counts Counts, opts ...stateOpt) stateCleanup {
	var cleanup stateCleanup
	prev, _ := r.states.Get(name)
	info := prev
	info.Name = name
	info.State = state
	info.Error = err
	info.Client = client
	info.Counts = counts
	for _, opt := range opts {
		opt(&info)
	}
	switch state {
	case StateConnected:
		info.ConnectedAt = time.Now()
	case StateDisabled:
		info.Config = config.MCPConfig{}
		info.PendingConfig = nil
	case StateError:
		// StateError is terminal for the publishing attempt. Detach its side-
		// effect ownership together with the session and catalogs so late work
		// cannot republish after the transition.
		delete(r.owners, name)
		// A session that has errored is dead to us. Atomically remove it and
		// close it so the child process and its stdio pipes are released — the
		// bare map delete this used to do leaked both. Clearing the tool
		// registry keeps the agent from advertising tools it can no longer
		// call: without it, sennit_info / the `/mcp` menu and the tool list
		// handed to the LLM diverge, so a server still reads "connected, N
		// tools" while every call fails with "tool not found".
		if old, ok := r.sessions.Take(name); ok {
			delete(r.sessionOwners, name)
			cleanup.session = old
		}
		// Drop every registry entry for the dead server. Leaving prompts or
		// resources behind lets a disconnected server keep advertising
		// capabilities the agent can no longer fulfil, the same divergence the
		// tool clear prevents.
		r.clearCatalog(name)
	}
	r.states.Set(name, info)

	// Publish state change event
	r.broker.Publish(pubsub.UpdatedEvent, Event{
		Type:   EventStateChanged,
		Name:   name,
		State:  state,
		Error:  err,
		Counts: counts,
	})
	return cleanup
}

// clearMCPData removes a stale MCP server's tools, prompts,
// resources, and auth handlers from global state so they are not
// served to the agent.
func (r *Registry) clearCatalog(name string) {
	r.catalogMu.Lock()
	r.allTools.Del(name)
	r.allPrompts.Del(name)
	r.allResources.Del(name)
	r.catalogChanged()
	r.catalogMu.Unlock()
}
