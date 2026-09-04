package tools

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/stretchr/testify/require"
)

func TestSennitInfo_MinimalConfig(t *testing.T) {
	t.Parallel()

	cfg := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})
	output := buildSennitInfo(cfg, nil, nil, nil, nil, nil, nil)
	require.NotContains(t, output, "[providers]")
	require.NotContains(t, output, "[lsp]")
	require.NotContains(t, output, "[mcp]")
	require.NotContains(t, output, "[permissions]")
	require.NotContains(t, output, "[tools]")
}

func TestSennitInfo_ConfigFiles(t *testing.T) {
	t.Parallel()

	cfg := configtest.NewStore(t,
		&config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()},
		configtest.WithLoadedPaths(
			"/home/user/.config/sennit/sennit.json",
			"/project/.sennit/sennit.json",
		),
	)
	output := buildSennitInfo(cfg, nil, nil, nil, nil, nil, nil)
	require.Contains(t, output, "[config_files]")
	require.Contains(t, output, "/home/user/.config/sennit/sennit.json")
	require.Contains(t, output, "/project/.sennit/sennit.json")
}

func TestSennitInfo_Models(t *testing.T) {
	t.Parallel()

	cfg := configtest.NewStore(t, &config.Config{
		Model:     config.SelectedModel{Model: "claude-sonnet-4-20250514", Provider: "anthropic"},
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})
	output := buildSennitInfo(cfg, nil, nil, nil, nil, nil, nil)
	require.Contains(t, output, "[model]")
	require.Contains(t, output, "model = claude-sonnet-4-20250514 (anthropic)")
}

func TestSennitInfo_Providers(t *testing.T) {
	t.Parallel()

	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("openai", config.ProviderConfig{Models: make([]catwalk.Model, 8)})
	providers.Set("anthropic", config.ProviderConfig{Models: make([]catwalk.Model, 12)})

	cfg := configtest.NewStore(t, &config.Config{Providers: providers})
	output := buildSennitInfo(cfg, nil, nil, nil, nil, nil, nil)
	require.Contains(t, output, "[providers]")
	anthropicIdx := strings.Index(output, "anthropic = enabled")
	openaiIdx := strings.Index(output, "openai = enabled")
	require.Greater(t, anthropicIdx, -1)
	require.Greater(t, openaiIdx, -1)
	require.Less(t, anthropicIdx, openaiIdx, "anthropic should appear before openai")
	require.Contains(t, output, "anthropic = enabled (12 models)")
	require.Contains(t, output, "openai = enabled (8 models)")
}

func TestBuildModelsFor_ListsIDs(t *testing.T) {
	t.Parallel()

	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("anthropic", config.ProviderConfig{Models: []catwalk.Model{
		{ID: "claude-opus-4"},
		{ID: "claude-sonnet-4"},
	}})

	cfg := configtest.NewStore(t, &config.Config{Providers: providers})
	output := buildModelsFor(cfg, "anthropic", "")
	require.Contains(t, output, "[models_for.anthropic]")
	require.Contains(t, output, "claude-opus-4")
	require.Contains(t, output, "claude-sonnet-4")
	require.NotContains(t, output, "more")
}

func TestBuildModelsFor_UnknownProvider(t *testing.T) {
	t.Parallel()

	cfg := configtest.NewStore(t, &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()})
	output := buildModelsFor(cfg, "does-not-exist", "")
	require.Contains(t, output, "[models_for.does-not-exist]")
	require.Contains(t, output, "error = provider not found or disabled")
}

func TestBuildModelsFor_CapsLargeRouterCatalog(t *testing.T) {
	t.Parallel()

	models := make([]catwalk.Model, 0, 1239)
	for i := range 1239 {
		models = append(models, catwalk.Model{ID: fmt.Sprintf("model-%04d", i)})
	}
	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("omniroute", config.ProviderConfig{Models: models})

	cfg := configtest.NewStore(t, &config.Config{Providers: providers})
	output := buildModelsFor(cfg, "omniroute", "")
	require.Contains(t, output, "[models_for.omniroute]")
	require.Contains(t, output, "...and 1189 more")
	require.Equal(t, modelsForCap, strings.Count(output, "model-"))
}

// TestBuildModelsFor_FilterFindsIDPastTheCap is the regression test for
// finding J: sennit_info.md advised using models_for to verify a model ID
// exists, but the list was capped at modelsForCap with no filter and no
// way to page — for a router provider with thousands of models, an ID
// past the cap was unreachable and read as "not found" even though it was
// configured. model_filter must find it regardless of position.
func TestBuildModelsFor_FilterFindsIDPastTheCap(t *testing.T) {
	t.Parallel()

	models := make([]catwalk.Model, 0, 1239)
	for i := range 1239 {
		models = append(models, catwalk.Model{ID: fmt.Sprintf("model-%04d", i)})
	}
	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("omniroute", config.ProviderConfig{Models: models})
	cfg := configtest.NewStore(t, &config.Config{Providers: providers})

	// model-1200 sorts well past modelsForCap (50), so an unfiltered call
	// would never show it.
	output := buildModelsFor(cfg, "omniroute", "1200")
	require.Contains(t, output, "model-1200")
	require.NotContains(t, output, "more", "a filtered result small enough to fit must not claim truncation")

	empty := buildModelsFor(cfg, "omniroute", "no-such-id")
	require.Contains(t, empty, `no model IDs match filter "no-such-id"`)
}

func TestSennitInfo_DisabledProvidersOmitted(t *testing.T) {
	t.Parallel()

	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("openai", config.ProviderConfig{Disable: true, Models: make([]catwalk.Model, 8)})
	providers.Set("anthropic", config.ProviderConfig{Models: make([]catwalk.Model, 12)})

	cfg := configtest.NewStore(t, &config.Config{Providers: providers})
	output := buildSennitInfo(cfg, nil, nil, nil, nil, nil, nil)
	require.Contains(t, output, "anthropic = enabled")
	require.NotContains(t, output, "openai")
}

func TestSennitInfo_LSPStates(t *testing.T) {
	t.Parallel()

	mgr := lsp.NewManager(configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	}))
	readyClient := &lsp.Client{}
	readyClient.SetServerState(lsp.StateReady)
	mgr.Clients().Set("gopls", readyClient)

	errorClient := &lsp.Client{}
	errorClient.SetServerState(lsp.StateError)
	mgr.Clients().Set("pyright", errorClient)

	cfg := configtest.NewStore(t, &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()})
	output := buildSennitInfo(cfg, nil, mgr, nil, nil, nil, nil)
	require.Contains(t, output, "[lsp]")
	require.Contains(t, output, "gopls = ready")
	require.Contains(t, output, "pyright = error")
	goplsIdx := strings.Index(output, "gopls = ready")
	pyrightIdx := strings.Index(output, "pyright = error")
	require.Less(t, goplsIdx, pyrightIdx, "gopls should appear before pyright")
}

func TestSennitInfo_MCPStates(t *testing.T) {
	t.Parallel()

	connectedAt := time.Date(2025, 1, 15, 15, 4, 5, 0, time.UTC)
	states := map[string]mcp.ClientInfo{
		"github": {
			Name:        "github",
			State:       mcp.StateConnected,
			Counts:      mcp.Counts{Tools: 42, Resources: 7},
			ConnectedAt: connectedAt,
		},
		"filesystem": {
			Name:  "filesystem",
			State: mcp.StateError,
			Error: errors.New("connection refused"),
		},
	}

	cfg := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})

	var b strings.Builder
	writeMCP(&b, states, cfg)
	output := b.String()
	require.Contains(t, output, "[mcp]")
	require.Contains(t, output, "filesystem = error: connection refused")
	require.Contains(t, output, "github = connected (42 tools, 7 resources) since 15:04:05")
	filesystemIdx := strings.Index(output, "filesystem")
	githubIdx := strings.Index(output, "github")
	require.Less(t, filesystemIdx, githubIdx, "filesystem should appear before github")
}

func TestSennitInfo_YoloMode(t *testing.T) {
	t.Parallel()

	cfg := configtest.NewStore(t, &config.Config{
		Providers:   csync.NewMap[string, config.ProviderConfig](),
		Permissions: &config.Permissions{},
	})
	cfg.SetSkipPermissionRequests(true)

	output := buildSennitInfo(cfg, nil, nil, nil, nil, nil, nil)
	require.Contains(t, output, "[permissions]")
	require.Contains(t, output, "mode = yolo")
}

func TestSennitInfo_AllowedTools(t *testing.T) {
	t.Parallel()

	cfg := configtest.NewStore(t, &config.Config{
		Providers:   csync.NewMap[string, config.ProviderConfig](),
		Permissions: &config.Permissions{AllowedTools: []string{"edit:write", "bash"}},
	})

	output := buildSennitInfo(cfg, nil, nil, nil, nil, nil, nil)
	require.Contains(t, output, "[permissions]")
	require.Contains(t, output, "allowed_tools = bash, edit:write")
}

func TestSennitInfo_DisabledTools(t *testing.T) {
	t.Parallel()

	cfg := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options:   &config.Options{DisabledTools: []string{"download", "agentic_fetch"}},
	})

	output := buildSennitInfo(cfg, nil, nil, nil, nil, nil, nil)
	require.Contains(t, output, "[tools]")
	require.Contains(t, output, "disabled = agentic_fetch, download")
}

func TestSennitInfo_Options(t *testing.T) {
	t.Parallel()

	cfg := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options: &config.Options{
			DataDirectory:        "/Users/user/project/.sennit",
			Debug:                true,
			DisableAutoSummarize: true,
		},
	})

	output := buildSennitInfo(cfg, nil, nil, nil, nil, nil, nil)
	require.Contains(t, output, "[options]")
	require.Contains(t, output, "auto_lsp = true")
	require.Contains(t, output, "auto_summarize = false")
	require.Contains(t, output, "data_directory = /Users/user/project/.sennit")
	require.Contains(t, output, "debug = true")
}

func TestSennitInfo_AutoSummarizeInversion(t *testing.T) {
	t.Parallel()

	cfgFalse := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options:   &config.Options{DisableAutoSummarize: true},
	})
	outputFalse := buildSennitInfo(cfgFalse, nil, nil, nil, nil, nil, nil)
	require.Contains(t, outputFalse, "auto_summarize = false")

	cfgTrue := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options:   &config.Options{DisableAutoSummarize: false},
	})
	outputTrue := buildSennitInfo(cfgTrue, nil, nil, nil, nil, nil, nil)
	require.Contains(t, outputTrue, "auto_summarize = true")
}

func TestSennitInfo_NoSecrets(t *testing.T) {
	t.Parallel()

	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("openai", config.ProviderConfig{
		APIKey: "sk-super-secret-key-12345",
		Models: make([]catwalk.Model, 8),
	})

	cfg := configtest.NewStore(t, &config.Config{Providers: providers})
	output := buildSennitInfo(cfg, nil, nil, nil, nil, nil, nil)
	require.NotContains(t, output, "sk-super-secret-key-12345")
	require.NotContains(t, output, "secret")
	require.Contains(t, output, "openai = enabled (8 models)")
}

func TestSennitInfo_DeterministicOrdering(t *testing.T) {
	t.Parallel()

	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("zebra", config.ProviderConfig{Models: make([]catwalk.Model, 1)})
	providers.Set("alpha", config.ProviderConfig{Models: make([]catwalk.Model, 2)})
	providers.Set("middle", config.ProviderConfig{Models: make([]catwalk.Model, 3)})

	states := map[string]mcp.ClientInfo{
		"z-mcp": {Name: "z-mcp", State: mcp.StateConnected, Counts: mcp.Counts{Tools: 1}},
		"a-mcp": {Name: "a-mcp", State: mcp.StateConnected, Counts: mcp.Counts{Tools: 2}},
	}

	cfg := configtest.NewStore(t, &config.Config{
		Providers: providers,
		Options:   &config.Options{DisabledTools: []string{"z-tool", "a-tool"}},
		Permissions: &config.Permissions{
			AllowedTools: []string{"z-perm", "a-perm"},
		},
	})
	cfg.SetSkipPermissionRequests(true)

	// Test MCP ordering via writeMCP directly.
	var mcpBuf strings.Builder
	writeMCP(&mcpBuf, states, cfg)
	mcpOutput := mcpBuf.String()
	aMcpIdx := strings.Index(mcpOutput, "a-mcp = connected")
	zMcpIdx := strings.Index(mcpOutput, "z-mcp = connected")
	require.Less(t, aMcpIdx, zMcpIdx)

	output := buildSennitInfo(cfg, nil, nil, nil, nil, nil, nil)

	alphaIdx := strings.Index(output, "alpha = enabled")
	middleIdx := strings.Index(output, "middle = enabled")
	zebraIdx := strings.Index(output, "zebra = enabled")
	require.Less(t, alphaIdx, middleIdx)
	require.Less(t, middleIdx, zebraIdx)

	require.Contains(t, output, "disabled = a-tool, z-tool")
	require.Contains(t, output, "allowed_tools = a-perm, z-perm")
}

func TestSennitInfo_EmptySectionsOmitted(t *testing.T) {
	t.Parallel()

	cfg := configtest.NewStore(t, &config.Config{
		Providers:   csync.NewMap[string, config.ProviderConfig](),
		Permissions: &config.Permissions{},
		Options:     &config.Options{},
	})

	output := buildSennitInfo(cfg, nil, nil, nil, nil, nil, nil)
	require.NotContains(t, output, "[tools]")
	require.NotContains(t, output, "[permissions]")
	require.NotContains(t, output, "[lsp]")
	require.NotContains(t, output, "[mcp]")
	require.NotContains(t, output, "[skills]")
}

func TestSennitInfo_ConfigStaleness_Clean(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "sennit.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{}`), 0o600))

	store := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	}, configtest.WithLoadedPaths(configPath))

	// Capture snapshot (normally done in Load)
	store.CaptureStalenessSnapshot([]string{configPath})

	output := buildSennitInfo(store, nil, nil, nil, nil, nil, nil)
	require.Contains(t, output, "[config]")
	require.Contains(t, output, "dirty = false")
	require.NotContains(t, output, "changed_paths")
	require.NotContains(t, output, "missing_paths")
}

func TestSennitInfo_ConfigStaleness_Dirty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "sennit.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": false}`), 0o600))

	store := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	}, configtest.WithLoadedPaths(configPath))

	// Capture initial snapshot
	store.CaptureStalenessSnapshot([]string{configPath})

	// Modify file to trigger dirty state
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": true}`), 0o600))

	output := buildSennitInfo(store, nil, nil, nil, nil, nil, nil)
	require.Contains(t, output, "[config]")
	require.Contains(t, output, "dirty = true")
	require.Contains(t, output, "changed_paths")
	require.Contains(t, output, configPath)
}

func TestSennitInfo_ConfigStaleness_MissingPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "sennit.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{}`), 0o600))

	store := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	}, configtest.WithLoadedPaths(configPath))

	// Capture initial snapshot
	store.CaptureStalenessSnapshot([]string{configPath})

	// Delete file to trigger missing state
	require.NoError(t, os.Remove(configPath))

	output := buildSennitInfo(store, nil, nil, nil, nil, nil, nil)
	require.Contains(t, output, "[config]")
	require.Contains(t, output, "dirty = true")
	require.Contains(t, output, "missing_paths")
	require.Contains(t, output, configPath)
}

func TestSennitInfo_Skills_NoSkills(t *testing.T) {
	t.Parallel()

	cfg := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})
	output := buildSennitInfo(cfg, nil, nil, nil, nil, nil, nil)
	require.NotContains(t, output, "[skills]")
}

func TestSennitInfo_Skills_MixedLoadedUnloaded(t *testing.T) {
	t.Parallel()

	allSkills := []*skills.Skill{
		{Name: "go-doc", Builtin: false},
		{Name: "bash", Builtin: false},
		{Name: "sennit-config", Builtin: true},
	}
	activeSkills := allSkills

	tracker := skills.NewTracker(activeSkills)
	tracker.MarkLoaded("bash")
	tracker.MarkLoaded("sennit-config")

	cfg := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})
	output := buildSennitInfo(cfg, nil, nil, allSkills, activeSkills, tracker, nil)
	require.Contains(t, output, "[skills]")
	require.Contains(t, output, "bash = user, loaded")
	require.Contains(t, output, "sennit-config = builtin, loaded")
	require.Contains(t, output, "go-doc = user, unloaded")
}

func TestSennitInfo_Skills_DisabledSkills(t *testing.T) {
	t.Parallel()

	allSkills := []*skills.Skill{
		{Name: "bash", Builtin: false},
		{Name: "sennit-config", Builtin: true},
		{Name: "image-convert", Builtin: false},
	}
	activeSkills := []*skills.Skill{
		{Name: "bash", Builtin: false},
		{Name: "sennit-config", Builtin: true},
	}

	tracker := skills.NewTracker(activeSkills)

	cfg := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options:   &config.Options{DisabledSkills: []string{"image-convert"}},
	})
	output := buildSennitInfo(cfg, nil, nil, allSkills, activeSkills, tracker, nil)
	require.Contains(t, output, "[skills]")
	require.Contains(t, output, "bash = user, unloaded")
	require.Contains(t, output, "sennit-config = builtin, unloaded")
	require.Contains(t, output, "image-convert = user, disabled")
}

func TestSennitInfo_Skills_Ordering(t *testing.T) {
	t.Parallel()

	allSkills := []*skills.Skill{
		{Name: "z-skill", Builtin: false},
		{Name: "a-skill", Builtin: true},
		{Name: "m-skill", Builtin: false},
	}
	activeSkills := allSkills
	tracker := skills.NewTracker(activeSkills)

	cfg := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})
	output := buildSennitInfo(cfg, nil, nil, allSkills, activeSkills, tracker, nil)

	aIdx := strings.Index(output, "a-skill")
	mIdx := strings.Index(output, "m-skill")
	zIdx := strings.Index(output, "z-skill")
	require.Less(t, aIdx, mIdx)
	require.Less(t, mIdx, zIdx)
}

func TestSennitInfo_Skills_BuiltinOrigin(t *testing.T) {
	t.Parallel()

	allSkills := []*skills.Skill{
		{Name: "sennit-config", Builtin: true},
		{Name: "my-skill", Builtin: false},
	}
	activeSkills := allSkills
	tracker := skills.NewTracker(activeSkills)

	cfg := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})
	output := buildSennitInfo(cfg, nil, nil, allSkills, activeSkills, tracker, nil)
	require.Contains(t, output, "sennit-config = builtin, unloaded")
	require.Contains(t, output, "my-skill = user, unloaded")
}

func TestSennitInfo_Hooks(t *testing.T) {
	t.Parallel()

	cfg := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Hooks: map[string][]config.HookConfig{
			"PreToolUse": {
				{Command: "check-privates.sh", Matcher: "edit|write"},
				{Command: "audit.sh"},
			},
		},
	})

	output := buildSennitInfo(cfg, nil, nil, nil, nil, nil, nil)
	require.Contains(t, output, "[hooks]")
	require.Contains(t, output, "PreToolUse (matcher: edit|write) = check-privates.sh")
	require.Contains(t, output, "PreToolUse = audit.sh")
}

func TestSennitInfo_Hooks_NoHooks(t *testing.T) {
	t.Parallel()

	cfg := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})

	output := buildSennitInfo(cfg, nil, nil, nil, nil, nil, nil)
	require.NotContains(t, output, "[hooks]")
}

// TestSennitInfo_Problems_None verifies the section is omitted for a clean
// config, matching the other [section] omission tests above.
//
// The environment problems are stubbed out rather than read from the host:
// they report what the machine is missing, so a CI runner with no clipboard
// helper installed would otherwise put a [problems] section in the output of
// a config that has nothing wrong with it.
func TestSennitInfo_Problems_None(t *testing.T) {
	t.Parallel()

	cfg := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options:   &config.Options{},
	})

	output := buildSennitInfo(cfg, nil, nil, nil, nil, nil, nil, func() []config.Problem { return nil })
	require.NotContains(t, output, "[problems]")
}

// TestSennitInfo_Problems_UnresolvedAgentModel is the feature's motivating
// case: a sub-agent pinned to a model that doesn't exist among the
// providers used to be a silent log warning with a fallback the user never
// saw. It must now show up in sennit_info's [problems] section.
func TestSennitInfo_Problems_UnresolvedAgentModel(t *testing.T) {
	t.Parallel()

	// buildSennitInfo's [problems] section is config.Doctor(cfg), which
	// reads cfg.Problems as-is; the recomputation that would normally add
	// this Problem is validUserAgents' job (unexported, exercised end to
	// end in internal/config/doctor_test.go), so it is set directly here.
	c := &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options:   &config.Options{},
		Problems: []config.Problem{
			{
				Severity: config.SeverityWarn, Area: config.AreaAgent, Subject: "reviewer",
				Message: "agent reviewer: model nope/nope not found — falls back to the main model",
				Hint:    "run 'sennit models' to see available provider/model pairs",
			},
		},
	}
	cfg := configtest.NewStore(t, c)

	output := buildSennitInfo(cfg, nil, nil, nil, nil, nil, nil)
	require.Contains(t, output, "[problems]")
	require.Contains(t, output, "agent.reviewer")
	require.Contains(t, output, "falls back to the main model")
}

// TestSennitInfo_Problems_MCPError verifies an MCP server stuck in an
// error/needs-auth state is merged into the same [problems] section,
// alongside the config.Doctor findings, even though internal/config cannot
// see the MCP registry directly (import-cycle boundary).
func TestSennitInfo_Problems_MCPError(t *testing.T) {
	t.Parallel()

	states := map[string]mcp.ClientInfo{
		"filesystem": {Name: "filesystem", State: mcp.StateError, Error: errors.New("connection refused")},
	}
	cfg := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options:   &config.Options{},
	})

	var b strings.Builder
	writeProblems(&b, cfg, states, nil, func() []config.Problem { return nil })
	output := b.String()
	require.Contains(t, output, "[problems]")
	require.Contains(t, output, "mcp.filesystem")
	require.Contains(t, output, "connection refused")
}

// A SKILL.md that fails to parse is not loaded at all, and an agent told
// to follow it otherwise has no way to find that out: it just proceeds
// without the skill. So the discovery failure has to reach [problems],
// where the agent can actually read it.
func TestSennitInfo_Problems_BrokenSkill(t *testing.T) {
	t.Parallel()

	cfg := configtest.NewStore(t, &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options:   &config.Options{},
	})
	states := []*skills.SkillState{{
		Path:  "/repo/.sennit/skills/feature-development/SKILL.md",
		State: skills.StateError,
		Err:   errors.New("parsing frontmatter: yaml: line 2: mapping values are not allowed in this context"),
	}}

	var b strings.Builder
	writeProblems(&b, cfg, nil, states, func() []config.Problem { return nil })
	output := b.String()
	require.Contains(t, output, "[problems]")
	// Named after its directory: a skill whose frontmatter did not parse
	// has no name of its own yet.
	require.Contains(t, output, "skill.feature-development")
	require.Contains(t, output, "mapping values are not allowed")
}

// TestSennitInfo_Providers_CachedCustomProviderModels verifies that a
// custom provider's model count shows up in [providers] when those models
// come from the global model-discovery cache (internal/config's
// applyCachedModelsForCustomProviders) rather than from sennit.json — i.e.
// that writeProviders needs no changes of its own now that discovered
// models are cache-backed, since it only ever reads len(pc.Models) off the
// already-loaded config.
func TestSennitInfo_Providers_CachedCustomProviderModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "cached-model-1", "object": "model"}, {"id": "cached-model-2", "object": "model"}]}`))
	}))
	defer server.Close()

	globalDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", dataDir)

	dataConfigPath := config.GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(dataConfigPath), 0o755))
	seed := fmt.Sprintf(`{"providers": {"custom": {"api_key": "test-key", "base_url": %q}}}`, server.URL+"/v1")
	require.NoError(t, os.WriteFile(dataConfigPath, []byte(seed), 0o644))

	// First load discovers over HTTP and populates the cache as a side
	// effect (internal/config.validateCustomProviders).
	_, err := configruntime.Load(t.TempDir(), "", false)
	require.NoError(t, err)

	// Second, independent load must pick the models up from the cache, with
	// no models array left in sennit.json to source them from.
	store, err := configruntime.Load(t.TempDir(), "", false)
	require.NoError(t, err)

	output := buildSennitInfo(store, nil, nil, nil, nil, nil, nil)
	require.Contains(t, output, "custom = enabled (2 models)")
}
