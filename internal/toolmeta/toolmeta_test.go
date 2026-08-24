package toolmeta

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegistryIntegrityAndDefensiveCopies(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range Builtins() {
		if d.Name == "" || seen[d.Name] {
			t.Fatalf("invalid or duplicate descriptor %q", d.Name)
		}
		seen[d.Name] = true
		if got, ok := Lookup(d.Name); !ok || got.Name != d.Name {
			t.Fatalf("lookup %q failed", d.Name)
		}
		for _, alias := range d.Aliases {
			if CanonicalName(alias) != d.Name {
				t.Fatalf("alias %q does not resolve to %q", alias, d.Name)
			}
		}
	}
	first := Builtins()
	first[0].Name = "mutated"
	first[0].Aliases = append(first[0].Aliases, "mutated")
	if Builtins()[0].Name == "mutated" || slices.Contains(Builtins()[0].Aliases, "mutated") {
		t.Fatal("Builtins returned mutable registry data")
	}
	read, ok := Lookup("view")
	if !ok {
		t.Fatal("view alias not found")
	}
	read.Aliases[0] = "mutated"
	if got, _ := Lookup("read"); got.Aliases[0] == "mutated" {
		t.Fatal("Lookup returned mutable alias data")
	}
}

func TestFrozenAccessAndGateClassifications(t *testing.T) {
	wantAccess := map[AccessMode][]string{
		AccessDynamic: {"bash"},
		AccessRead: {
			"sennit_info", "sennit_logs", "job_output", "lsp_diagnostics", "lsp_references", "lsp_symbols",
			"lsp_definition", "lsp_call_hierarchy", "fetch", "web_fetch", "web_search", "glob", "grep",
			"ripgrep", "ls", "read", "multi_read", "list_mcp_resources", "read_mcp_resource", "thread_list",
			"thread_status", "thread_wait", "task_list", "task_result", "task_output",
		},
		AccessWrite: {
			"agent", "job_kill", "download", "edit", "multiedit", "lsp_restart", "lsp_rename",
			"lsp_replace_symbol", "agentic_fetch", "question", "todos", "write", "thread_create",
			"thread_send", "thread_merge", "thread_remove", "task_cancel", "task_send", "ask_parent",
		},
	}
	wantGate := map[Gate][]string{
		GateAlways: {
			"bash", "sennit_info", "sennit_logs", "job_output", "job_kill", "download", "edit", "multiedit",
			"fetch", "web_fetch", "web_search", "glob", "grep", "ripgrep", "ls", "todos", "read", "multi_read", "write",
		},
		GateAllowed:     {"agent", "agentic_fetch"},
		GateNotSubAgent: {"ask_parent"},
		GateThreads:     {"thread_create", "thread_list", "thread_status", "thread_send", "thread_wait", "thread_merge", "thread_remove"},
		GateTasks:       {"task_list", "task_result", "task_cancel", "task_send", "task_output"},
		GateLSP:         {"lsp_diagnostics", "lsp_references", "lsp_restart", "lsp_symbols", "lsp_definition", "lsp_call_hierarchy", "lsp_rename", "lsp_replace_symbol"},
		GateMCP:         {"list_mcp_resources", "read_mcp_resource"},
		GateInteractive: {"question"},
	}
	seen := make(map[string]bool)
	for _, descriptor := range Builtins() {
		require.Contains(t, wantAccess[descriptor.Access], descriptor.Name, "access classification changed for %q", descriptor.Name)
		require.Contains(t, wantGate[descriptor.Gate], descriptor.Name, "gate classification changed for %q", descriptor.Name)
		require.Equal(t, descriptor.Name == "bash", descriptor.Access == AccessDynamic, "only bash has call-dependent access")
		require.Equal(t, SchemaConstructor, descriptor.Schema, "built-in schemas stay owned by constructors")
		seen[descriptor.Name] = true
	}
	for _, groups := range []map[AccessMode][]string{wantAccess} {
		for _, names := range groups {
			for _, name := range names {
				require.Truef(t, seen[name], "frozen access classification contains unknown tool %q", name)
			}
		}
	}
	for _, names := range wantGate {
		for _, name := range names {
			require.Truef(t, seen[name], "frozen gate classification contains unknown tool %q", name)
		}
	}
}

func TestConfiguredSetsAreExact(t *testing.T) {
	wantDefault := []string{"agent", "bash", "sennit_info", "sennit_logs", "job_output", "job_kill", "download", "edit", "multiedit", "lsp_diagnostics", "lsp_references", "lsp_restart", "lsp_symbols", "lsp_definition", "lsp_call_hierarchy", "lsp_rename", "lsp_replace_symbol", "fetch", "agentic_fetch", "web_fetch", "web_search", "glob", "grep", "ripgrep", "ls", "question", "todos", "read", "multi_read", "write", "list_mcp_resources", "read_mcp_resource", "thread_create", "thread_list", "thread_status", "thread_wait", "thread_merge", "thread_remove", "task_list", "task_result", "task_cancel", "task_send", "task_output", "ask_parent"}
	if !slices.Equal(DefaultNames(), wantDefault) {
		t.Fatalf("defaults = %v", DefaultNames())
	}
	wantReadOnly := []string{"lsp_symbols", "lsp_definition", "lsp_call_hierarchy", "fetch", "web_fetch", "web_search", "glob", "grep", "ripgrep", "ls", "read", "multi_read"}
	if !slices.Equal(TaskReadOnlyNames(), wantReadOnly) {
		t.Fatalf("task read-only = %v", TaskReadOnlyNames())
	}
}

func TestDocsReferenceToolsParity(t *testing.T) {
	body, err := os.ReadFile("../../docs/reference/tools.md")
	if err != nil {
		t.Fatal(err)
	}
	headings := map[string]DocsCategory{"Files": DocsFiles, "Shell": DocsShell, "Language servers": DocsLSP, "Network": DocsWeb, "Delegation": DocsDelegation, "Threads": DocsThreads, "MCP": DocsMCP, "Interaction and state": DocsInteraction}
	section := ""
	documented := map[string]DocsCategory{}
	tool := regexp.MustCompile("`([a-z][a-z0-9_]*)`")
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "## ") {
			section = strings.TrimPrefix(line, "## ")
			continue
		}
		category, ok := headings[section]
		if !ok || !strings.HasPrefix(line, "| `") {
			continue
		}
		if match := tool.FindStringSubmatch(line); match != nil {
			if previous, exists := documented[match[1]]; exists {
				t.Errorf("docs tool %q is documented more than once (%v and %v)", match[1], previous, category)
			}
			documented[match[1]] = category
		}
	}
	canonical := NamesAll()
	for _, d := range Builtins() {
		got, ok := documented[d.Name]
		if !ok {
			t.Errorf("metadata tool %q is missing from docs/reference/tools.md", d.Name)
			continue
		}
		if got != d.Docs {
			t.Errorf("tool %q docs category = %v, want %v", d.Name, got, d.Docs)
		}
	}
	for name := range documented {
		if !slices.Contains(canonical, name) {
			t.Errorf("docs tool %q has no canonical metadata descriptor", name)
		}
	}
}
