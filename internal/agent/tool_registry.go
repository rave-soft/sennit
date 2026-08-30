package agent

import (
	"context"
	"slices"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/toolmeta"
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
	toolAvailability   tools.ToolAvailabilityOption

	inputs runtimeToolInputs
}

// toolSpec lists the exact static tool names built by a row. Their gate is
// always derived from toolmeta; grouped rows must have one shared gate.
type toolSpec struct {
	Names []string
	Build func(ctx context.Context, b *runtimeBuilder, bctx *buildToolsCtx) ([]fantasy.AgentTool, error)
}

func specGate(spec toolSpec) (toolmeta.Gate, bool) {
	var gate toolmeta.Gate
	for i, name := range spec.Names {
		d, ok := toolmeta.Lookup(name)
		if !ok {
			panic("agent: tool spec has no metadata for " + name)
		}
		if i == 0 {
			gate = d.Gate
			continue
		}
		if d.Gate != gate {
			panic("agent: grouped tool spec has mixed gates")
		}
	}
	return gate, len(spec.Names) != 0
}

// gateAllows is the sole runtime interpretation of tool metadata gates.
func gateAllows(g toolmeta.Gate, name string, b *buildToolsCtx) bool {
	switch g {
	case toolmeta.GateAlways:
		return true
	case toolmeta.GateAllowed:
		return slices.Contains(b.agent.AllowedTools, name)
	case toolmeta.GateNotSubAgent:
		return !b.isSubAgent
	case toolmeta.GateThreads:
		return !b.isSubAgent && b.threads != nil
	case toolmeta.GateTasks:
		return !b.isSubAgent && b.backgroundAgentsOn && b.taskManager != nil
	case toolmeta.GateLSP:
		return b.cfg.HasLSP() || b.cfg.AutoLSPEnabled()
	case toolmeta.GateMCP:
		return b.cfg.HasMCP()
	case toolmeta.GateInteractive:
		return !b.isSubAgent && b.interactive
	default:
		return false
	}
}

// one adapts a single-tool builder into a toolSpec.Build func for rows
// that never fail and never need ctx.
func one(fn func(b *buildToolsCtx) fantasy.AgentTool) func(context.Context, *runtimeBuilder, *buildToolsCtx) ([]fantasy.AgentTool, error) {
	return func(_ context.Context, _ *runtimeBuilder, b *buildToolsCtx) ([]fantasy.AgentTool, error) {
		return []fantasy.AgentTool{fn(b)}, nil
	}
}

// toolSpecs is the registry buildTools walks. Two tools are deliberately
// left out of it — see buildTools' doc comment for why:
//   - the user-defined agent tools (custom_agent_tool.go), whose count and
//     names depend on config.Agents and so cannot be fixed rows;
//   - the per-MCP-server tools (tools.GetMCPTools), gated by AllowedMCP
//     rather than AllowedTools and likewise dynamic in name and count.
func coreToolNames() []string {
	names := []string{"bash", "git_status", "git_diff", "git_log", "sennit_info", "sennit_logs", "agent_trace", "job_output", "job_kill", "download", "edit", "multiedit", "fetch", "web_fetch", "web_search", "glob"}
	if tools.HasRipgrep() {
		names = append(names, tools.RipgrepToolName)
	} else {
		names = append(names, tools.GrepToolName)
	}
	return append(names, "ls", "todos", "read", tools.MultiReadToolName, "write")
}

func toolSpecs() []toolSpec {
	return []toolSpec{
		// Gated on AllowedTools up front, unlike every other row: building
		// either does real work (agentTool recursively runs buildAgent for
		// the "task" role), worth skipping outright rather than relying on
		// the post-table filter every other row uses.
		{
			[]string{AgentToolName},
			func(ctx context.Context, rb *runtimeBuilder, b *buildToolsCtx) ([]fantasy.AgentTool, error) {
				return []fantasy.AgentTool{b.inputs.delegationToolsBuilt[AgentToolName]}, nil
			},
		},
		{
			[]string{tools.AgenticFetchToolName},
			func(ctx context.Context, rb *runtimeBuilder, b *buildToolsCtx) ([]fantasy.AgentTool, error) {
				return []fantasy.AgentTool{b.inputs.delegationToolsBuilt[tools.AgenticFetchToolName]}, nil
			},
		},

		// Always-built core tools; agent.AllowedTools decides who actually
		// gets each one via the uniform filter in buildTools.
		{coreToolNames(), func(_ context.Context, rb *runtimeBuilder, b *buildToolsCtx) ([]fantasy.AgentTool, error) {
			f := b.inputs
			return []fantasy.AgentTool{
				tools.NewBashTool(f.permissions, rb.cfg.WorkingDir(), b.cfg.Attribution(), b.modelID, f.background, b.toolAvailability),
				tools.NewGitStatusTool(rb.cfg.WorkingDir()),
				tools.NewGitDiffTool(rb.cfg.WorkingDir()),
				tools.NewGitLogTool(rb.cfg.WorkingDir()),
				tools.NewSennitInfoTool(rb.cfg, rb.mcp, f.lspManager, b.allSkills, b.activeSkills, b.skillTracker, b.inputs.skillStates),
				tools.NewSennitLogsTool(b.logFile),
				tools.NewAgentTraceTool(b.logFile),
				tools.NewJobOutputTool(f.background),
				tools.NewJobKillTool(f.background),
				tools.NewDownloadTool(f.permissions, rb.cfg.WorkingDir(), nil),
				tools.NewEditTool(f.lspManager, f.permissions, f.history, f.filetracker, rb.cfg.WorkingDir()),
				tools.NewMultiEditTool(f.lspManager, f.permissions, f.history, f.filetracker, rb.cfg.WorkingDir()),
				tools.NewFetchTool(f.permissions, rb.cfg.WorkingDir(), nil, b.toolAvailability),
				tools.NewWebFetchTool(f.permissions, rb.cfg.WorkingDir(), nil, b.toolAvailability),
				tools.NewWebSearchTool(f.permissions, rb.cfg.WorkingDir(), nil, b.searchBackend, b.toolAvailability),
				tools.NewGlobTool(rb.cfg.WorkingDir(), b.cfg.Glob()),
				tools.NewSearchTool(rb.cfg.WorkingDir(), b.cfg.Grep()),
				tools.NewLsTool(f.permissions, rb.cfg.WorkingDir(), b.cfg.Ls()),
				tools.NewTodosTool(f.sessions),
				tools.NewReadTool(f.lspManager, f.permissions, f.filetracker, b.skillTracker, rb.cfg.WorkingDir(), b.cfg.SkillsPaths()...),
				tools.NewMultiReadTool(f.lspManager, f.permissions, f.filetracker, b.skillTracker, rb.cfg.WorkingDir(), b.cfg.SkillsPaths()...),
				tools.NewWriteTool(f.lspManager, f.permissions, f.history, f.filetracker, rb.cfg.WorkingDir()),
			}, nil
		}},

		// Thread tools: top-level agent of the workspace owning the thread
		// manager only — sub-agents nesting workspace ownership isn't
		// supported, and non-git/thread-spawned workspaces have no manager.
		{[]string{"thread_create", "thread_list", "thread_status", "thread_send", "thread_merge", "thread_remove"}, func(_ context.Context, rb *runtimeBuilder, b *buildToolsCtx) ([]fantasy.AgentTool, error) {
			return []fantasy.AgentTool{
				tools.NewThreadCreateTool(b.threads, b.inputs.permissions),
				tools.NewThreadListTool(b.threads),
				tools.NewThreadStatusTool(b.threads),
				tools.NewThreadSendTool(b.threads),
				tools.NewThreadMergeTool(b.threads, b.inputs.permissions),
				tools.NewThreadRemoveTool(b.threads, b.inputs.permissions),
			}, nil
		}},

		// Task tools observe/steer background task delegations (see the
		// "agent" tool's background mode). Same restriction as thread
		// tools, plus the explicit options.background_agents opt-out.
		{[]string{"task_list", "task_result", "task_cancel", "task_send", "task_output"}, func(_ context.Context, rb *runtimeBuilder, b *buildToolsCtx) ([]fantasy.AgentTool, error) {
			return []fantasy.AgentTool{
				tools.NewTaskListTool(b.taskManager),
				tools.NewTaskResultTool(b.taskManager),
				tools.NewTaskCancelTool(b.taskManager, b.inputs.permissions),
				tools.NewTaskSendTool(b.taskManager),
				tools.NewTaskOutputTool(b.taskManager),
			}, nil
		}},

		// ask_parent (Coordinator.SendToParent) is registered for every
		// non-sub-agent build, including the parent's own top-level
		// session (a task shares its parent's coordinator/tool list), and
		// gated for real at runtime instead — see withoutUnusableParentTool.
		{[]string{"ask_parent"}, one(func(b *buildToolsCtx) fantasy.AgentTool { return b.inputs.delegationToolsBuilt["ask_parent"] })},

		// Question tool is interactive-only and not available to sub-agents.
		{
			[]string{"question"},
			one(func(b *buildToolsCtx) fantasy.AgentTool {
				return tools.NewQuestionTool(b.inputs.questions)
			}),
		},

		// LSP tools: offered whenever the user configured an LSP
		// explicitly, or auto_lsp is unset/true.
		{[]string{"lsp_diagnostics", "lsp_references", "lsp_restart", "lsp_symbols", "lsp_workspace_symbols", "lsp_hover", "lsp_definition", "lsp_call_hierarchy", "lsp_rename", "lsp_replace_symbol"}, func(_ context.Context, rb *runtimeBuilder, b *buildToolsCtx) ([]fantasy.AgentTool, error) {
			f := b.inputs
			return []fantasy.AgentTool{
				tools.NewDiagnosticsTool(f.lspManager, rb.cfg.WorkingDir()),
				tools.NewReferencesTool(f.lspManager, rb.cfg.WorkingDir()),
				tools.NewLSPRestartTool(f.lspManager),
				tools.NewSymbolsTool(f.lspManager, rb.cfg.WorkingDir()),
				tools.NewWorkspaceSymbolsTool(f.lspManager, rb.cfg.WorkingDir()),
				tools.NewHoverTool(f.lspManager, rb.cfg.WorkingDir()),
				tools.NewDefinitionTool(f.lspManager, rb.cfg.WorkingDir()),
				tools.NewCallHierarchyTool(f.lspManager, rb.cfg.WorkingDir()),
				tools.NewRenameTool(f.lspManager, f.permissions, f.history, f.filetracker, rb.cfg.WorkingDir()),
				tools.NewReplaceSymbolTool(f.lspManager, f.permissions, f.history, f.filetracker, rb.cfg.WorkingDir()),
			}, nil
		}},

		// MCP resource browsing tools: offered whenever at least one MCP
		// server is configured (independent of AllowedMCP, which only
		// gates the per-server tools built outside this table).
		{[]string{"list_mcp_resources", "read_mcp_resource"}, func(_ context.Context, rb *runtimeBuilder, b *buildToolsCtx) ([]fantasy.AgentTool, error) {
			return []fantasy.AgentTool{
				tools.NewListMCPResourcesTool(rb.cfg, rb.mcp, b.inputs.permissions),
				tools.NewReadMCPResourceTool(rb.cfg, rb.mcp, b.inputs.permissions),
			}, nil
		}},
	}
}
