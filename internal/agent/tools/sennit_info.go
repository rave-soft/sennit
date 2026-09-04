package tools

import (
	"context"
	_ "embed"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/doctor"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/skills"
)

const SennitInfoToolName = brand.ToolInfo

//go:embed sennit_info.md
var sennitInfoDescription string

type SennitInfoParams struct {
	ModelsFor string `json:"models_for,omitempty" description:"Provider ID (e.g. \"anthropic\", \"xl0.ru\") to list that provider's available model IDs instead of the full state dump. Use this to verify a model ID is real before writing it into an agent file or model config."`
	// ModelFilter narrows models_for's output to IDs containing this
	// substring (case-insensitive). Without it, a router provider backed
	// by a large model-discovery catalog silently caps at modelsForCap,
	// so an ID past the cap reads as "not found" even though it exists.
	ModelFilter string `json:"model_filter,omitempty" description:"Only used with models_for: case-insensitive substring to narrow the model ID list. Use this to find a specific ID in a provider with more than 50 models, where the unfiltered list is capped."`
}

// SennitInfoConfig is the slice of *config.ConfigStore this tool needs: the
// dictionary read, the runtime overrides, the loaded config paths, and the
// staleness snapshot. Declaring it here rather than accepting the concrete
// *config.ConfigStore keeps this tool's dependency on config narrow (ISP).
type SennitInfoConfig interface {
	Config() *config.Config
	Overrides() config.RuntimeOverrides
	LoadedPaths() []string
	ConfigStaleness() config.StalenessResult
}

var _ SennitInfoConfig = (*config.ConfigStore)(nil)

func NewSennitInfoTool(
	cfg SennitInfoConfig,
	reg *mcp.Registry,
	lspManager *lsp.Manager,
	allSkills []*skills.Skill,
	activeSkills []*skills.Skill,
	skillTracker *skills.Tracker,
	skillStates []*skills.SkillState,
	environmentProblems ...func() []config.Problem,
) fantasy.AgentTool {
	collectEnvironmentProblems := doctor.EnvironmentProblems
	if len(environmentProblems) > 0 {
		collectEnvironmentProblems = environmentProblems[0]
	}
	return fantasy.NewParallelAgentTool(
		SennitInfoToolName,
		sennitInfoDescription,
		func(ctx context.Context, params SennitInfoParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ModelsFor != "" {
				return fantasy.NewTextResponse(buildModelsFor(cfg, params.ModelsFor, params.ModelFilter)), nil
			}
			return fantasy.NewTextResponse(buildSennitInfo(cfg, reg, lspManager, allSkills, activeSkills, skillTracker, skillStates, collectEnvironmentProblems)), nil
		},
	)
}

// modelsForCap bounds how many model IDs buildModelsFor lists directly.
// Router providers backed by the model-discovery cache (see
// internal/modelcache) can carry thousands of models; dumping all
// of them defeats the point of a quick "does this ID exist" check.
const modelsForCap = 50

// buildModelsFor renders just the model list for one provider so an agent
// configuring subagents/skills can check a model ID is real — via
// sennit_info{"models_for": "..."} — before writing provider/model-id into
// an agent file, instead of guessing. The full sennit_info dump only reports
// a per-provider count ([providers]), not the IDs themselves.
func buildModelsFor(cfg SennitInfoConfig, providerID, filter string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[models_for.%s]\n", providerID)

	pc, ok := cfg.Config().Providers.Get(providerID)
	if !ok || pc.Disable {
		b.WriteString("error = provider not found or disabled\n")
		return b.String()
	}

	ids := make([]string, 0, len(pc.Models))
	for _, m := range pc.Models {
		if filter != "" && !strings.Contains(strings.ToLower(m.ID), strings.ToLower(filter)) {
			continue
		}
		ids = append(ids, m.ID)
	}
	slices.Sort(ids)

	if len(ids) == 0 {
		if filter != "" {
			fmt.Fprintf(&b, "(no model IDs match filter %q)\n", filter)
		} else {
			b.WriteString("(no models configured)\n")
		}
		return b.String()
	}

	shown := ids
	var truncated int
	if len(ids) > modelsForCap {
		shown = ids[:modelsForCap]
		truncated = len(ids) - modelsForCap
	}
	for _, id := range shown {
		b.WriteString(id + "\n")
	}
	if truncated > 0 {
		fmt.Fprintf(&b, "...and %d more\n", truncated)
	}
	return b.String()
}

func buildSennitInfo(cfg SennitInfoConfig, reg *mcp.Registry, lspManager *lsp.Manager, allSkills []*skills.Skill, activeSkills []*skills.Skill, skillTracker *skills.Tracker, skillStates []*skills.SkillState, environmentProblems ...func() []config.Problem) string {
	collectEnvironmentProblems := doctor.EnvironmentProblems
	if len(environmentProblems) > 0 {
		collectEnvironmentProblems = environmentProblems[0]
	}
	var b strings.Builder

	var mcpStates map[string]mcp.ClientInfo
	if reg != nil {
		mcpStates = reg.GetStates()
	}

	writeConfigFiles(&b, cfg)
	writeConfigStaleness(&b, cfg)
	writeProblems(&b, cfg, mcpStates, skillStates, collectEnvironmentProblems)
	writeModels(&b, cfg)
	writeProviders(&b, cfg)
	writeLSP(&b, lspManager, cfg)
	writeMCP(&b, mcpStates, cfg)
	writeSkills(&b, allSkills, activeSkills, skillTracker, cfg)
	writeHooks(&b, cfg)
	writePermissions(&b, cfg)
	writeDisabledTools(&b, cfg)
	writeOptions(&b, cfg)
	writeAttribution(&b, cfg)

	return b.String()
}

func writeConfigFiles(b *strings.Builder, cfg SennitInfoConfig) {
	b.WriteString("[config_files]\n")
	paths := cfg.LoadedPaths()
	for _, p := range paths {
		b.WriteString(p + "\n")
	}
	b.WriteString("\n")
}

func writeConfigStaleness(b *strings.Builder, cfg SennitInfoConfig) {
	staleness := cfg.ConfigStaleness()

	b.WriteString("[config]\n")
	fmt.Fprintf(b, "dirty = %v\n", staleness.Dirty)

	if len(staleness.Changed) > 0 {
		sorted := slices.Clone(staleness.Changed)
		slices.Sort(sorted)
		fmt.Fprintf(b, "changed_paths = %s\n", strings.Join(sorted, ", "))
	}

	if len(staleness.Missing) > 0 {
		sorted := slices.Clone(staleness.Missing)
		slices.Sort(sorted)
		fmt.Fprintf(b, "missing_paths = %s\n", strings.Join(sorted, ", "))
	}

	if len(staleness.Errors) > 0 {
		var paths []string
		for path := range staleness.Errors {
			paths = append(paths, path)
		}
		slices.Sort(paths)
		fmt.Fprintf(b, "errors = %s\n", strings.Join(paths, ", "))
	}

	b.WriteString("\n")
}

// writeProblems reports config.Doctor's static findings, any MCP server
// stuck in an error/needs-auth state, and any SKILL.md that failed to
// parse or validate — so an agent asked "why is my sub-agent on the wrong
// model?" (or "why can't I use that MCP tool?", or "why did the skill I
// was told to follow do nothing?") can answer from its own sennit_info
// output instead of a log file it never sees.
func writeProblems(b *strings.Builder, cfg SennitInfoConfig, mcpStates map[string]mcp.ClientInfo, skillStates []*skills.SkillState, environmentProblems ...func() []config.Problem) {
	collectEnvironmentProblems := doctor.EnvironmentProblems
	if len(environmentProblems) > 0 {
		collectEnvironmentProblems = environmentProblems[0]
	}
	problems := config.Doctor(cfg.Config())
	problems = append(problems, collectEnvironmentProblems()...)
	problems = append(problems, doctor.SkillProblems(skillStates)...)
	for name, info := range mcpStates {
		if info.State != mcp.StateError && info.State != mcp.StateNeedsAuth {
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
	if len(problems) == 0 {
		return
	}

	slices.SortFunc(problems, func(a, b config.Problem) int {
		if c := strings.Compare(string(a.Area), string(b.Area)); c != 0 {
			return c
		}
		return strings.Compare(a.Subject, b.Subject)
	})
	b.WriteString("[problems]\n")
	for _, p := range problems {
		fmt.Fprintf(b, "%s.%s = %s: %s\n", p.Area, p.Subject, p.Severity, p.Message)
	}
	b.WriteString("\n")
}

func writeModels(b *strings.Builder, cfg SennitInfoConfig) {
	c := cfg.Config()
	if c.Model.Model == "" {
		return
	}
	b.WriteString("[model]\n")
	fmt.Fprintf(b, "model = %s (%s)\n", c.Model.Model, c.Model.Provider)
	b.WriteString("\n")
}

func writeProviders(b *strings.Builder, cfg SennitInfoConfig) {
	c := cfg.Config()
	type pv struct {
		name  string
		count int
	}
	var providers []pv
	for name, pc := range c.Providers.Seq2() {
		if pc.Disable {
			continue
		}
		providers = append(providers, pv{name: name, count: len(pc.Models)})
	}
	if len(providers) == 0 {
		return
	}
	slices.SortFunc(providers, func(a, b pv) int { return strings.Compare(a.name, b.name) })
	b.WriteString("[providers]\n")
	for _, p := range providers {
		fmt.Fprintf(b, "%s = enabled (%d models)\n", p.name, p.count)
	}
	b.WriteString("\n")
}

func writeLSP(b *strings.Builder, lspManager *lsp.Manager, cfg SennitInfoConfig) {
	// Write runtime LSP clients
	if lspManager != nil && lspManager.Clients().Len() > 0 {
		type entry struct {
			name      string
			state     lsp.ServerState
			fileTypes []string
		}
		var entries []entry
		for name, client := range lspManager.Clients().Seq2() {
			entries = append(entries, entry{
				name:      name,
				state:     client.GetServerState(),
				fileTypes: client.FileTypes(),
			})
		}
		if len(entries) > 0 {
			slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.name, b.name) })
			b.WriteString("[lsp]\n")
			for _, e := range entries {
				stateStr := lspStateString(e.state)
				if len(e.fileTypes) > 0 {
					sorted := slices.Clone(e.fileTypes)
					slices.Sort(sorted)
					fmt.Fprintf(b, "%s = %s (%s)\n", e.name, stateStr, strings.Join(sorted, ", "))
				} else {
					fmt.Fprintf(b, "%s = %s\n", e.name, stateStr)
				}
			}
			b.WriteString("\n")
		}
	}

	// Write configured but not running LSP servers
	c := cfg.Config()
	if len(c.LSP) > 0 {
		runtimeNames := make(map[string]bool)
		if lspManager != nil {
			for name := range lspManager.Clients().Seq2() {
				runtimeNames[name] = true
			}
		}

		type configuredEntry struct {
			name   string
			status string
		}
		var entries []configuredEntry
		for name, lspCfg := range c.LSP {
			// Skip if already in runtime
			if runtimeNames[name] {
				continue
			}
			status := "not_started"
			if lspCfg.Disabled {
				status = "disabled"
			}
			entries = append(entries, configuredEntry{name: name, status: status})
		}

		if len(entries) > 0 {
			slices.SortFunc(entries, func(a, b configuredEntry) int { return strings.Compare(a.name, b.name) })
			b.WriteString("[lsp_configured]\n")
			for _, e := range entries {
				fmt.Fprintf(b, "%s = %s\n", e.name, e.status)
			}
			b.WriteString("\n")
		}
	}
}

func writeMCP(b *strings.Builder, states map[string]mcp.ClientInfo, cfg SennitInfoConfig) {
	// Write runtime MCP states
	if len(states) > 0 {
		type entry struct {
			name        string
			state       mcp.State
			err         error
			tools       int
			resources   int
			connectedAt string
		}
		var entries []entry
		for name, info := range states {
			e := entry{
				name:  name,
				state: info.State,
				err:   info.Error,
			}
			if info.State == mcp.StateConnected {
				e.tools = info.Counts.Tools
				e.resources = info.Counts.Resources
				if !info.ConnectedAt.IsZero() {
					e.connectedAt = info.ConnectedAt.Format("15:04:05")
				}
			}
			entries = append(entries, e)
		}
		slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.name, b.name) })
		b.WriteString("[mcp]\n")
		for _, e := range entries {
			switch e.state {
			case mcp.StateConnected:
				if e.connectedAt != "" {
					fmt.Fprintf(b, "%s = connected (%d tools, %d resources) since %s\n", e.name, e.tools, e.resources, e.connectedAt)
				} else {
					fmt.Fprintf(b, "%s = connected (%d tools, %d resources)\n", e.name, e.tools, e.resources)
				}
			case mcp.StateError:
				if e.err != nil {
					fmt.Fprintf(b, "%s = error: %s\n", e.name, e.err.Error())
				} else {
					fmt.Fprintf(b, "%s = error\n", e.name)
				}
			default:
				fmt.Fprintf(b, "%s = %s\n", e.name, e.state)
			}
		}
		b.WriteString("\n")
	}

	// Write configured but not running MCP servers
	c := cfg.Config()
	if len(c.MCP) > 0 {
		runtimeNames := make(map[string]bool)
		for name := range states {
			runtimeNames[name] = true
		}

		type configuredEntry struct {
			name   string
			status string
		}
		var entries []configuredEntry
		for name, mcpCfg := range c.MCP {
			// Skip if already in runtime
			if runtimeNames[name] {
				continue
			}
			status := "not_started"
			if mcpCfg.Disabled {
				status = "disabled"
			}
			entries = append(entries, configuredEntry{name: name, status: status})
		}

		if len(entries) > 0 {
			slices.SortFunc(entries, func(a, b configuredEntry) int { return strings.Compare(a.name, b.name) })
			b.WriteString("[mcp_configured]\n")
			for _, e := range entries {
				fmt.Fprintf(b, "%s = %s\n", e.name, e.status)
			}
			b.WriteString("\n")
		}
	}
}

func writeSkills(b *strings.Builder, allSkills []*skills.Skill, activeSkills []*skills.Skill, tracker *skills.Tracker, cfg SennitInfoConfig) {
	var disabled []string
	if cfg.Config().Options != nil {
		disabled = cfg.Config().Options.DisabledSkills
	}
	if len(activeSkills) == 0 && len(disabled) == 0 {
		return
	}

	// Build origin map from the pre-filter list.
	originMap := make(map[string]string, len(allSkills))
	for _, s := range allSkills {
		if s.Builtin {
			originMap[s.Name] = "builtin"
		} else {
			originMap[s.Name] = "user"
		}
	}

	type entry struct {
		name   string
		origin string
		state  string
	}
	var entries []entry

	// Active skills: loaded or unloaded.
	for _, s := range activeSkills {
		state := "unloaded"
		if tracker.IsLoaded(s.Name) {
			state = "loaded"
		}
		origin := originMap[s.Name]
		entries = append(entries, entry{name: s.Name, origin: origin, state: state})
	}

	// Disabled skills.
	for _, name := range disabled {
		origin := originMap[name]
		if origin == "" {
			origin = "user"
		}
		entries = append(entries, entry{name: name, origin: origin, state: "disabled"})
	}

	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.name, b.name) })
	b.WriteString("[skills]\n")
	fmt.Fprintf(b, "loaded_this_session = %d/%d\n", tracker.LoadedCount(), len(activeSkills))
	for _, e := range entries {
		fmt.Fprintf(b, "%s = %s, %s\n", e.name, e.origin, e.state)
	}
	b.WriteString("\n")
}

func writePermissions(b *strings.Builder, cfg SennitInfoConfig) {
	c := cfg.Config()
	overrides := cfg.Overrides()

	if c.Permissions == nil {
		if !overrides.SkipPermissionRequests {
			return
		}
	} else if !overrides.SkipPermissionRequests && len(c.Permissions.AllowedTools) == 0 {
		return
	}
	b.WriteString("[permissions]\n")
	if overrides.SkipPermissionRequests {
		b.WriteString("mode = yolo\n")
	}
	if c.Permissions != nil && len(c.Permissions.AllowedTools) > 0 {
		sorted := slices.Clone(c.Permissions.AllowedTools)
		slices.Sort(sorted)
		fmt.Fprintf(b, "allowed_tools = %s\n", strings.Join(sorted, ", "))
	}
	b.WriteString("\n")
}

func writeDisabledTools(b *strings.Builder, cfg SennitInfoConfig) {
	c := cfg.Config()
	if c.Options == nil || len(c.Options.DisabledTools) == 0 {
		return
	}
	sorted := slices.Clone(c.Options.DisabledTools)
	slices.Sort(sorted)
	b.WriteString("[tools]\n")
	fmt.Fprintf(b, "disabled = %s\n", strings.Join(sorted, ", "))
	b.WriteString("\n")
}

func writeOptions(b *strings.Builder, cfg SennitInfoConfig) {
	c := cfg.Config()
	if c.Options == nil {
		return
	}
	type kv struct {
		key   string
		value string
	}
	var opts []kv

	opts = append(opts, kv{"data_directory", c.Options.DataDirectory})
	opts = append(opts, kv{"debug", fmt.Sprintf("%v", c.Options.Debug)})
	autoLSP := c.Options.AutoLSP == nil || *c.Options.AutoLSP
	opts = append(opts, kv{"auto_lsp", fmt.Sprintf("%v", autoLSP)})
	autoSummarize := !c.Options.DisableAutoSummarize
	opts = append(opts, kv{"auto_summarize", fmt.Sprintf("%v", autoSummarize)})
	if autoSummarize && c.Options.AutoSummarizeAt > 0 {
		opts = append(opts, kv{"auto_summarize_at", strconv.FormatInt(c.Options.AutoSummarizeAt, 10)})
	}
	// The idle pass's thresholds are only worth reporting when it can
	// actually fire: with auto-summarize off, or the pass itself off,
	// they describe nothing that happens.
	idle := c.Options.AutoSummarizeIdle
	if autoSummarize {
		opts = append(opts, kv{"auto_summarize_idle", fmt.Sprintf("%v", idle.IsEnabled())})
		if idle.IsEnabled() {
			opts = append(opts, kv{"auto_summarize_idle_context_tokens", strconv.FormatInt(idle.EffectiveContextTokens(), 10)})
			opts = append(opts, kv{"auto_summarize_idle_after", idle.EffectiveAfter().String()})
		}
	}

	slices.SortFunc(opts, func(a, b kv) int { return strings.Compare(a.key, b.key) })
	b.WriteString("[options]\n")
	for _, o := range opts {
		fmt.Fprintf(b, "%s = %s\n", o.key, o.value)
	}
	b.WriteString("\n")
}

func writeAttribution(b *strings.Builder, cfg SennitInfoConfig) {
	c := cfg.Config()
	if c.Options == nil || c.Options.Attribution == nil {
		return
	}
	b.WriteString("[attribution]\n")
	trailerStyle := c.Options.Attribution.TrailerStyle
	if trailerStyle == "" {
		trailerStyle = config.TrailerStyleAssistedBy
	}
	fmt.Fprintf(b, "trailer_style = %s\n", trailerStyle)
	fmt.Fprintf(b, "generated_with = %v\n", c.Options.Attribution.GeneratedWith)
	b.WriteString("\n")
}

func writeHooks(b *strings.Builder, cfg SennitInfoConfig) {
	c := cfg.Config()
	if len(c.Hooks) == 0 {
		return
	}

	type entry struct {
		event   string
		matcher string
		command string
		timeout int
	}
	var entries []entry
	for event, hookList := range c.Hooks {
		for _, h := range hookList {
			entries = append(entries, entry{
				event:   event,
				matcher: h.Matcher,
				command: h.Command,
				timeout: h.Timeout,
			})
		}
	}
	slices.SortFunc(entries, func(a, b entry) int {
		if a.event != b.event {
			return strings.Compare(a.event, b.event)
		}
		return strings.Compare(a.command, b.command)
	})

	b.WriteString("[hooks]\n")
	for _, e := range entries {
		line := fmt.Sprintf("%s = %s", e.event, e.command)
		if e.matcher != "" {
			line = fmt.Sprintf("%s (matcher: %s) = %s", e.event, e.matcher, e.command)
		}
		if e.timeout > 0 && e.timeout != 30 {
			line += fmt.Sprintf(" (timeout: %ds)", e.timeout)
		}
		b.WriteString(line + "\n")
	}

	b.WriteString("\n")
}

func lspStateString(state lsp.ServerState) string {
	switch state {
	case lsp.StateUnstarted:
		return "unstarted"
	case lsp.StateStarting:
		return "starting"
	case lsp.StateReady:
		return "ready"
	case lsp.StateError:
		return "error"
	case lsp.StateStopped:
		return "stopped"
	case lsp.StateDisabled:
		return "disabled"
	default:
		return "unknown"
	}
}
