package model

import "math/rand"

var readyPlaceholders = [...]string{
	"Ready!",
	"Ready...",
	"Ready?",
	"Ready for instructions",
}

var workingPlaceholders = [...]string{
	"Working!",
	"Working...",
	"Brrrrr...",
	"Prrrrrrrr...",
	"Processing...",
	"Thinking...",
}

// editorPlaceholderState owns the randomized editor placeholder text and
// resolves it from the UI context supplied by the main update loop.
type editorPlaceholderState struct {
	ready   string
	working string
}

type editorPlaceholderContext struct {
	current                  string
	editorFocused            bool
	viewingChildSession      bool
	exitChildSessionShortcut string
	bangActive               bool
	busy                     bool
	yolo                     bool
}

func newEditorPlaceholderState() editorPlaceholderState {
	state := editorPlaceholderState{}
	state.randomize()
	return state
}

func (p *editorPlaceholderState) randomize() {
	p.working = workingPlaceholders[rand.Intn(len(workingPlaceholders))]
	p.ready = readyPlaceholders[rand.Intn(len(readyPlaceholders))]
}

func (p editorPlaceholderState) selectPlaceholder(context editorPlaceholderContext) string {
	if !context.editorFocused {
		return context.current
	}

	placeholder := p.ready
	switch {
	case context.viewingChildSession:
		placeholder = "viewing subagent session · " + context.exitChildSessionShortcut + " to return"
	case context.bangActive:
		placeholder = "Run a shell command"
	case context.busy:
		placeholder = p.working
	}
	if !context.bangActive && context.yolo {
		return "Yolo mode!"
	}
	return placeholder
}
