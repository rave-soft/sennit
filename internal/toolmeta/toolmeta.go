// Package toolmeta contains provider-independent metadata for built-in tools.
// It deliberately has no dependency on agent, config, permission, or UI packages.
package toolmeta

type AccessMode uint8

const (
	AccessRead AccessMode = iota
	AccessWrite
	AccessDynamic
)

type BuilderClass uint8

const (
	BuilderToolsPackage BuilderClass = iota
	BuilderCoordinator
)

type SchemaOwner uint8

const (
	SchemaConstructor SchemaOwner = iota
	SchemaDynamic
)

type Gate uint8

const (
	GateAlways Gate = iota
	GateNotSubAgent
	GateThreads
	GateTasks
	GateLSP
	GateMCP
	GateInteractive
	GateAllowed
)

type RendererClass uint8

const (
	RendererGeneric RendererClass = iota
	RendererDedicated
	RendererSpecial
)

type DocsCategory uint8

const (
	DocsInteraction DocsCategory = iota
	DocsShell
	DocsFiles
	DocsSearch
	DocsWeb
	DocsLSP
	DocsMCP
	DocsDelegation
	DocsThreads
)

type Descriptor struct {
	Name                                                         string
	Aliases                                                      []string
	Access                                                       AccessMode
	Builder                                                      BuilderClass
	Schema                                                       SchemaOwner
	Writes, ParallelSafe, Confined, DefaultAllowed, TaskReadOnly bool
	Gate                                                         Gate
	Renderer                                                     RendererClass
	Docs                                                         DocsCategory
}

// descriptors is the canonical static built-in registry. Dynamic MCP tools and
// user-defined agent tools intentionally do not belong here.
var descriptors = []Descriptor{
	{Name: "agent", Renderer: RendererSpecial, Docs: DocsDelegation, Access: AccessWrite, Builder: BuilderCoordinator, Writes: true, ParallelSafe: true, DefaultAllowed: true, Gate: GateAllowed},
	{Name: "bash", Access: AccessDynamic, Writes: true, Confined: true, DefaultAllowed: true, Gate: GateAlways, Renderer: RendererSpecial, Docs: DocsShell},
	{Name: "git_status", Access: AccessRead, ParallelSafe: true, Confined: true, DefaultAllowed: true, TaskReadOnly: true, Gate: GateAlways, Renderer: RendererDedicated, Docs: DocsShell},
	{Name: "git_diff", Access: AccessRead, ParallelSafe: true, Confined: true, DefaultAllowed: true, TaskReadOnly: true, Gate: GateAlways, Renderer: RendererDedicated, Docs: DocsShell},
	{Name: "git_log", Access: AccessRead, ParallelSafe: true, Confined: true, DefaultAllowed: true, TaskReadOnly: true, Gate: GateAlways, Renderer: RendererDedicated, Docs: DocsShell},
	{Name: "sennit_info", Docs: DocsInteraction, Access: AccessRead, ParallelSafe: true, DefaultAllowed: true},
	{Name: "sennit_logs", Docs: DocsInteraction, Access: AccessRead, ParallelSafe: true, DefaultAllowed: true},
	{Name: "agent_trace", Docs: DocsInteraction, Access: AccessRead, ParallelSafe: true, TaskReadOnly: true, DefaultAllowed: true, Gate: GateAlways},
	{Name: "job_output", Renderer: RendererDedicated, Docs: DocsShell, Access: AccessRead, DefaultAllowed: true},
	{Name: "job_kill", Renderer: RendererDedicated, Docs: DocsShell, Access: AccessWrite, DefaultAllowed: true},
	{Name: "download", Renderer: RendererDedicated, Access: AccessWrite, Writes: true, ParallelSafe: true, Confined: true, DefaultAllowed: true, Docs: DocsWeb},
	{Name: "edit", Access: AccessWrite, Writes: true, Confined: true, DefaultAllowed: true, Renderer: RendererSpecial, Docs: DocsFiles},
	{Name: "multiedit", Access: AccessWrite, Writes: true, Confined: true, DefaultAllowed: true, Renderer: RendererSpecial, Docs: DocsFiles},
	{Name: "lsp_diagnostics", Renderer: RendererDedicated, Access: AccessRead, DefaultAllowed: true, Gate: GateLSP, Docs: DocsLSP},
	{Name: "lsp_references", Renderer: RendererDedicated, Access: AccessRead, DefaultAllowed: true, Gate: GateLSP, Docs: DocsLSP},
	{Name: "lsp_restart", Renderer: RendererDedicated, Access: AccessWrite, DefaultAllowed: true, Gate: GateLSP, Docs: DocsLSP},
	{Name: "lsp_symbols", Renderer: RendererDedicated, Access: AccessRead, DefaultAllowed: true, TaskReadOnly: true, Gate: GateLSP, Docs: DocsLSP},
	{Name: "lsp_workspace_symbols", Renderer: RendererDedicated, Access: AccessRead, DefaultAllowed: true, TaskReadOnly: true, Gate: GateLSP, Docs: DocsLSP},
	{Name: "lsp_hover", Renderer: RendererDedicated, Access: AccessRead, DefaultAllowed: true, TaskReadOnly: true, Gate: GateLSP, Docs: DocsLSP},
	{Name: "lsp_definition", Renderer: RendererDedicated, Access: AccessRead, DefaultAllowed: true, TaskReadOnly: true, Gate: GateLSP, Docs: DocsLSP},
	{Name: "lsp_call_hierarchy", Renderer: RendererDedicated, Access: AccessRead, DefaultAllowed: true, TaskReadOnly: true, Gate: GateLSP, Docs: DocsLSP},
	{Name: "lsp_rename", Renderer: RendererDedicated, Access: AccessWrite, Writes: true, Confined: true, DefaultAllowed: true, Gate: GateLSP, Docs: DocsLSP},
	{Name: "lsp_replace_symbol", Renderer: RendererDedicated, Access: AccessWrite, Writes: true, Confined: true, DefaultAllowed: true, Gate: GateLSP, Docs: DocsLSP},
	{Name: "fetch", Renderer: RendererDedicated, Access: AccessRead, Writes: true, ParallelSafe: true, DefaultAllowed: true, TaskReadOnly: true, Gate: GateAlways, Docs: DocsWeb},
	{Name: "agentic_fetch", Access: AccessWrite, Builder: BuilderCoordinator, Writes: true, ParallelSafe: true, DefaultAllowed: true, Gate: GateAllowed, Renderer: RendererSpecial, Docs: DocsWeb},
	{Name: "web_fetch", Renderer: RendererDedicated, Access: AccessRead, Writes: true, ParallelSafe: true, DefaultAllowed: true, TaskReadOnly: true, Docs: DocsWeb},
	{Name: "web_search", Renderer: RendererDedicated, Access: AccessRead, Writes: true, ParallelSafe: true, DefaultAllowed: true, TaskReadOnly: true, Docs: DocsWeb},
	{Name: "glob", Renderer: RendererDedicated, Access: AccessRead, ParallelSafe: true, DefaultAllowed: true, TaskReadOnly: true, Docs: DocsFiles},
	{Name: "grep", Renderer: RendererDedicated, Access: AccessRead, ParallelSafe: true, DefaultAllowed: true, TaskReadOnly: true, Docs: DocsFiles},
	{Name: "ripgrep", Renderer: RendererDedicated, Access: AccessRead, ParallelSafe: true, DefaultAllowed: true, TaskReadOnly: true, Docs: DocsFiles},
	{Name: "ls", Renderer: RendererDedicated, Access: AccessRead, ParallelSafe: true, DefaultAllowed: true, TaskReadOnly: true, Docs: DocsFiles},
	{Name: "question", Renderer: RendererDedicated, Docs: DocsInteraction, Access: AccessWrite, DefaultAllowed: true, Gate: GateInteractive},
	{Name: "todos", Renderer: RendererDedicated, Docs: DocsInteraction, Access: AccessWrite, DefaultAllowed: true},
	{Name: "read", Aliases: []string{"view"}, Access: AccessRead, DefaultAllowed: true, TaskReadOnly: true, Renderer: RendererDedicated, Docs: DocsFiles},
	{Name: "multi_read", Access: AccessRead, DefaultAllowed: true, TaskReadOnly: true, Renderer: RendererGeneric, Docs: DocsFiles},
	{Name: "write", Access: AccessWrite, Writes: true, Confined: true, DefaultAllowed: true, Renderer: RendererSpecial, Docs: DocsFiles},
	{Name: "list_mcp_resources", Access: AccessRead, ParallelSafe: true, DefaultAllowed: true, Gate: GateMCP, Docs: DocsMCP},
	{Name: "read_mcp_resource", Access: AccessRead, ParallelSafe: true, DefaultAllowed: true, Gate: GateMCP, Docs: DocsMCP},
	{Name: "thread_create", Access: AccessWrite, Writes: true, DefaultAllowed: true, Gate: GateThreads, Docs: DocsThreads},
	{Name: "thread_list", Access: AccessRead, DefaultAllowed: true, Gate: GateThreads, Docs: DocsThreads},
	{Name: "thread_status", Access: AccessRead, DefaultAllowed: true, Gate: GateThreads, Docs: DocsThreads},
	{Name: "thread_send", Access: AccessWrite, Gate: GateThreads, Docs: DocsThreads},
	{Name: "thread_merge", Access: AccessWrite, Writes: true, DefaultAllowed: true, Gate: GateThreads, Docs: DocsThreads},
	{Name: "thread_remove", Access: AccessWrite, Writes: true, DefaultAllowed: true, Gate: GateThreads, Docs: DocsThreads},
	{Name: "task_list", Access: AccessRead, DefaultAllowed: true, Gate: GateTasks, Renderer: RendererDedicated, Docs: DocsDelegation},
	{Name: "task_result", Access: AccessRead, DefaultAllowed: true, Gate: GateTasks, Renderer: RendererDedicated, Docs: DocsDelegation},
	{Name: "task_cancel", Access: AccessWrite, DefaultAllowed: true, Gate: GateTasks, Renderer: RendererDedicated, Docs: DocsDelegation},
	{Name: "task_send", Access: AccessWrite, DefaultAllowed: true, Gate: GateTasks, Renderer: RendererDedicated, Docs: DocsDelegation},
	{Name: "task_output", Access: AccessRead, DefaultAllowed: true, Gate: GateTasks, Renderer: RendererDedicated, Docs: DocsDelegation},
	{Name: "ask_parent", Access: AccessWrite, DefaultAllowed: true, Gate: GateNotSubAgent, Docs: DocsDelegation},
}

func clone(d Descriptor) Descriptor { d.Aliases = append([]string(nil), d.Aliases...); return d }
func Builtins() []Descriptor {
	out := make([]Descriptor, len(descriptors))
	for i, d := range descriptors {
		out[i] = clone(d)
	}
	return out
}

func Lookup(name string) (Descriptor, bool) {
	for _, d := range descriptors {
		if d.Name == name {
			return clone(d), true
		}
		for _, a := range d.Aliases {
			if a == name {
				return clone(d), true
			}
		}
	}
	return Descriptor{}, false
}

func CanonicalName(name string) string {
	if d, ok := Lookup(name); ok {
		return d.Name
	}
	return name
}

func names(match func(Descriptor) bool) []string {
	out := make([]string, 0, len(descriptors))
	for _, d := range descriptors {
		if match(d) {
			out = append(out, d.Name)
		}
	}
	return out
}
func NamesAll() []string     { return names(func(Descriptor) bool { return true }) }
func DefaultNames() []string { return names(func(d Descriptor) bool { return d.DefaultAllowed }) }

func TaskReadOnlyNames() []string { return names(func(d Descriptor) bool { return d.TaskReadOnly }) }

func AliasNames() []string {
	var out []string
	for _, d := range descriptors {
		out = append(out, d.Aliases...)
	}
	return out
}

func init() {
	owners := make(map[string]string, len(descriptors))
	for _, d := range descriptors {
		if d.Name == "" {
			panic("toolmeta: descriptor has an empty name")
		}
		for _, name := range append([]string{d.Name}, d.Aliases...) {
			if owner, exists := owners[name]; exists {
				panic("toolmeta: name " + name + " is owned by both " + owner + " and " + d.Name)
			}
			owners[name] = d.Name
		}
	}
}
