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

		ws, cleanup, err := setupLocalWorkspace(cmd)
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
	progress := ws.Config().Options.Progress == nil || *ws.Config().Options.Progress

	var spinner *format.Spinner
	if !hideSpinner && stderrTTY {
		t := styles.Theme(ws.Config().ThemeID())
		spinnerMode, _ := ws.Config().SpinnerMode()

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
				return ev.Err
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

	providers := ws.Config().Providers.Copy()

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
