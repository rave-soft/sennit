package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/util"
	"github.com/rave-soft/braid/internal/workspace"
)

// sendMessage sends a message with the given content and attachments.
// All I/O (AgentReadyErr, CreateSession, AgentRun) runs inside a tea.Cmd
// so that the Update goroutine is never blocked.
func (m *UI) sendMessage(content string, attachments ...message.Attachment) tea.Cmd {
	if m.viewingChildSession() {
		return util.ReportWarn("viewing subagent session · " + m.exitChildSessionShortcut() + " to return")
	}
	if m.session != nil && m.sessionLoadExpectedID != "" && m.sessionLoadExpectedID != m.session.ID {
		m.editor.pendingSendQueue = append(m.editor.pendingSendQueue, sendQueueItem{
			content:        content,
			attachments:    attachments,
			sessionID:      m.sessionLoadExpectedID,
			loadGeneration: m.sessionLoadGen,
		})
		return nil
	}
	if m.session != nil {
		if m.editor.pendingSendActive {
			m.editor.pendingSendQueue = append(m.editor.pendingSendQueue, sendQueueItem{
				content:        content,
				attachments:    attachments,
				sessionID:      m.session.ID,
				loadGeneration: m.sessionLoadGen,
			})
			return nil
		}
		m.editor.pendingSendActive = true
	}
	return m.sendMessageNow(content, attachments...)
}

func (m *UI) sendMessageNow(content string, attachments ...message.Attachment) tea.Cmd {
	if m.session == nil && m.editor.pendingSendLoading {
		m.editor.pendingSendQueue = append(m.editor.pendingSendQueue, sendQueueItem{content: content, attachments: attachments, generation: m.editor.pendingSendGen})
		return nil
	}

	ws := m.com.Workspace
	styles := m.com.Styles
	reads := append([]string(nil), m.sessionFileReads...)
	ctx := context.Background()
	sessionID := ""
	generation := m.editor.pendingSendGen
	loadGeneration := m.sessionLoadGen
	creating := m.session == nil
	if creating {
		m.editor.pendingSendLoading = true
		m.editor.pendingSendGen++
		generation = m.editor.pendingSendGen
	} else {
		sessionID = m.session.ID
		m.wsCache.agentBusyCache.set(true)
		m.wsCache.busyFetchGen++
		m.invalidatePromptQueue()
	}

	return func() tea.Msg {
		if err := ws.AgentReadyErr(); err != nil {
			return sendMessageErrorMsg{Err: err, generation: generation, sessionID: sessionID, loadGeneration: loadGeneration, creating: creating}
		}
		if creating {
			created, err := ws.CreateSession(ctx, "New Session")
			if err != nil {
				return sendMessageErrorMsg{Err: err, generation: generation, creating: true}
			}
			return createSessionMsg{session: created, content: content, attachments: attachments, generation: generation}
		}
		common.StartTurn()
		for _, path := range reads {
			ws.FileTrackerRecordRead(ctx, sessionID, path)
			ws.LSPStart(ctx, path)
		}
		if err := ws.AgentRun(ctx, sessionID, content, attachments...); err != nil && !errors.Is(err, context.Canceled) {
			if quota, ok := workspace.GetProviderQuotaInfo(err); ok {
				link := styles.Dialog.OAuth.Link.Hyperlink(quota.SettingsURL, "id=copilot").Render(quota.SettingsURL)
				return sendMessageErrorMsg{Err: fmt.Errorf("%q is not enabled in Copilot. Go to the following page to enable it. Then, wait 5 minutes before trying again. %s", quota.Model, link), generation: generation, sessionID: sessionID, loadGeneration: loadGeneration}
			}
			return sendMessageErrorMsg{Err: err, generation: generation, sessionID: sessionID, loadGeneration: loadGeneration}
		}
		return agentRunSubmittedMsg{sessionID: sessionID, loadGeneration: loadGeneration}
	}
}

const cancelTimerDuration = 2 * time.Second

// cancelTimerCmd creates a command that expires the cancel timer.
func cancelTimerCmd() tea.Cmd {
	return tea.Tick(cancelTimerDuration, func(time.Time) tea.Msg {
		return cancelTimerExpiredMsg{}
	})
}

// cancelAgent handles the cancel key press. The first press sets isCanceling to true
// and starts a timer. The second press (before the timer expires) actually
// cancels the proto.
func (m *UI) cancelAgent() tea.Cmd {
	if !m.hasSession() {
		return nil
	}

	// Gate on the memoized ready state: esc is a hot key and AgentIsReady
	// is a synchronous HTTP round-trip in client/server mode.
	if !m.wsCache.agentReady {
		return nil
	}

	if m.isCanceling {
		// Second escape press — actually cancel.
		m.isCanceling = false

		// Cancel a running bang command if one is in progress.
		if m.editor.bangCancel != nil {
			m.editor.bangCancel()
			m.editor.bangCancel = nil
		}

		m.com.Workspace.AgentCancel(m.session.ID)
		// Stop the spinning todo indicator and drop the memoized busy
		// state the cancel just changed; the session panel reads
		// m.panel.panelIsSpinning fresh on every draw, and again once the
		// off-thread refresh (and the agent's own events) land.
		m.panel.panelIsSpinning = false
		m.invalidateBusyCaches()
		return m.dispatchBusyRefresh()
	}

	// Queued prompts pending: esc clears the queue. Decide from the cached
	// count (event-driven) instead of a synchronous workspace probe.
	if m.wsCache.promptQueue > 0 {
		m.com.Workspace.AgentClearQueue(m.session.ID)
		m.wsCache.promptQueue = 0
		m.wsCache.promptQueueItems = nil
		m.wsCache.promptQueueCheckedAt = time.Now()
		// Bump the queue generation so a fetch started before this clear
		// cannot land and repopulate the pill we just emptied.
		m.invalidatePromptQueue()
		m.updateLayoutAndSize()
		return nil
	}

	// First escape press - set canceling state and start timer.
	m.isCanceling = true
	return cancelTimerCmd()
}
