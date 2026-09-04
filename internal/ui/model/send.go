package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
)

// sendMessageMsg is sent to send a message.
// currently only used for mcp prompts.
//
// uiOwned: dispatched by runMCPPrompt and initializeProject. Routed by
// active screen instead, an MCP prompt run from a thread's own commands
// dialog (or the onboarding init prompt) could be sent into whichever
// screen's editor happened to be active when the round trip finished.
type sendMessageMsg struct {
	uiOwned

	Content     string
	Attachments []message.Attachment
}

// sendPendingQueueMsg advances one pending send after a session load
// completes.
//
// uiOwned: dispatched from several places across shell.go and
// update_session.go, all pendingSend-queue-draining steps for one UI's own
// editor. Routed by active screen instead, a queued send drained while a
// different screen was on top applied to the wrong UI's pendingSend state
// (or was dropped by the dashboard), leaving the queue stuck.
type sendPendingQueueMsg struct{ uiOwned }

type notificationSentMsg struct{ uiOwned }

// sendMessage sends a message with the given content and attachments.
// All I/O (AgentReadyErr, CreateSession, AgentRun) runs inside a tea.Cmd
// so that the Update goroutine is never blocked.
func (m *UI) sendMessage(content string, attachments ...message.Attachment) tea.Cmd {
	if m.sess.viewingChildSession() {
		return util.ReportWarn("viewing subagent session · " + m.exitChildSessionShortcut() + " to return")
	}
	if m.sess.current != nil && m.sess.loadExpectedID != "" && m.sess.loadExpectedID != m.sess.current.ID {
		m.editor.pendingSend.enqueue(sendQueueItem{
			content:        content,
			attachments:    attachments,
			sessionID:      m.sess.loadExpectedID,
			loadGeneration: m.sess.loadGen,
		})
		return nil
	}
	if m.sess.current != nil {
		if m.editor.pendingSend.activeNow() {
			m.editor.pendingSend.enqueue(sendQueueItem{
				content:        content,
				attachments:    attachments,
				sessionID:      m.sess.current.ID,
				loadGeneration: m.sess.loadGen,
			})
			return nil
		}
		m.editor.pendingSend.beginActive()
		// A busy session queues this prompt instead of running it, and
		// nothing is persisted until the running turn takes it. Show it
		// as waiting rather than letting it vanish until then.
		if m.isAgentBusy() {
			m.queued.show(m.chat, m.com.Styles, content)
		}
	}
	return m.sendMessageNow(content, attachments...)
}

func (m *UI) sendMessageNow(content string, attachments ...message.Attachment) tea.Cmd {
	if m.sess.current == nil && m.editor.pendingSend.loadingNow() {
		m.editor.pendingSend.enqueue(sendQueueItem{content: content, attachments: attachments, generation: m.editor.pendingSend.generationNow()})
		return nil
	}

	ws := m.com.Workspace
	styles := m.com.Styles
	reads := append([]string(nil), m.sess.fileReads...)
	ctx := m.com.Context()
	sessionID := ""
	generation := m.editor.pendingSend.generationNow()
	loadGeneration := m.sess.loadGen
	creating := m.sess.current == nil
	if creating {
		generation = m.editor.pendingSend.beginLoading()
	} else {
		sessionID = m.sess.current.ID
		m.wsCache.agentBusyCache.Set(true)
		m.wsCache.busyFetchGen++
		m.promptQueue.invalidate()
	}

	owner := m
	return func() tea.Msg {
		if err := ws.AgentReadyErr(); err != nil {
			return sendMessageErrorMsg{uiOwned: uiOwned{owner: owner}, Err: err, generation: generation, sessionID: sessionID, loadGeneration: loadGeneration, creating: creating, content: content}
		}
		if creating {
			created, err := ws.CreateSession(ctx, "New Session")
			if err != nil {
				return sendMessageErrorMsg{uiOwned: uiOwned{owner: owner}, Err: err, generation: generation, creating: true}
			}
			return createSessionMsg{uiOwned: uiOwned{owner: owner}, session: created, content: content, attachments: attachments, generation: generation}
		}
		// Only start the clock for a turn that is actually beginning.
		// AgentRun below either starts a fresh turn or folds this prompt
		// into one already running (steering) — see
		// attachedThreadWorkspace.AgentRun's doc. Calling StartTurn
		// unconditionally reset an in-flight turn's elapsed time on every
		// follow-up message sent while it was still running, and the
		// symmetric miss — nothing starting the clock when the agent's own
		// queue later hands a folded prompt to its own next turn — remains
		// open: no event today tells this client that happened.
		if !ws.AgentIsSessionBusy(sessionID) {
			common.StartTurn(sessionID)
		}
		for _, path := range reads {
			ws.FileTrackerRecordRead(ctx, sessionID, path)
			ws.LSPStart(ctx, path)
		}
		if err := ws.AgentRun(ctx, sessionID, content, attachments...); err != nil && !errors.Is(err, context.Canceled) {
			if quota, ok := workspace.GetProviderQuotaInfo(err); ok {
				link := styles.Dialog.OAuth.Link.Hyperlink(quota.SettingsURL, "id=copilot").Render(quota.SettingsURL)
				return sendMessageErrorMsg{uiOwned: uiOwned{owner: owner}, Err: fmt.Errorf("%q is not enabled in Copilot. Go to the following page to enable it. Then, wait 5 minutes before trying again. %s", quota.Model, link), generation: generation, sessionID: sessionID, loadGeneration: loadGeneration, content: content}
			}
			return sendMessageErrorMsg{uiOwned: uiOwned{owner: owner}, Err: err, generation: generation, sessionID: sessionID, loadGeneration: loadGeneration, content: content}
		}
		return agentRunSubmittedMsg{uiOwned: uiOwned{owner: owner}, sessionID: sessionID, loadGeneration: loadGeneration}
	}
}

const cancelTimerDuration = 2 * time.Second

// cancelTimerCmd creates a command that expires the cancel timer, owned by
// owner so Root hands the expiry back to the *UI that started it rather
// than to whichever screen is active two seconds later.
func cancelTimerCmd(owner *UI) tea.Cmd {
	return tea.Tick(cancelTimerDuration, func(time.Time) tea.Msg {
		return cancelTimerExpiredMsg{uiOwned: uiOwned{owner: owner}}
	})
}

// cancelAgent handles the cancel key press. The first press arms the
// confirmation and starts a timer. The second press (before the timer
// expires) actually cancels the agent.
func (m *UI) cancelAgent() tea.Cmd {
	if !m.sess.hasSession() {
		return nil
	}

	// Gate on the memoized ready state: esc is a hot key and AgentIsReady
	// is treated as IO — see workspace_cache.go.
	if !m.wsCache.agentCache.Value.ready {
		return nil
	}

	if m.cancellation.confirm() {
		return m.confirmAgentCancellation()
	}

	// Queued prompts pending: esc clears the queue. Decide from the cached
	// count (event-driven) instead of a synchronous workspace probe.
	if m.promptQueue.count() > 0 {
		m.com.Workspace.AgentClearQueue(m.sess.current.ID)
		m.queued.clear(m.chat)
		// Bump the queue generation so a fetch started before this clear
		// cannot land and repopulate the pill we just emptied, then write
		// the now-authoritative empty queue through as fresh.
		m.promptQueue.clear()
		m.updateLayoutAndSize()
		return nil
	}

	// First escape press arms the confirmation and starts its timer.
	m.cancellation.arm()
	return cancelTimerCmd(m)
}

// confirmAgentCancellation performs the UI orchestration that follows a
// confirmed cancellation request.
func (m *UI) confirmAgentCancellation() tea.Cmd {
	// Cancel a running bang command if one is in progress.
	m.editor.bang.cancelRunning()

	m.com.Workspace.AgentCancel(m.sess.current.ID)
	// A cancelled turn publishes neither AgentNotificationFinished (only
	// sent on a clean end) nor AgentNotificationError (only sent for a
	// non-cancellation failure — see handleAgentNotification), so this is
	// the only place StopTurn runs for it. Left uncalled, the timer table
	// entry outlives the turn it was tracking.
	common.StopTurn(m.sess.current.ID)
	// A cancel clears the agent's queue too, so nothing is left waiting for
	// these placeholders to stand in for.
	m.queued.clear(m.chat)
	// Stop the spinning todo indicator and drop the memoized busy state the
	// cancel just changed; the session panel reads m.panel.isSpinning fresh
	// on every draw, and again once the off-thread refresh (and the agent's
	// own events) land.
	m.panel.isSpinning = false
	m.wsCache.invalidateBusyCaches()
	return m.dispatchBusyRefresh()
}
