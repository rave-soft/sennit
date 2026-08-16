package model

import (
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/workspace"
)

// sessionModelRef names the provider and model a session's own assistant
// messages were produced by. Every assistant message records both (see
// runTurn's message.CreateMessageParams), which is the only place the truth
// lives for a session nobody is currently running: a delegation may have
// been given its own model in .sennit/agents, and a session opened from
// history may predate several model switches.
type sessionModelRef struct {
	provider string
	model    string
}

// String renders the reference the way delegations and the model picker
// spell one, "provider/model-id", and "" when nothing is known.
func (r sessionModelRef) String() string {
	if r.provider == "" || r.model == "" {
		return ""
	}
	return r.provider + "/" + r.model
}

// lastAssistantModel returns the model the most recent assistant message in
// msgs was produced by. The last one rather than the first: a session can
// span a model switch, and what answered most recently is what the reader
// is looking at.
func lastAssistantModel(msgs []message.Message) sessionModelRef {
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Role != message.Assistant || msg.Model == "" || msg.Provider == "" {
			continue
		}
		return sessionModelRef{provider: msg.Provider, model: msg.Model}
	}
	return sessionModelRef{}
}

// recordAssistantModel notes the model an incoming message was produced by,
// so a session that starts answering while it is on screen stops being
// described by whatever the previous one used.
func (m *UI) recordAssistantModel(msg message.Message) {
	if msg.Role != message.Assistant || msg.Model == "" || msg.Provider == "" {
		return
	}
	m.sess.modelUsed = sessionModelRef{provider: msg.Provider, model: msg.Model}
}

// viewedModel is the model to describe for whatever session is on screen.
//
// For the main session that is the current selection: the sidebar is there
// to say what the next prompt will run on. A child session is a read-only
// record of a delegation that may well have run on a different model, so
// there what its own messages say wins, and the selection is only the
// fallback for a delegation that has not answered anything yet.
func (m *UI) viewedModel() *workspace.AgentModel {
	if m.viewingChildSession() {
		if model := m.recordedModel(); model != nil {
			return model
		}
	}
	return m.selectedModel()
}

// recordedModel resolves the loaded session's recorded provider/model
// against the catalog. A model the config no longer knows about (a provider
// removed since, an id renamed) still gets named rather than dropped —
// "which model ran this" is exactly the question being asked, and the id
// alone answers it better than silence does.
func (m *UI) recordedModel() *workspace.AgentModel {
	ref := m.sess.modelUsed
	if ref.provider == "" || ref.model == "" {
		return nil
	}
	catalog := catwalk.Model{ID: ref.model, Name: ref.model}
	// A config without providers is a config that can resolve nothing, and
	// that is the shape a UI built without a workspace has.
	if cfg := m.com.Config(); cfg != nil && cfg.Providers != nil {
		if known := cfg.GetModel(ref.provider, ref.model); known != nil {
			catalog = *known
		}
	}
	selected := config.SelectedModel{Provider: ref.provider, Model: ref.model}
	// The messages record no effort, but the delegation that started this
	// session does, and it is the effort those messages were produced at.
	if len(m.sess.navStack) > 0 {
		selected.ReasoningEffort = m.sess.navStack[len(m.sess.navStack)-1].effort
	}
	return &workspace.AgentModel{CatalogCfg: catalog, ModelCfg: selected}
}
