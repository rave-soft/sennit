package model

import (
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/workspace"
)

// sessionModelRef names the provider and model a session's own assistant
// messages were produced by. Every assistant message records both (see
// runTurn's message.CreateMessageParams), which is the only place the truth
// lives for a session nobody is currently running: a delegation may have
// been given its own model in .sennit/agents, and a session opened from
// history may predate several model switches.
//
// sessionID is which session the reading is about. Navigation into a
// delegation is asynchronous — the nav frame is pushed before the child's
// load lands, and the parent stays loaded in between — so a reading that
// carries no session is a reading that can be attributed to the wrong one
// (see recordedModel, and childDelegationBusy for the same hazard).
type sessionModelRef struct {
	sessionID string
	provider  string
	model     string
}

// String renders the reference the way delegations and the model picker
// spell one, "provider/model-id", and "" when nothing is known.
func (r sessionModelRef) String() string {
	if r.provider == "" || r.model == "" {
		return ""
	}
	return r.provider + "/" + r.model
}

// forSession returns r when it is a reading about sessionID, and an empty
// reference otherwise — see recordedModel for why asking that question is
// the whole point of the field.
func (r sessionModelRef) forSession(sessionID string) sessionModelRef {
	if r.sessionID != sessionID {
		return sessionModelRef{}
	}
	return r
}

// lastAssistantModel returns the model the most recent assistant message in
// msgs was produced by. The last one rather than the first: a session can
// span a model switch, and what answered most recently is what the reader
// is looking at.
func lastAssistantModel(sessionID string, msgs []message.Message) sessionModelRef {
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Role != message.Assistant || msg.Model == "" || msg.Provider == "" {
			continue
		}
		return sessionModelRef{sessionID: sessionID, provider: msg.Provider, model: msg.Model}
	}
	// Nothing has answered yet. The session is still named, so a later
	// reading for it can be told apart from one about another session.
	return sessionModelRef{sessionID: sessionID}
}

// recordAssistantModel notes the model an incoming message was produced by,
// so a session that starts answering while it is on screen stops being
// described by whatever the previous one used.
func (s *sessionState) recordAssistantModel(msg message.Message) {
	if msg.Role != message.Assistant || msg.Model == "" || msg.Provider == "" {
		return
	}
	s.modelUsed = sessionModelRef{sessionID: msg.SessionID, provider: msg.Provider, model: msg.Model}
}

// viewedModel is the model to describe for whatever session is on screen.
//
// For the main session that is the current selection: the sidebar is there
// to say what the next prompt will run on. A child session is a read-only
// record of a delegation that may well have run on a different model, so
// there what its own messages say wins, then the model it was pinned to,
// and the selection is only the fallback for a delegation that has
// neither.
func (m *UI) viewedModel() *workspace.AgentModel {
	if len(m.sess.navStack) > 0 {
		if model := m.sess.recordedModel(m.com, m.sess.navStack[len(m.sess.navStack)-1]); model != nil {
			return model
		}
	}
	return m.selectedModel()
}

// recordedModel names the model a delegation ran on, for the frame the UI
// is navigated into.
//
// Two things can answer, in this order:
//
//   - what the child session's own assistant messages record — the truth
//     for a delegation that inherited the app's model, and for one read
//     back from history whose model is long gone from the picker;
//   - failing that, the model the delegation was pinned to in
//     .sennit/agents, which is what it will answer on once it does.
//
// The first is accepted only while it is a reading *about this frame's
// session*. m.sess.modelUsed follows whatever session is loaded, and
// navigation is asynchronous: between pushing the frame and the child's
// load landing, the parent is still the loaded session and still
// recording its own model against every message it streams. Attributing
// that to the child is how the sidebar came to show the parent's model
// inside a delegation, and to flip between the two as messages arrived.
//
// Nothing at all is returned when neither answers — a delegation with no
// override that has yet to say a word really does run on the selection,
// and that is what the caller falls back to.
func (s *sessionState) recordedModel(com *common.Common, frame sessionNavFrame) *workspace.AgentModel {
	provider, model := "", ""
	if ref := s.modelUsed.forSession(frame.childSessionID); ref.model != "" {
		provider, model = ref.provider, ref.model
	}
	if provider == "" || model == "" {
		// The provider is the part before the FIRST slash; model ids may
		// contain slashes themselves.
		provider, model, _ = strings.Cut(frame.model, "/")
	}
	if provider == "" || model == "" {
		return nil
	}

	catalog := catwalk.Model{ID: model, Name: model}
	// A config without providers is a config that can resolve nothing, and
	// that is the shape a UI built without a workspace has.
	if cfg := com.Config(); cfg != nil && cfg.Providers != nil {
		if known := cfg.GetModel(provider, model); known != nil {
			catalog = *known
		}
	}
	// The messages record no effort, but the delegation that started this
	// session does, and it is the effort those messages were produced at.
	selected := config.SelectedModel{Provider: provider, Model: model, ReasoningEffort: frame.effort}
	return &workspace.AgentModel{CatalogCfg: catalog, ModelCfg: selected}
}
