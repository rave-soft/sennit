package appws

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/commands"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/doctor"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/workspace"
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
func (w *AppWorkspace) ListCustomCommands(ctx context.Context) ([]workspace.CustomCommand, error) {
	custom, cmdErr := commands.LoadCustomCommands(w.store.Config())

	entries, skillErr := w.ListSkills(ctx)
	if skillErr == nil {
		custom = append(custom, commands.FromSkillCatalog(entries)...)
	}

	return toWorkspaceCustomCommands(custom), errors.Join(cmdErr, skillErr)
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

// DoctorProblems implements Workspace.
func (w *AppWorkspace) DoctorProblems() []config.Problem {
	return w.doctorProblemsWithEnvironment(doctor.EnvironmentProblems)
}

// doctorProblemsWithEnvironment collects every config.Problem for this
// workspace: ConfigProblems' static findings, any MCP server currently
// stuck in an error/needs-auth state, and any SKILL.md that failed to
// parse or validate. This mirrors sennit_info's [problems] section
// (internal/agent/tools/sennit_info.go's writeProblems) — the same merge,
// on the workspace side of the UI boundary now that the /doctor dialog no
// longer assembles it itself.
//
// environmentProblems is injected so tests can drive this without the real
// environment probe (which shells out and walks PATH).
func (w *AppWorkspace) doctorProblemsWithEnvironment(environmentProblems func() []config.Problem) []config.Problem {
	problems := w.ConfigProblems()
	problems = append(problems, environmentProblems()...)
	problems = append(problems, doctor.SkillProblems(w.SkillStates())...)
	problems = append(problems, mcpDoctorProblems(w.MCPGetStates())...)
	return problems
}

// mcpDoctorProblems turns every MCP server stuck in an error/needs-auth
// state into a config.Problem. It takes the state map rather than reading
// w.MCPGetStates() itself so it can be tested with a literal map, without
// standing up a real MCP registry.
func mcpDoctorProblems(states map[string]workspace.MCPClientInfo) []config.Problem {
	var problems []config.Problem
	// Sorted, because map iteration is random and this list is rendered:
	// unsorted, the MCP problems would shuffle position on every call.
	for _, name := range slices.Sorted(maps.Keys(states)) {
		info := states[name]
		if info.State != workspace.MCPStateError && info.State != workspace.MCPStateNeedsAuth {
			continue
		}
		msg := fmt.Sprintf("mcp server %s is in state %s", name, info.State)
		if info.Error != nil {
			msg += ": " + info.Error.Error()
		}
		problems = append(problems, config.Problem{
			Severity: config.SeverityError,
			Area:     config.AreaMCP,
			Subject:  name,
			Message:  msg,
		})
	}
	return problems
}
