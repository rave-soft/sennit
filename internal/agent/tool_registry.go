package agent

import (
	"context"
	"slices"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/skills"
)

// buildToolsCtx bundles the values a toolSpec's Gate/Build needs, computed
// once per buildTools call rather than once per tool.
type buildToolsCtx struct {
	agent       config.Agent
	isSubAgent  bool
	interactive bool
	cfg         agentConfig

	modelID       string
	logFile       string
	searchBackend tools.SearchBackend

	allSkills, activeSkills []*skills.Skill
	skillTracker            *skills.Tracker

	threads            tools.ThreadManager
	taskManager        tools.TaskManager
	backgroundAgentsOn bool
}

// toolSpec is one row of buildTools' registry: an existence gate for a
// tool or a group of tools that always share one, and how to build them.
// Gate decides whether the row's tools are offered *at all* in this
// build, before the uniform AllowedTools/AllowedMCP filter in buildTools
// narrows the set further. Every Gate below is copied verbatim from the
// hand-written buildTools this table replaces — the tool set is what the
// model is allowed to do, so a changed gate is a security regression, not
// a style one. See TestBuildToolsPinnedSet for the regression net.
type toolSpec struct {
	Name  string // documents the row; each tool's real name is Info().Name.
	Gate  func(b *buildToolsCtx) bool
	Build func(ctx context.Context, c *coordinator, b *buildToolsCtx) ([]fantasy.AgentTool, error)
}

func always(*buildToolsCtx) bool { return true }

// Gates shared by more than one row.
func notSubAgent(b *buildToolsCtx) bool { return !b.isSubAgent }
func threadGate(b *buildToolsCtx) bool  { return !b.isSubAgent && b.threads != nil }
func taskGate(b *buildToolsCtx) bool {
	return !b.isSubAgent && b.backgroundAgentsOn && b.taskManager != nil
}
func lspGate(b *buildToolsCtx) bool { return b.cfg.HasLSP() || b.cfg.AutoLSPEnabled() }
func mcpGate(b *buildToolsCtx) bool { return b.cfg.HasMCP() }

// one adapts a single-tool builder into a toolSpec.Build func for rows
// that never fail and never need ctx.
func one(fn func(c *coordinator, b *buildToolsCtx) fantasy.AgentTool) func(context.Context, *coordinator, *buildToolsCtx) ([]fantasy.AgentTool, error) {
	return func(_ context.Context, c *coordinator, b *buildToolsCtx) ([]fantasy.AgentTool, error) {
		return []fantasy.AgentTool{fn(c, b)}, nil
	}
}

// toolSpecs is the registry buildTools walks. Two tools are deliberately
// left out of it — see buildTools' doc comment for why:
//   - the user-defined agent tools (custom_agent_tool.go), whose count and
//     names depend on config.Agents and so cannot be fixed rows;
//   - the per-MCP-server tools (tools.GetMCPTools), gated by AllowedMCP
//     rather than AllowedTools and likewise dynamic in name and count.
func toolSpecs() []toolSpec {
	return []toolSpec{
		// Gated on AllowedTools up front, unlike every other row: building
		// either does real work (agentTool recursively runs buildAgent for
		// the "task" role), worth skipping outright rather than relying on
		// the post-table filter every other row uses.
		{
			AgentToolName, func(b *buildToolsCtx) bool { return slices.Contains(b.agent.AllowedTools, AgentToolName) },
			func(ctx context.Context, c *coordinator, b *buildToolsCtx) ([]fantasy.AgentTool, error) {
				t, err := c.agentTool(ctx, b.cfg)
				if err != nil {
					return nil, err
				}
				return []fantasy.AgentTool{t}, nil
			},
		},
		{
			tools.AgenticFetchToolName, func(b *buildToolsCtx) bool { return slices.Contains(b.agent.AllowedTools, tools.AgenticFetchToolName) },
			func(ctx context.Context, c *coordinator, b *buildToolsCtx) ([]fantasy.AgentTool, error) {
				t, err := c.agenticFetchTool(ctx, nil)
				if err != nil {
					return nil, err
				}
				return []fantasy.AgentTool{t}, nil
			},
		},

		// Always-built core tools; agent.AllowedTools decides who actually
		// gets each one via the uniform filter in buildTools.
		{"core", always, func(_ context.Context, c *coordinator, b *buildToolsCtx) ([]fantasy.AgentTool, error) {
			return []fantasy.AgentTool{
				tools.NewBashTool(c.permissions, c.cfg.WorkingDir(), b.cfg.Attribution(), b.modelID, c.background),
				tools.NewSennitInfoTool(c.cfg, c.mcp, c.lspManager, b.allSkills, b.activeSkills, b.skillTracker, c.skillStates()),
				tools.NewSennitLogsTool(b.logFile),
				tools.NewJobOutputTool(c.background),
				tools.NewJobKillTool(c.background),
				tools.NewDownloadTool(c.permissions, c.cfg.WorkingDir(), nil),
				tools.NewEditTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
				tools.NewMultiEditTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
				tools.NewFetchTool(c.permissions, c.cfg.WorkingDir(), nil),
				tools.NewWebFetchTool(c.permissions, c.cfg.WorkingDir(), nil),
				tools.NewWebSearchTool(c.permissions, c.cfg.WorkingDir(), nil, b.searchBackend),
				tools.NewGlobTool(c.cfg.WorkingDir(), b.cfg.Glob()),
				tools.NewSearchTool(c.cfg.WorkingDir(), b.cfg.Grep()),
				tools.NewLsTool(c.permissions, c.cfg.WorkingDir(), b.cfg.Ls()),
				tools.NewTodosTool(c.sessions),
				tools.NewReadTool(c.lspManager, c.permissions, c.filetracker, b.skillTracker, c.cfg.WorkingDir(), b.cfg.SkillsPaths()...),
				tools.NewWriteTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
			}, nil
		}},

		// Thread tools: top-level agent of the workspace owning the thread
		// manager only — sub-agents nesting workspace ownership isn't
		// supported, and non-git/thread-spawned workspaces have no manager.
		{"threads", threadGate, func(_ context.Context, c *coordinator, b *buildToolsCtx) ([]fantasy.AgentTool, error) {
			return []fantasy.AgentTool{
				tools.NewThreadCreateTool(b.threads, c.permissions),
				tools.NewThreadListTool(b.threads),
				tools.NewThreadStatusTool(b.threads),
				tools.NewThreadSendTool(b.threads),
				tools.NewThreadWaitTool(b.threads),
				tools.NewThreadMergeTool(b.threads, c.permissions),
				tools.NewThreadRemoveTool(b.threads, c.permissions),
			}, nil
		}},

		// Task tools observe/steer background task delegations (see the
		// "agent" tool's background mode). Same restriction as thread
		// tools, plus the explicit options.background_agents opt-out.
		{"tasks", taskGate, func(_ context.Context, c *coordinator, b *buildToolsCtx) ([]fantasy.AgentTool, error) {
			return []fantasy.AgentTool{
				tools.NewTaskListTool(b.taskManager),
				tools.NewTaskResultTool(b.taskManager),
				tools.NewTaskCancelTool(b.taskManager, c.permissions),
				tools.NewTaskSendTool(b.taskManager),
				tools.NewTaskOutputTool(b.taskManager),
			}, nil
		}},

		// ask_parent (Coordinator.SendToParent) is registered for every
		// non-sub-agent build, including the parent's own top-level
		// session (a task shares its parent's coordinator/tool list), and
		// gated for real at runtime instead — see withoutUnusableParentTool.
		{"ask_parent", notSubAgent, one(func(c *coordinator, b *buildToolsCtx) fantasy.AgentTool { return tools.NewAskParentTool(c) })},

		// Question tool is interactive-only and not available to sub-agents.
		{
			"question", func(b *buildToolsCtx) bool { return !b.isSubAgent && b.interactive },
			one(func(c *coordinator, b *buildToolsCtx) fantasy.AgentTool { return tools.NewQuestionTool(c.questions) }),
		},

		// LSP tools: offered whenever the user configured an LSP
		// explicitly, or auto_lsp is unset/true.
		{"lsp", lspGate, func(_ context.Context, c *coordinator, b *buildToolsCtx) ([]fantasy.AgentTool, error) {
			return []fantasy.AgentTool{
				tools.NewDiagnosticsTool(c.lspManager),
				tools.NewReferencesTool(c.lspManager, c.cfg.WorkingDir()),
				tools.NewLSPRestartTool(c.lspManager),
				tools.NewSymbolsTool(c.lspManager, c.cfg.WorkingDir()),
				tools.NewDefinitionTool(c.lspManager, c.cfg.WorkingDir()),
				tools.NewCallHierarchyTool(c.lspManager, c.cfg.WorkingDir()),
				tools.NewRenameTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
				tools.NewReplaceSymbolTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
			}, nil
		}},

		// MCP resource browsing tools: offered whenever at least one MCP
		// server is configured (independent of AllowedMCP, which only
		// gates the per-server tools built outside this table).
		{"mcp_resources", mcpGate, func(_ context.Context, c *coordinator, b *buildToolsCtx) ([]fantasy.AgentTool, error) {
			return []fantasy.AgentTool{
				tools.NewListMCPResourcesTool(c.cfg, c.mcp, c.permissions),
				tools.NewReadMCPResourceTool(c.cfg, c.mcp, c.permissions),
			}, nil
		}},
	}
}
