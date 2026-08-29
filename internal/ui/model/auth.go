package model

import (
	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/dialog"
)

func (m *UI) handleReAuthenticate(providerID string) tea.Cmd {
	cfg := m.com.Config()
	if cfg == nil {
		return nil
	}
	providerCfg, ok := cfg.Providers.Get(providerID)
	if !ok {
		return nil
	}
	if _, ok := cfg.Agents[config.AgentCoder]; !ok {
		return nil
	}
	// The coder agent leaves Model unset (it inherits the app's configured
	// model), so the model it actually runs on is always cfg.Model.
	return m.openAuthenticationDialog(providerCfg.ToProvider(), cfg.Model)
}

// handleAWSSSOAuth opens the AWS SSO progress dialog (or updates the SSO URL
// on an already-open one). The refresh command runs in the coordinator; this
// dialog is a display surface driven by agent notifications.
func (w *widgets) handleAWSSSOAuth(com *common.Common, command, url string) tea.Cmd {
	// Update the URL on an already-open dialog.
	if existing := w.dialog.Dialog(dialog.AWSSSOID); existing != nil {
		if awsDlg, ok := existing.(*dialog.AWSSSO); ok && url != "" {
			awsDlg.SetURL(url)
		}
		w.dialog.BringToFront(dialog.AWSSSOID)
		return nil
	}
	if command == "" {
		return nil
	}
	dlg, cmd := dialog.NewAWSSSO(com, command)
	if url != "" {
		dlg.SetURL(url)
	}
	w.dialog.OpenDialogWithGrace(dlg)
	return cmd
}

// handleAWSSSOAuthResult finishes the AWS SSO dialog once the refresh command
// exits: it closes on success or shows the error so the user can dismiss it.
func (w *widgets) handleAWSSSOAuthResult(errMsg string) tea.Cmd {
	existing := w.dialog.Dialog(dialog.AWSSSOID)
	if existing == nil {
		return nil
	}
	awsDlg, ok := existing.(*dialog.AWSSSO)
	if !ok {
		return nil
	}
	if errMsg == "" {
		// Success: the turn retries transparently, so no need to linger.
		w.dialog.CloseDialog(dialog.AWSSSOID)
		return nil
	}
	awsDlg.Finish(errMsg)
	return nil
}
