package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/styles"
)

func confirmTestCommon() *common.Common {
	sty := styles.BraidDark()
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

	remove := NewThreadRemoveConfirm(com, "thread-1", "test")
	if action := remove.HandleMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); action != nil {
		t.Fatalf("ctrl+c must not confirm thread removal, got %T", action)
	}
	if _, ok := remove.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEsc}).(ActionClose); !ok {
		t.Fatal("Esc must cancel thread removal")
	}
	remove = NewThreadRemoveConfirm(com, "thread-1", "test")
	if action, ok := remove.HandleMsg(tea.KeyPressMsg{Text: "y"}).(ActionRemoveThreadConfirmed); !ok || action.ID != "thread-1" {
		t.Fatalf("y must confirm thread removal with its ID, got %#v", action)
	}
}
