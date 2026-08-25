package format

import (
	"context"
	"errors"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/ui/anim"
)

// Spinner wraps the bubbles spinner for non-interactive mode
type Spinner struct {
	done chan struct{}
	prog *tea.Program
}

type model struct {
	cancel context.CancelFunc
	anim   *anim.Anim
}

func (m model) Init() tea.Cmd  { return m.anim.Start() }
func (m model) View() tea.View { return tea.NewView(m.anim.Render()) }

// setLabelMsg carries a new spinner label into the tea loop. The label is
// written by [Anim.SetLabel], which mutates the animation's pre-rendered
// state, while Render runs on the tea goroutine — so the update travels
// as a message rather than being applied from the caller's goroutine.
type setLabelMsg string

// SetLabel changes the word the spinner shows, so a long non-interactive
// run says what the agent is doing instead of one fixed label for the
// whole turn. Safe to call from any goroutine; a no-op once the spinner
// has stopped.
func (s *Spinner) SetLabel(label string) {
	s.prog.Send(setLabelMsg(label))
}

// Update implements tea.Model.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case setLabelMsg:
		m.anim.SetLabel(string(msg))
		return m, nil
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.cancel()
			return m, tea.Quit
		}
	case anim.StepMsg:
		cmd := m.anim.Animate(msg)
		return m, cmd
	}
	return m, nil
}

// NewSpinner creates a new spinner with the given message
func NewSpinner(ctx context.Context, cancel context.CancelFunc, animSettings anim.Settings) *Spinner {
	m := model{
		anim:   anim.New(animSettings),
		cancel: cancel,
	}

	p := tea.NewProgram(m, tea.WithOutput(os.Stderr), tea.WithContext(ctx))

	return &Spinner{
		prog: p,
		done: make(chan struct{}, 1),
	}
}

// Start begins the spinner animation
func (s *Spinner) Start() {
	go func() {
		defer close(s.done)
		_, err := s.prog.Run()
		// ensures line is cleared
		fmt.Fprint(os.Stderr, ansi.EraseEntireLine)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, tea.ErrInterrupted) {
			fmt.Fprintf(os.Stderr, "Error running spinner: %v\n", err)
		}
	}()
}

// Stop ends the spinner animation
func (s *Spinner) Stop() {
	s.prog.Quit()
	<-s.done
}
