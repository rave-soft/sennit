package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

func confirmTestCommon() *common.Common {
	sty := styles.SennitDark()
	return &common.Common{Styles: &sty}
}

func TestConfirmDialogKeySemantics(t *testing.T) {
	com := confirmTestCommon()
	quit := NewQuit(com)
	if _, ok := quit.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(ActionClose); !ok {
		t.Fatal("Enter must select the default No action")
	}
	quit = NewQuit(com)
	quit.HandleMsg(tea.KeyPressMsg{Code: tea.KeyRight})
	if _, ok := quit.HandleMsg(tea.KeyPressMsg{Code: ' '}).(ActionQuit); !ok {
		t.Fatal("Space after a toggle must confirm Quit")
	}
	if _, ok := NewQuit(com).HandleMsg(tea.KeyPressMsg{Text: "y"}).(ActionQuit); !ok {
		t.Fatal("y must confirm Quit")
	}
	if _, ok := NewQuit(com).HandleMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}).(ActionQuit); !ok {
		t.Fatal("ctrl+c must retain Quit's quit behavior")
	}

	cleanup := NewDelegationCleanupConfirm(com, "thread-1", "test")
	if action := cleanup.HandleMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); action != nil {
		t.Fatalf("ctrl+c must not confirm delegation cleanup, got %T", action)
	}
	if _, ok := cleanup.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEsc}).(ActionClose); !ok {
		t.Fatal("Esc must cancel delegation cleanup")
	}
	cleanup = NewDelegationCleanupConfirm(com, "thread-1", "test")
	if action, ok := cleanup.HandleMsg(tea.KeyPressMsg{Text: "y"}).(ActionCleanupDelegationConfirmed); !ok || action.ID != "thread-1" {
		t.Fatalf("y must confirm delegation cleanup with its ID, got %#v", action)
	}
}
