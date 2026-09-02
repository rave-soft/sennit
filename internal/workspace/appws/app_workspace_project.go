package appws

import (
	"context"
	"errors"

	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/commands"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/skills"
)

// -- Project lifecycle --

func (w *AppWorkspace) ProjectNeedsInitialization() (bool, error) {
	return config.ProjectNeedsInitialization(w.store)
}

func (w *AppWorkspace) MarkProjectInitialized() error {
	return config.MarkProjectInitialized(w.store)
}

func (w *AppWorkspace) InitializePrompt() (string, error) {
	return agent.InitializePrompt(w.store)
}

func (w *AppWorkspace) ListSkills(_ context.Context) ([]skills.CatalogEntry, error) {
	mgr := w.app.Skills
	return skills.Catalog(mgr.ActiveSkills(), mgr.ResolvedPaths(), mgr.WorkingDir()), nil
}

// ListCustomCommands implements Workspace: the markdown commands found in
// the config directories, followed by the user-invocable skills.
//
// A failure to read one half is logged and the other half is still
// returned. The palette exists to offer what is available, and a broken
// commands directory is a worse reason to show an empty list than to show
// the skills that did load — which is also why the error is returned
// rather than swallowed: the caller decides whether to say anything.
func (w *AppWorkspace) ListCustomCommands(ctx context.Context) ([]commands.CustomCommand, error) {
	custom, cmdErr := commands.LoadCustomCommands(w.store.Config())

	entries, skillErr := w.ListSkills(ctx)
	if skillErr == nil {
		custom = append(custom, commands.FromSkillCatalog(entries)...)
	}

	return custom, errors.Join(cmdErr, skillErr)
}

func (w *AppWorkspace) ReadSkill(_ context.Context, skillID string) ([]byte, skills.SkillReadResult, error) {
	mgr := w.app.Skills
	return skills.ReadContent(mgr.ActiveSkills(), mgr.ResolvedPaths(), mgr.WorkingDir(), skillID)
}

// ConfigProblems implements Workspace.
func (w *AppWorkspace) ConfigProblems() []config.Problem {
	return config.Doctor(w.store.Config())
}

// SkillStates implements Workspace.
func (w *AppWorkspace) SkillStates() []*skills.SkillState {
	return w.app.Skills.States()
}

// BuiltinSkills implements Workspace.
func (w *AppWorkspace) BuiltinSkills() []*skills.Skill {
	return skills.DiscoverBuiltin()
}
