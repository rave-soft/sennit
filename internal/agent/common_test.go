package agent

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/filetracker"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"

	_ "github.com/joho/godotenv/autoload"
)

// ---------------------------------------------------------------------------
// Test environment
// ---------------------------------------------------------------------------

// fakeEnv is an environment for testing.
type fakeEnv struct {
	workingDir  string
	sessions    session.Service
	messages    message.Service
	permissions permission.Service
	history     history.Service
	filetracker *filetracker.Service
	lspClients  *csync.Map[string, *lsp.Client]
}

// writeGlobalConfig points the global config location at a fresh directory
// for this test and seeds it with cfgJSON.
//
// Provider and model settings are global-only: a providers/model block in a
// project sennit.json is stripped before the merge (see config.globalOnlyKeys),
// so a fixture that needs a mock provider has to seed the global layer. The
// caller must not be a parallel test — this uses t.Setenv.
func writeGlobalConfig(t *testing.T, cfgJSON string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", filepath.Join(dir, "data"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sennit.json"), []byte(cfgJSON), 0o644))
}

// canonicalTestTempRoot replaces os.TempDir() as the root for the ephemeral
// test working directory. os.TempDir() honors $TMPDIR, which is "/tmp" on
// Linux CI runners but something like "/var/folders/xx/yy/T" on macOS
// runners — and that root ends up baked verbatim into VCR cassettes: tool
// results (ls, edit, grep, glob) echo back the absolute working directory,
// and unlike the system prompt (stripped by normalizeForMatch) those
// tool-role message bytes are matched strictly. A root that moves with the
// OS/environment therefore makes every cassette recorded on Linux
// unreplayable anywhere else. Pinning it keeps the harness's on-disk paths —
// and so the request bodies built from them — identical across the OS
// matrix. See vcr_determinism_test.go for the test that pins this down.
const canonicalTestTempRoot = "/tmp"

// testTempRoot is canonicalTestTempRoot everywhere the cassettes are
// actually replayed, and os.TempDir() on Windows. TestCoderAgent — the only
// cassette consumer — skips on Windows (agent_test.go), so pinning there
// would buy no determinism while pointing 31 other tests at C:\tmp, which
// is not a conventional writable location on a Windows runner.
func testTempRoot() string {
	if runtime.GOOS == "windows" {
		return os.TempDir()
	}
	return canonicalTestTempRoot
}

func testEnv(t *testing.T) fakeEnv {
	t.Helper()
	return testEnvAt(t, filepath.Join(testTempRoot(), "sennit-test-", t.Name()))
}

func testEnvAt(t *testing.T, workingDir string) fakeEnv {
	t.Helper()
	_ = os.RemoveAll(workingDir)

	err := os.MkdirAll(workingDir, 0o755)
	require.NoError(t, err)

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)

	q := db.New(conn)
	sessions := session.NewService(q, conn, "/test/project")
	messages := message.NewService(q)

	permissions := permission.NewPermissionService(workingDir, true, []string{})
	history := history.NewService(q, conn)
	filetrackerService := filetracker.NewService(q, workingDir)
	lspClients := csync.NewMap[string, *lsp.Client]()

	t.Cleanup(func() {
		// Close the message service before the connection: it owns
		// debounce timers that write on their own schedule, and one
		// firing after conn.Close holds a connection open — which on
		// Windows keeps the database file locked and makes t.TempDir's
		// own cleanup fail the test that had already passed.
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = messages.Close(closeCtx)
		cancel()
		_ = conn.Close()
		_ = os.RemoveAll(workingDir)
	})

	return fakeEnv{
		workingDir,
		sessions,
		messages,
		permissions,
		history,
		&filetrackerService,
		lspClients,
	}
}

// titleCallMaxTokens is the output-token budget generateTitle sets for a
// non-reasoning model. It is the only marker a fake model has to tell the
// detached title stream apart from the turn's own stream.
const titleCallMaxTokens = 40

// isTitleCall reports whether call comes from the detached title goroutine.
// Titles used to run on a separate small model; now that an agent carries one
// model, fakes that count or block their turn's streams answer the title call
// out of band through titleStream instead.
func isTitleCall(call fantasy.Call) bool {
	return call.MaxOutputTokens != nil && *call.MaxOutputTokens == titleCallMaxTokens
}

// titleStream is the trivial finished response a fake model returns for the
// title call.
func titleStream() (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "title"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "title", Delta: "title"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "title"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

// syncLogBuffer is a thread-safe buffer, since the delegation lifecycle's
// log calls come from goroutines (run watchers, continuation dispatch)
// racing the test's own assertions.
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// logCaptureMu serializes every captureLogs/captureJSONLogs call across
// this package's whole test binary. slog.SetDefault is process-global:
// two t.Parallel() tests each installing their own handler would race —
// whichever SetDefault ran last wins, so an unlucky ordering makes one
// test's log lines vanish and hands them to the other's buffer instead
// (this was an actual observed failure, not a theoretical one). Holding
// this for the capture's whole lifetime — install through the t.Cleanup
// restore — makes "capturing logs" a resource only one test can hold at a
// time, the same way a shared external resource would be serialized,
// without having to strip t.Parallel() from every test that uses it.
var logCaptureMu sync.Mutex

// captureLogs installs a buffer-backed slog handler at Debug level for the
// duration of the test, restoring the previous default handler via
// t.Cleanup. See logCaptureMu
// for why this serializes against every other capture in the package.
func captureLogs(t *testing.T) *syncLogBuffer {
	t.Helper()
	logCaptureMu.Lock()
	var b syncLogBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		logCaptureMu.Unlock()
	})
	return &b
}

// ---------------------------------------------------------------------------
// Agent construction
// ---------------------------------------------------------------------------

func testSessionAgent(env fakeEnv, model fantasy.LanguageModel, systemPrompt string, tools ...fantasy.AgentTool) SessionAgent {
	agent := NewSessionAgent(SessionAgentOptions{
		Model: Model{
			Model: model,
			CatalogCfg: catwalk.Model{
				ContextWindow:    200000,
				DefaultMaxTokens: 10000,
			},
		},
		SystemPrompt: systemPrompt,
		Sessions:     env.sessions,
		Messages:     env.messages,
		Tools:        tools,
	})
	return agent
}

func coderAgent(client *http.Client, env fakeEnv, model fantasy.LanguageModel) (SessionAgent, error) {
	fixedTime := func() time.Time {
		t, _ := time.Parse("1/2/2006", "1/1/2025")
		return t
	}
	p, err := coderPrompt(
		prompt.WithTimeFunc(fixedTime),
		prompt.WithPlatform("linux"),
		prompt.WithWorkingDir(filepath.ToSlash(env.workingDir)),
	)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(env.workingDir, "", false)
	if err != nil {
		return nil, err
	}

	// Set a fixed config to ensure cassettes match
	// independently of user config on `$HOME/.config/sennit/sennit.json`.
	cfg.Config().Options.Attribution = &config.Attribution{
		TrailerStyle:  "assisted-by",
		GeneratedWith: true,
	}

	// Clear some fields to avoid issues with VCR cassette matching. Every
	// builtin skill's name/description goes into the system prompt (so the
	// model can choose to load it), so adding one changes the recorded
	// cassettes' request bodies just like sennit-config already did —
	// "tasks" (added alongside this comment) needs the same exclusion.
	cfg.Config().Options.SkillsPaths = nil
	cfg.Config().Options.DisabledSkills = []string{"sennit-config", "tasks"}
	cfg.Config().Options.ContextPaths = nil
	cfg.Config().Options.GlobalContextPaths = nil
	cfg.Config().LSP = nil

	systemPrompt, err := p.Build(context.TODO(), model.Provider(), model.Model(), cfg)
	if err != nil {
		return nil, err
	}

	// Get the model name for the bash tool
	modelName := model.Model() // fallback to ID if Name not available
	if cfgModel := cfg.Config().GetModel(model.Provider(), model.Model()); cfgModel != nil {
		modelName = cfgModel.Name
	}

	allTools := []fantasy.AgentTool{
		tools.NewBashTool(env.permissions, env.workingDir, cfg.Config().Options.Attribution, modelName, shell.NewBackgroundShellManager()),
		tools.NewDownloadTool(env.permissions, env.workingDir, client),
		tools.NewEditTool(nil, env.permissions, env.history, *env.filetracker, env.workingDir),
		tools.NewMultiEditTool(nil, env.permissions, env.history, *env.filetracker, env.workingDir),
		tools.NewFetchTool(env.permissions, env.workingDir, client),
		tools.NewGlobTool(env.workingDir, cfg.Config().Tools.Glob),
		tools.NewGrepTool(env.workingDir, cfg.Config().Tools.Grep),
		tools.NewLsTool(env.permissions, env.workingDir, cfg.Config().Tools.Ls),
		tools.NewReadTool(nil, env.permissions, *env.filetracker, nil, env.workingDir),
		tools.NewWriteTool(nil, env.permissions, env.history, *env.filetracker, env.workingDir),
	}

	return testSessionAgent(env, model, systemPrompt, allTools...), nil
}
