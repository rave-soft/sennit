package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	fang "charm.land/fang/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	xstrings "github.com/charmbracelet/x/exp/strings"
	"github.com/charmbracelet/x/term"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/app/threadspawn"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
	sennitlog "github.com/rave-soft/sennit/internal/log"
	"github.com/rave-soft/sennit/internal/projects"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/logo"
	ui "github.com/rave-soft/sennit/internal/ui/model"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/version"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/appws"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.PersistentFlags().StringP("cwd", "c", "", "Current working directory")
	rootCmd.PersistentFlags().StringP("data-dir", "D", "", "Custom sennit data directory")
	rootCmd.PersistentFlags().BoolP("debug", "d", false, "Debug")
	rootCmd.PersistentFlags().Bool("trust-project", false, "Trust and enable configuration from the current project")
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
		accountsCmd,
		statCmd,
		sessionCmd,
		threadsCmd,
		gcCmd,
	)
}

var earlyLogs *sennitlog.EarlyHandler

var rootCmd = &cobra.Command{
	Use:   brand.Slug,
	Short: "A terminal-first AI assistant for software development",
	Long:  "A glamorous, terminal-first AI assistant for software development and adjacent tasks",
	Example: `
# Run in interactive mode
sennit

# Run non-interactively
sennit run "Guess my 5 favorite Pokémon"

# Run a non-interactively with pipes and redirection
cat README.md | sennit run "make this more glamorous" > GLAMOROUS_README.md

# Run with debug logging in a specific directory
sennit --debug --cwd /path/to/project

# Run in yolo mode (auto-accept all permissions; use with care)
sennit --yolo

# Run with custom data directory
sennit --data-dir /path/to/custom/.sennit

# Continue a previous session
sennit --session {session-id}

# Continue the most recent session
sennit --continue
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
			sess, err := resolveSessionID(cmd.Context(), workspaceSessionLookup{ws}, sessionID)
			if err != nil {
				return err
			}
			sessionID = sess.ID
		}

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
		go ws.Subscribe(func(msg any) { program.Send(msg) })

		_, err = program.Run()
		model.Cleanup()
		if err != nil {
			slog.Error("TUI run error", "error", err)
			return errors.New("Sennit crashed. Please copy the stacktrace above and open an issue at " + brand.RepoURL + "/issues/new?template=bug.yml") //nolint:staticcheck
		}
		return nil
	},
}

// versionLogo renders the Sennit wordmark shown above `sennit --version`
// in a terminal, in place of the ASCII heart that used to be there.
//
// The version it draws into the wordmark's meta row is truncated to the
// width of the letterforms — that row is sized for the sidebar this logo
// was written for. So the plain "sennit version X" line still follows it:
// the art is decoration, the line is the answer, and a development
// version string is far too long to fit.
//
// The default palette is used rather than the person's configured theme:
// --version must answer without loading config, which is the whole reason
// it is handled before PreRunE runs.
func versionLogo() string {
	t := styles.SennitDark()
	return logo.Render(t.Logo.GradCanvas, version.Version, false, logo.Opts{
		FieldColor:   t.Logo.FieldColor,
		TitleColorA:  t.Logo.TitleColorA,
		TitleColorB:  t.Logo.TitleColorB,
		VendorColor:  t.Logo.VendorColor,
		VersionColor: t.Logo.VersionColor,
	})
}

// copied from cobra:
const defaultVersionTemplate = `{{with .DisplayName}}{{printf "%s " .}}{{end}}{{printf "version %s" .Version}}
`

func Execute() {
	// Config loading determines the log path, yet it may report useful warnings.
	// Buffer them until setupLocalWorkspace installs the file logger so startup
	// diagnostics neither leak to stderr nor get silently discarded.
	earlyLogs = sennitlog.NewEarlyHandler()
	slog.SetDefault(slog.New(earlyLogs))

	// NOTE: very hacky: we create a colorprofile writer with STDOUT, then make
	// it forward to a bytes.Buffer, write the colored logo to it, and then
	// use that as the whole version template.
	// Unfortunately cobra doesn't give us a way to set a function to handle
	// printing the version, and PreRunE runs after the version is already
	// handled, so that doesn't work either.
	// This is the only way I could find that works relatively well.
	//
	// Only when stdout is a terminal: a pipe gets the plain
	// "sennit version X" line alone, which is what anything parsing this
	// wants.
	if term.IsTerminal(os.Stdout.Fd()) {
		var b bytes.Buffer
		w := colorprofile.NewWriter(os.Stdout, os.Environ())
		w.Forward = &b
		_, _ = w.WriteString(versionLogo())
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

// setupProcessLogging installs the process-wide file logger the first
// time any command reaches this point, then replays whatever startup
// diagnostics EarlyHandler buffered before the logger existed (e.g.
// config/migrate's deprecated-key warnings). It is the one place that
// calls sennitlog.Setup: setupLocalWorkspace (interactive and
// `sennit run`) and initConfig (every read-only command: doctor, stat,
// models, logs, session, gc, import) both funnel through it, so a
// warning logged before either path had gotten this far isn't lost
// depending on which command produced it.
//
// sennitlog.Setup only takes effect on its first call in the process
// (see internal/log's initOnce) - later calls, including from a second
// command in the same process, are no-ops.
func setupProcessLogging(cmd *cobra.Command, debug bool) {
	sennitlog.Setup(config.GlobalLogFile(), debug, verboseLogWriters(cmd)...)
	if earlyLogs != nil {
		earlyLogs.Replay(slog.Default())
	}
}

// verboseLogWriters returns the extra writers Setup should mirror log
// records to. Only `sennit run --verbose` wants this; cmd.Flags().GetBool
// returns false (with an ignored error) for a command that has no
// "verbose" flag, which is every other command, so calling this
// unconditionally from setupProcessLogging is safe.
//
// Split out as its own function so the verbose/no-verbose decision is
// unit-testable without tripping internal/log's process-wide Setup
// singleton.
func verboseLogWriters(cmd *cobra.Command) []io.Writer {
	if verbose, _ := cmd.Flags().GetBool("verbose"); verbose {
		return []io.Writer{os.Stderr}
	}
	return nil
}

// setupWorkspaceWithProgressBar wraps setupLocalWorkspace with an optional
// terminal progress bar shown during initialization.
func setupWorkspaceWithProgressBar(cmd *cobra.Command) (workspace.Workspace, func(), error) {
	showProgress := supportsProgressBar()
	if showProgress {
		_, _ = fmt.Fprintf(os.Stderr, ansi.SetIndeterminateProgressBar)
	}

	ws, cleanup, err := setupLocalWorkspace(cmd)

	if showProgress {
		_, _ = fmt.Fprintf(os.Stderr, ansi.ResetProgressBar)
	}

	return ws, cleanup, err
}

// setupLocalWorkspace creates an in-process app.App and wraps it in an
// AppWorkspace.
func setupLocalWorkspace(cmd *cobra.Command) (workspace.Workspace, func(), error) {
	debug, _ := cmd.Flags().GetBool("debug")
	yolo, _ := cmd.Flags().GetBool("yolo")
	channels, _ := cmd.Flags().GetStringSlice("channels")
	dataDir, _ := cmd.Flags().GetString("data-dir")
	trustProject, _ := cmd.Flags().GetBool("trust-project")
	ctx := cmd.Context()

	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return nil, nil, err
	}

	boot, err := app.Bootstrap(ctx, cwd, app.BootstrapOptions{
		DataDir:       dataDir,
		Debug:         debug,
		YOLO:          yolo,
		Channels:      channels,
		TrustProject:  trustProject,
		WorkspaceLock: true,
		PostDataDir: func(cfg *config.ConfigStore) error {
			if err := projects.Register(cwd, cfg.Config().Options.DataDirectory); err != nil {
				slog.Warn("Failed to register project", "error", err)
			}
			return nil
		},
		PostConnect: func(cfg *config.ConfigStore) error {
			setupProcessLogging(cmd, debug)
			return nil
		},
		OnAppInitFailure: func(err error) {
			slog.Error("Failed to create app instance", "error", err)
		},
	})
	if err != nil {
		return nil, nil, err
	}

	threadspawn.Attach(ctx, boot.App, cwd, threadspawn.NewLocalSpawner(
		func() map[string]config.Agent { return boot.App.Config().UserAgents() },
		func() []*skills.Skill { return skills.Inheritable(boot.App.Skills.AllSkills()) },
		boot.App.PermissionsSkipFunc(),
		func() config.SelectedModel { return boot.App.Config().Model },
		func(a *app.App) workspace.Workspace { return appws.NewAppWorkspace(a, a.Store()) },
	))

	ws := appws.NewAppWorkspace(boot.App, boot.Config)
	cleanup := func() { boot.App.Shutdown() }
	return ws, cleanup, nil
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

func ResolveCwd(cmd *cobra.Command) (string, error) {
	cwd, _ := cmd.Flags().GetString("cwd")
	if cwd != "" {
		err := os.Chdir(cwd)
		if err != nil {
			return "", fmt.Errorf("failed to change directory: %w", err)
		}
		return cwd, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current working directory: %w", err)
	}
	return cwd, nil
}
