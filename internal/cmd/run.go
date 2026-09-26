package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/daemon/supervisor"
	"github.com/rave-soft/sennit/internal/format"
	"github.com/rave-soft/sennit/internal/spin"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Aliases: []string{"r"},
	Use:     "run [prompt...]",
	Short:   "Run a single non-interactive prompt",
	Long: `Run a single prompt in non-interactive mode and exit.
The prompt can be provided as arguments or piped from stdin.`,
	Example: `
# Run a simple prompt
sennit run "Guess my 5 favorite Pokémon"

# Pipe input from stdin
curl https://example.com | sennit run "Summarize this website"

# Read from a file
sennit run "What is this code doing?" <<< prrr.go

# Redirect output to a file
sennit run "Generate a hot README for this project" > MY_HOT_README.md

# Run in quiet mode (hide the spinner)
sennit run --quiet "Generate a README for this project"

# Run in verbose mode (show logs)
sennit run --verbose "Generate a README for this project"

# Continue a previous session
sennit run --session {session-id} "Follow up on your last response"

# Continue the most recent session
sennit run --continue "Follow up on your last response"

  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		var (
			quiet, _     = cmd.Flags().GetBool("quiet")
			verbose, _   = cmd.Flags().GetBool("verbose")
			model, _     = cmd.Flags().GetString("model")
			sessionID, _ = cmd.Flags().GetString("session")
			useLast, _   = cmd.Flags().GetBool("continue")
			detach, _    = cmd.Flags().GetBool("detach")
		)

		// Cancel on SIGINT or SIGTERM. Rooted at the command's own
		// context, not Background: everything downstream (the App, its
		// agent, its shells) hangs off this one, and a context with no
		// parent left them running when cobra's was cancelled.
		ctx, cancel := runSignalContext(cmd.Context())
		defer cancel()

		prompt := strings.Join(args, " ")

		prompt, err := MaybePrependStdin(prompt)
		if err != nil {
			slog.Error("Failed to read from stdin", "error", err)
			return err
		}

		if prompt == "" {
			return fmt.Errorf("no prompt provided")
		}

		if detach {
			return runDetached(cmd, prompt, model, sessionID, useLast)
		}

		ws, cleanup, err := setupRunWorkspace(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		if !ws.Config().IsConfigured() {
			return fmt.Errorf("no providers configured - please run 'sennit' to set up a provider interactively")
		}

		if sessionID != "" {
			sess, err := resolveSessionID(ctx, workspaceSessionLookup{ws}, sessionID)
			if err != nil {
				return err
			}
			sessionID = sess.ID
		}

		return runAgent(ctx, ws, prompt, model, quiet || verbose, sessionID, useLast)
	},
}

// setupRunWorkspace is `sennit run`'s own workspace setup: it connects
// to this project's daemon when one is already running, and never
// starts one otherwise (CLIENT-SERVER.md, PR 2.3) -- unlike the
// interactive root command's options.daemon=auto path, a plain `run`
// spawning a daemon behind the person's back would be a surprise, not a
// convenience, for a single non-interactive invocation. A daemon lock
// held by an embedded TUI (*supervisor.ErrTUILocked) is returned as-is,
// same wording an in-process run would hit on its own when it tries to
// acquire the workspace lock.
func setupRunWorkspace(cmd *cobra.Command) (workspace.Workspace, func(), error) {
	ctx := cmd.Context()
	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return nil, nil, err
	}
	debug, _ := cmd.Flags().GetBool("debug")
	dataDir, _ := cmd.Flags().GetString("data-dir")

	socketPath, running, err := supervisor.ProbeRunning(ctx, cwd, supervisor.Options{DataDir: dataDir, Debug: debug})
	if err != nil {
		return nil, nil, err
	}
	if !running {
		return setupLocalWorkspace(cmd)
	}

	client, _, cleanup, err := connectDaemonWorkspace(ctx, cwd, dataDir, debug, socketPath)
	if err != nil {
		return nil, nil, err
	}
	return client, cleanup, nil
}

// runDetached implements `sennit run --detach`: it finds or starts this
// project's daemon (spawning is allowed here, unlike a plain run --
// that's the whole point of asking to detach), resolves the session the
// same way an attached run would, hands the daemon the turn with
// AgentRun rather than AgentRunStream (fire-and-forget: nothing here
// waits for it), and returns immediately with the session ID and how to
// come back to it.
func runDetached(cmd *cobra.Command, prompt, model, sessionID string, useLast bool) error {
	ctx := cmd.Context()
	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return err
	}
	debug, _ := cmd.Flags().GetBool("debug")
	dataDir, _ := cmd.Flags().GetString("data-dir")

	client, _, cleanup, err := setupDaemonWorkspace(ctx, cwd, dataDir, debug, supervisor.Options{})
	if err != nil {
		return err
	}
	defer cleanup()

	if !client.Config().IsConfigured() {
		return fmt.Errorf("no providers configured - please run 'sennit' to set up a provider interactively")
	}

	if err := client.InitCoderAgentNonInteractive(ctx); err != nil {
		return fmt.Errorf("failed to initialize agent: %w", err)
	}
	if err := overrideModel(ctx, client, model); err != nil {
		return fmt.Errorf("failed to override model: %w", err)
	}

	sess, err := workspace.ResolveSession(ctx, client, sessionID, useLast, "non-interactive")
	if err != nil {
		return fmt.Errorf("failed to resolve session: %w", err)
	}

	if err := client.SetCurrentSession(ctx, sess.ID); err != nil {
		slog.Debug("Failed to report the run's session", "session_id", sess.ID, "error", err)
	}

	if err := client.AgentRun(ctx, sess.ID, prompt); err != nil {
		return fmt.Errorf("starting turn: %w", err)
	}

	fmt.Fprintln(cmd.OutOrStdout(), sess.ID)
	fmt.Fprintf(cmd.ErrOrStderr(), "Turn started on the sennit daemon. Follow it with `sennit attach --session %s`.\n", sess.ID)
	return nil
}

// runSignalContext derives a context that cancels on SIGINT or SIGTERM, so
// a plain `kill <pid>` (which sends SIGTERM, not SIGKILL) gets the same
// graceful cancellation Ctrl-C does — SIGKILL cannot be caught by
// signal.Notify at all, so listening for os.Kill here was a silent no-op.
func runSignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

func init() {
	runCmd.Flags().BoolP("quiet", "q", false, "Hide spinner")
	runCmd.Flags().BoolP("verbose", "v", false, "Show logs")
	runCmd.Flags().StringP("model", "m", "", "Model to use. Accepts 'model' or 'provider/model' to disambiguate models with the same name across providers")
	runCmd.Flags().StringP("session", "s", "", "Continue a previous session by ID")
	runCmd.Flags().BoolP("continue", "C", false, "Continue the most recent session")
	runCmd.MarkFlagsMutuallyExclusive("session", "continue")
	runCmd.Flags().Bool("detach", false, "Start (or find) this project's daemon, hand it the turn, and exit immediately")
}

// progressBarRefresh is how often the terminal's indeterminate
// progress bar is redrawn while a run is in flight. AgentRunStream
// only emits an AgentRunEvent on text output and on the terminal
// event, so silent tool-call phases would otherwise let the terminal
// hide the bar for inactivity; a ticker keeps it alive independent of
// how chatty the current turn is. Pre-refactor this piggy-backed on
// every raw SSE/message event instead, which happened to be frequent
// enough to serve the same purpose.
const progressBarRefresh = 500 * time.Millisecond

// runAgent drives a single non-interactive turn against ws: it
// initializes the agent, applies any model overrides, resolves the
// target session, and streams the turn to stdout, owning every
// presentation concern (spinner, indeterminate progress bar) so the
// two Workspace implementations don't have to.
func runAgent(
	ctx context.Context,
	ws workspace.Workspace,
	prompt, model string,
	hideSpinner bool,
	continueSessionID string,
	useLast bool,
) error {
	slog.Info("Running in non-interactive mode")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := ws.InitCoderAgentNonInteractive(ctx); err != nil {
		return fmt.Errorf("failed to initialize agent: %w", err)
	}

	if err := overrideModel(ctx, ws, model); err != nil {
		return fmt.Errorf("failed to override model: %w", err)
	}

	sess, err := workspace.ResolveSession(ctx, ws, continueSessionID, useLast, "non-interactive")
	if err != nil {
		return fmt.Errorf("failed to resolve session: %w", err)
	}
	if continueSessionID != "" || useLast {
		slog.Info("Continuing session for non-interactive run", "session_id", sess.ID)
	} else {
		slog.Info("Created session for non-interactive run", "session_id", sess.ID)
	}

	// A non-interactive run works in one session just as an interactive
	// one does - this is it. Without saying so the wake path has nothing
	// to allow, and a run whose turn ended waiting on a delegation would
	// sit there until the process was killed. See
	// agent.Coordinator.SetLiveSession.
	if err := ws.SetCurrentSession(ctx, sess.ID); err != nil {
		slog.Debug("Failed to report the run's session", "session_id", sess.ID, "error", err)
	}

	stderrTTY := term.IsTerminal(os.Stderr.Fd())
	// cfg is nil for any ws that doesn't implement ServerConfigReader —
	// every remote Workspace (grpcws.Client included: ServerConfig
	// deliberately never crosses the wire, see serverConfig's doc
	// comment) as well as any future read-only stand-in. Unlike
	// cfg.ThemeID()/cfg.SpinnerMode(), which are nil-receiver-safe
	// methods, cfg.Options is a plain field access and needs its own nil
	// check here, so it doesn't panic on exactly the workspace this
	// package now also supports.
	cfg := serverConfig(ws)
	progress := cfg == nil || cfg.Options == nil || cfg.Options.Progress == nil || *cfg.Options.Progress

	var spinner *format.Spinner
	if !hideSpinner && stderrTTY {
		t := styles.Theme(cfg.ThemeID())
		spinnerMode, _ := cfg.SpinnerMode()

		spinner = format.NewSpinner(ctx, cancel, spin.Settings{
			Size: 10,
			// Starting label only: AgentRunEvent.Status replaces it with
			// what the agent is actually doing as soon as the turn says
			// anything about itself.
			Label:       "Generating",
			GradColorA:  t.WorkingGradFromColor,
			GradColorB:  t.WorkingGradToColor,
			CycleColors: true,
			Mode:        styles.SpinnerMode(spinnerMode),
		})
		spinner.Start()
	}
	stopSpinner := func() {
		if !hideSpinner && spinner != nil {
			spinner.Stop()
			spinner = nil
		}
	}
	defer stopSpinner()

	// Headless: there is no UI to answer a permission prompt with, so
	// this run must auto-approve everything the turn asks for, including
	// what any delegation it starts asks for (see AgentRunStream's doc
	// comment).
	events, err := ws.AgentRunStream(ctx, sess.ID, prompt, workspace.AgentRunOptions{AutoApprovePermissions: true})
	if err != nil {
		stopSpinner()
		return err
	}

	defer func() {
		if progress && stderrTTY {
			_, _ = fmt.Fprintf(os.Stderr, ansi.ResetProgressBar)
		}
		_, _ = fmt.Fprintln(os.Stdout)
	}()

	var progressTick <-chan time.Time
	if progress && stderrTTY {
		_, _ = fmt.Fprintf(os.Stderr, ansi.SetIndeterminateProgressBar)
		ticker := time.NewTicker(progressBarRefresh)
		defer ticker.Stop()
		progressTick = ticker.C
	}

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				// The channel closed without a terminal event reaching
				// this loop (ev.Done, handled below, always returns
				// first when the producer delivers one). That only
				// happens when our own ctx ended the run — a genuine
				// clean finish always sends Done before closing — so
				// report the cancellation instead of silently returning
				// success and leaving a caller unable to tell a
				// cancelled `sennit run` from a completed one by its
				// exit code.
				stopSpinner()
				return ctx.Err()
			}
			if ev.Status != "" && spinner != nil {
				spinner.SetLabel(ev.Status)
			}
			if ev.TextDelta != "" {
				stopSpinner()
				fmt.Fprint(os.Stdout, ev.TextDelta)
			}
			if ev.Done {
				stopSpinner()
				return workspace.DecodeError(ev.Err)
			}

		case <-progressTick:
			_, _ = fmt.Fprintf(os.Stderr, ansi.SetIndeterminateProgressBar)

		case <-ctx.Done():
			stopSpinner()
			return ctx.Err()
		}
	}
}

// overrideModel resolves the model string and updates the workspace
// configuration. Works against the Workspace interface so -m/--model
// applies uniformly regardless of the concrete implementation. Helper
// (small) model resolution is fully automatic and needs no CLI-side
// override.
func overrideModel(ctx context.Context, ws workspace.Workspace, model string) error {
	if model == "" {
		return nil
	}

	providers := serverConfig(ws).Providers.Copy()

	matches, err := config.FindModelMatches(providers, model)
	if err != nil {
		return err
	}

	found, err := config.ValidateModelMatches(matches, model, "model")
	if err != nil {
		return err
	}
	slog.Info("Overriding model", "provider", found.Provider, "model", found.ModelID)
	if err := ws.OverridePreferredModel(config.SelectedModel{
		Provider: found.Provider,
		Model:    found.ModelID,
	}); err != nil {
		return fmt.Errorf("failed to set model: %w", err)
	}

	return ws.UpdateAgentModel(ctx)
}
