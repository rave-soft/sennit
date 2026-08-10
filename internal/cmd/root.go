package cmd

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	fang "charm.land/fang/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/charmtone"
	xstrings "github.com/charmbracelet/x/exp/strings"
	"github.com/charmbracelet/x/term"
	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/client"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/event"
	"github.com/rave-soft/braid/internal/hostaddr"
	braidlog "github.com/rave-soft/braid/internal/log"
	"github.com/rave-soft/braid/internal/projects"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/server/supervisor"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/common"
	ui "github.com/rave-soft/braid/internal/ui/model"
	"github.com/rave-soft/braid/internal/version"
	"github.com/rave-soft/braid/internal/workspace"
	"github.com/spf13/cobra"
)

var clientHost string

func init() {
	rootCmd.PersistentFlags().StringP("cwd", "c", "", "Current working directory")
	rootCmd.PersistentFlags().StringP("data-dir", "D", "", "Custom braid data directory")
	rootCmd.PersistentFlags().BoolP("debug", "d", false, "Debug")
	rootCmd.PersistentFlags().StringVarP(&clientHost, "host", "H", hostaddr.DefaultHost(), "Connect to a specific braid server host (for advanced users)")
	rootCmd.Flags().BoolP("help", "h", false, "Help")
	rootCmd.Flags().BoolP("yolo", "y", false, "Automatically accept all permissions (dangerous mode)")
	rootCmd.PersistentFlags().StringSlice("channels", nil, "MCP servers to enable as channels (repeatable), e.g. --channels server:webhook")
	_ = rootCmd.PersistentFlags().MarkHidden("channels")
	rootCmd.Flags().StringP("session", "s", "", "Continue a previous session by ID")
	rootCmd.Flags().BoolP("continue", "C", false, "Continue the most recent session")
	rootCmd.MarkFlagsMutuallyExclusive("session", "continue")

	rootCmd.AddCommand(
		runCmd,
		dirsCmd,
		projectsCmd,
		logsCmd,
		logoutCmd,
		schemaCmd,
		loginCmd,
		statCmd,
		sessionCmd,
		gcCmd,
	)
}

var rootCmd = &cobra.Command{
	Use:   "braid",
	Short: "A terminal-first AI assistant for software development",
	Long:  "A glamorous, terminal-first AI assistant for software development and adjacent tasks",
	Example: `
# Run in interactive mode
braid

# Run non-interactively
braid run "Guess my 5 favorite Pokémon"

# Run a non-interactively with pipes and redirection
cat README.md | braid run "make this more glamorous" > GLAMOROUS_README.md

# Run with debug logging in a specific directory
braid --debug --cwd /path/to/project

# Run in yolo mode (auto-accept all permissions; use with care)
braid --yolo

# Run with custom data directory
braid --data-dir /path/to/custom/.braid

# Continue a previous session
braid --session {session-id}

# Continue the most recent session
braid --continue
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		sessionID, _ := cmd.Flags().GetString("session")
		continueLast, _ := cmd.Flags().GetBool("continue")

		ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		if sessionID != "" {
			sess, err := resolveWorkspaceSessionID(cmd.Context(), ws, sessionID)
			if err != nil {
				return err
			}
			sessionID = sess.ID
		}

		event.AppInitialized()

		com := common.DefaultCommon(cmd.Context(), ws)
		model := ui.NewRoot(com, sessionID, continueLast)

		inputFilter := ui.NewFilter()
		var env uv.Environ = os.Environ()
		program := tea.NewProgram(
			model,
			tea.WithEnvironment(env),
			tea.WithContext(cmd.Context()),
			tea.WithFilter(inputFilter.Filter),
		)
		model.SetSend(program.Send)
		go ws.Subscribe(program)

		_, err = program.Run()
		model.Cleanup()
		if err != nil {
			event.Error(err)
			slog.Error("TUI run error", "error", err)
			return errors.New("Braid crashed. If metrics are enabled, we were notified about it. If you'd like to report it, please copy the stacktrace above and open an issue at https://github.com/rave-soft/braid/issues/new?template=bug.yml") //nolint:staticcheck
		}
		return nil
	},
}

var heartbit = lipgloss.NewStyle().Foreground(charmtone.Dolly).SetString(`
    ▄▄▄▄▄▄▄▄    ▄▄▄▄▄▄▄▄
  ███████████  ███████████
████████████████████████████
████████████████████████████
██████████▀██████▀██████████
██████████ ██████ ██████████
▀▀██████▄████▄▄████▄██████▀▀
  ████████████████████████
    ████████████████████
       ▀▀██████████▀▀
           ▀▀▀▀▀▀
`)

// copied from cobra:
const defaultVersionTemplate = `{{with .DisplayName}}{{printf "%s " .}}{{end}}{{printf "version %s" .Version}}
`

func Execute() {
	// FIXME: config.Load uses slog internally during provider resolution,
	// but the file-based logger isn't set up until after config is loaded
	// (because the log path depends on the data directory from config).
	// This creates a window where slog calls in config.Load leak to
	// stderr. We discard early logs here as a workaround. The proper
	// fix is to remove slog calls from config.Load and have it return
	// warnings/diagnostics instead of logging them as a side effect.
	slog.SetDefault(slog.New(slog.DiscardHandler))

	// NOTE: very hacky: we create a colorprofile writer with STDOUT, then make
	// it forward to a bytes.Buffer, write the colored heartbit to it, and then
	// finally prepend it in the version template.
	// Unfortunately cobra doesn't give us a way to set a function to handle
	// printing the version, and PreRunE runs after the version is already
	// handled, so that doesn't work either.
	// This is the only way I could find that works relatively well.
	if term.IsTerminal(os.Stdout.Fd()) {
		var b bytes.Buffer
		w := colorprofile.NewWriter(os.Stdout, os.Environ())
		w.Forward = &b
		_, _ = w.WriteString(heartbit.String())
		rootCmd.SetVersionTemplate(b.String() + "\n" + defaultVersionTemplate)
	}
	if err := fang.Execute(
		context.Background(),
		rootCmd,
		fang.WithVersion(version.Version),
		fang.WithNotifySignal(os.Interrupt),
	); err != nil {
		os.Exit(1)
	}
}

// supportsProgressBar tries to determine whether the current terminal supports
// progress bars by looking into environment variables.
func supportsProgressBar() bool {
	if !term.IsTerminal(os.Stderr.Fd()) {
		return false
	}
	termProg := os.Getenv("TERM_PROGRAM")
	_, isWindowsTerminal := os.LookupEnv("WT_SESSION")

	return isWindowsTerminal || xstrings.ContainsAnyOf(strings.ToLower(termProg), "ghostty", "iterm2", "rio")
}

// useClientServer returns true when the client/server architecture is
// enabled via the BRAID_CLIENT_SERVER environment variable.
func useClientServer() bool {
	v, _ := strconv.ParseBool(os.Getenv("BRAID_CLIENT_SERVER"))
	return v
}

// setupWorkspaceWithProgressBar wraps setupWorkspace with an optional
// terminal progress bar shown during initialization.
func setupWorkspaceWithProgressBar(cmd *cobra.Command) (workspace.Workspace, func(), error) {
	showProgress := supportsProgressBar()
	if showProgress {
		_, _ = fmt.Fprintf(os.Stderr, ansi.SetIndeterminateProgressBar)
	}

	ws, cleanup, err := setupWorkspace(cmd)

	if showProgress {
		_, _ = fmt.Fprintf(os.Stderr, ansi.ResetProgressBar)
	}

	return ws, cleanup, err
}

// setupWorkspace returns a Workspace and cleanup function. When
// BRAID_CLIENT_SERVER=1, it connects to a server process and returns a
// ClientWorkspace. Otherwise it creates an in-process app.App and
// returns an AppWorkspace.
func setupWorkspace(cmd *cobra.Command) (workspace.Workspace, func(), error) {
	if useClientServer() {
		return setupClientServerWorkspace(cmd)
	}
	return setupLocalWorkspace(cmd)
}

// setupLocalWorkspace creates an in-process app.App and wraps it in an
// AppWorkspace.
func setupLocalWorkspace(cmd *cobra.Command) (workspace.Workspace, func(), error) {
	debug, _ := cmd.Flags().GetBool("debug")
	yolo, _ := cmd.Flags().GetBool("yolo")
	channels, _ := cmd.Flags().GetStringSlice("channels")
	dataDir, _ := cmd.Flags().GetString("data-dir")
	ctx := cmd.Context()

	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return nil, nil, err
	}

	// Local mode hosts a single workspace per process, so it enables
	// WithGlobalMirror (keeps the package globals the TUI reads via
	// skills.GetLatestStates in sync with the manager) and closes the DB
	// connection if app.New fails, since it owns the only reference to it.
	boot, err := app.Bootstrap(ctx, cwd, app.BootstrapOptions{
		DataDir:             dataDir,
		Debug:               debug,
		YOLO:                yolo,
		Channels:            channels,
		GlobalSkillsMirror:  true,
		CloseDBOnAppFailure: true,
		PostDataDir: func(cfg *config.ConfigStore) error {
			if err := projects.Register(cwd, cfg.Config().Options.DataDirectory); err != nil {
				slog.Warn("Failed to register project", "error", err)
			}
			return nil
		},
		PostConnect: func(cfg *config.ConfigStore) error {
			braidlog.Setup(config.GlobalLogFile(), debug)
			return nil
		},
		OnAppInitFailure: func(err error) {
			slog.Error("Failed to create app instance", "error", err)
		},
	})
	if err != nil {
		return nil, nil, err
	}

	attachLocalThreads(ctx, boot.App, cwd)

	ws := workspace.NewAppWorkspace(boot.App, boot.Config)
	cleanup := func() { boot.App.Shutdown() }
	return ws, cleanup, nil
}

// setupClientServerWorkspace connects to a server process and wraps the
// result in a ClientWorkspace.
func setupClientServerWorkspace(cmd *cobra.Command) (workspace.Workspace, func(), error) {
	c, protoWs, _, err := connectToServer(cmd)
	if err != nil {
		return nil, nil, err
	}

	clientWs := workspace.NewClientWorkspace(c, *protoWs)

	if protoWs.Config.IsConfigured() {
		if err := clientWs.InitCoderAgent(cmd.Context()); err != nil {
			slog.Error("Failed to initialize coder agent", "error", err)
		}
	}

	// Clean up via Shutdown rather than connectToServer's closure: it stops
	// the subscription's reconnect/recovery loop first, so our own exit
	// cannot be mistaken for a lost workspace and re-created mid-quit.
	return clientWs, clientWs.Shutdown, nil
}

// connectToServer ensures the server is running, creates a client and
// workspace, and returns a cleanup function that deletes the workspace.
func connectToServer(cmd *cobra.Command) (*client.Client, *proto.Workspace, func(), error) {
	hostURL, err := hostaddr.ParseHostURL(clientHost)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid host URL: %v", err)
	}

	if err := supervisor.EnsureRunning(cmd.Context(), hostURL, supervisorOptions(cmd)); err != nil {
		return nil, nil, nil, err
	}

	debug, _ := cmd.Flags().GetBool("debug")
	yolo, _ := cmd.Flags().GetBool("yolo")
	channels, _ := cmd.Flags().GetStringSlice("channels")
	dataDir, _ := cmd.Flags().GetString("data-dir")

	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return nil, nil, nil, err
	}

	c, err := client.NewClient(cwd, hostURL.Scheme, hostURL.Host)
	if err != nil {
		return nil, nil, nil, err
	}

	wsReq := proto.Workspace{
		Path:     cwd,
		DataDir:  dataDir,
		Debug:    debug,
		YOLO:     yolo,
		Channels: channels,
		Version:  version.Version,
		Env:      os.Environ(),
	}

	ws, err := createWorkspaceOnLiveServer(cmd.Context(), c, wsReq, func() error {
		return supervisor.Replace(cmd.Context(), hostURL, supervisorOptions(cmd))
	})
	if err != nil {
		return nil, nil, nil, err
	}

	if ws.Config != nil {
		braidlog.Setup(config.GlobalLogFile(), debug)
	}

	// Retiring the client releases every claim it holds, so it covers
	// workspaces this process created but never learned the ID of.
	cleanup := func() {
		if err := c.RetireClient(context.Background()); err != nil {
			_ = c.DeleteWorkspace(context.Background(), ws.ID)
		}
	}
	return c, ws, cleanup, nil
}

// maxStaleServerRetries bounds how many times workspace creation may be
// retried against a replacement server. Only one client can lose the race
// against a given server's shutdown, so a single retry is normally enough;
// the bound just keeps a pathological loop finite.
const maxStaleServerRetries = 3

// createWorkspaceOnLiveServer creates the workspace, retrying against a
// replacement when the server it reached has already committed to shutting
// itself down for being idle.
//
// That race is unavoidable: the server decides to exit while no client is
// talking to it, and a client can arrive between that decision and the
// socket going away. The decision is final on the server's side, so the
// only correct response is to bring up a fresh server and ask again
// instead of failing the command.
func createWorkspaceOnLiveServer(
	ctx context.Context, c *client.Client, req proto.Workspace, replace func() error,
) (*proto.Workspace, error) {
	for attempt := range maxStaleServerRetries {
		ws, err := c.CreateWorkspace(ctx, req)
		if err == nil {
			return ws, nil
		}
		if !errors.Is(err, client.ErrServerShuttingDown) || attempt == maxStaleServerRetries-1 {
			return nil, fmt.Errorf("failed to create workspace: %v", err)
		}
		slog.Warn("Server is shutting down; retrying against a replacement",
			"attempt", attempt+1, "error", err)
		if err := replace(); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("failed to create workspace: server kept shutting down")
}

// supervisorOptions builds the supervisor.Options the client-side
// auto-start machinery needs from this command's flags.
func supervisorOptions(cmd *cobra.Command) supervisor.Options {
	debug, _ := cmd.Flags().GetBool("debug")
	return supervisor.Options{
		Debug:      debug,
		ClientHost: clientHost,
	}
}

func MaybePrependStdin(prompt string) (string, error) {
	if term.IsTerminal(os.Stdin.Fd()) {
		return prompt, nil
	}
	fi, err := os.Stdin.Stat()
	if err != nil {
		return prompt, err
	}
	// Check if stdin is a named pipe ( | ) or regular file ( < ).
	if fi.Mode()&os.ModeNamedPipe == 0 && !fi.Mode().IsRegular() {
		return prompt, nil
	}
	bts, err := io.ReadAll(os.Stdin)
	if err != nil {
		return prompt, err
	}
	return string(bts) + "\n\n" + prompt, nil
}

// resolveWorkspaceSessionID resolves a session ID that may be a full
// UUID, full hash, or hash prefix. Works against the Workspace
// interface so both local and client/server paths get hash prefix
// support.
func resolveWorkspaceSessionID(ctx context.Context, ws workspace.Workspace, id string) (session.Session, error) {
	if sess, err := ws.GetSession(ctx, id); err == nil {
		return sess, nil
	}

	sessions, err := ws.ListSessions(ctx)
	if err != nil {
		return session.Session{}, err
	}

	var matches []session.Session
	for _, s := range sessions {
		hash := session.HashID(s.ID)
		if hash == id || strings.HasPrefix(hash, id) {
			matches = append(matches, s)
		}
	}

	switch len(matches) {
	case 0:
		return session.Session{}, fmt.Errorf("session not found: %s", id)
	case 1:
		return matches[0], nil
	default:
		return session.Session{}, fmt.Errorf("session ID %q is ambiguous (%d matches)", id, len(matches))
	}
}

func ResolveCwd(cmd *cobra.Command) (string, error) {
	cwd, _ := cmd.Flags().GetString("cwd")
	if cwd != "" {
		err := os.Chdir(cwd)
		if err != nil {
			return "", fmt.Errorf("failed to change directory: %v", err)
		}
		return cwd, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current working directory: %v", err)
	}
	return cwd, nil
}

func createDotBraidDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create data directory: %q %w", dir, err)
	}

	gitIgnorePath := filepath.Join(dir, ".gitignore")
	content, err := os.ReadFile(gitIgnorePath)

	// create or update if old version
	if os.IsNotExist(err) || string(content) == oldGitIgnore {
		if err := os.WriteFile(gitIgnorePath, []byte(defaultGitIgnore), 0o644); err != nil {
			return fmt.Errorf("failed to create .gitignore file: %q %w", gitIgnorePath, err)
		}
	}

	return nil
}

//go:embed gitignore/old
var oldGitIgnore string

//go:embed gitignore/default
var defaultGitIgnore string
