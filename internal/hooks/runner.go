package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rave-soft/sennit/internal/shell"
)

// abandonMargin is the slack given to abandonGrace on top of
// shell.KillTimeout, covering the time between SIGKILL landing and the
// killed process actually exiting and runOne's goroutine returning from
// Wait. It is not a timing guess: it exists only to absorb scheduling
// noise around a boundary that is otherwise exact.
const abandonMargin = 500 * time.Millisecond

// abandonGrace is how long runOne waits after ctx cancellation for the
// shell goroutine to yield before returning control to the caller and
// letting the goroutine finish on its own.
//
// It must be strictly greater than shell.KillTimeout: on cancellation the
// shell sends SIGINT and only escalates to SIGKILL after KillTimeout, so a
// hook that ignores SIGINT (an empty trap on INT, or a Python/Node process
// with its own handler) will not die until SIGKILL lands. Deriving abandonGrace
// from shell.KillTimeout instead of an independent literal means the two
// can't drift apart again — if KillTimeout changes, this changes with it.
// If this file no longer imports shell.KillTimeout, that's a sign someone
// has broken the coupling this comment describes.
const abandonGrace = shell.KillTimeout + abandonMargin

// compiledHook pairs a HookConfig with its compiled matcher regex. A nil
// matcher means "match every tool".
type compiledHook struct {
	cfg     Hook
	matcher *regexp.Regexp
}

// Runner executes hook commands and aggregates their results.
type Runner struct {
	hooks      []compiledHook
	cwd        string
	projectDir string
	runShell   func(context.Context, shell.RunOptions) error
	abandonFor time.Duration
}

// NewRunner creates a Runner from the given hook configs. Each hook's
// Matcher is compiled here so the Runner is self-sufficient; callers do
// not have to pre-compile matchers on the config, and reloads or merges
// that rebuild HookConfig values can't silently strip compiled state.
//
// Hooks whose matcher fails to compile are skipped with a warning rather
// than treated as match-everything. ValidateHooks is expected to have
// caught syntax errors earlier, so this is defense in depth.
func NewRunner(hooks []Hook, cwd, projectDir string) *Runner {
	compiled := make([]compiledHook, 0, len(hooks))
	for _, h := range hooks {
		ch := compiledHook{cfg: h}
		if h.Matcher != "" {
			re, err := regexp.Compile(h.Matcher)
			if err != nil {
				slog.Warn(
					"Hook matcher failed to compile; skipping hook",
					"matcher", h.Matcher,
					"command", h.Command,
					"error", err,
				)
				continue
			}
			ch.matcher = re
		}
		compiled = append(compiled, ch)
	}
	return &Runner{
		hooks:      compiled,
		cwd:        cwd,
		projectDir: projectDir,
		runShell:   shell.Run,
		abandonFor: abandonGrace,
	}
}

// Hooks returns the hook configs the runner was created with, in config
// order. Hooks whose matcher failed to compile at construction are
// omitted. Intended for diagnostics; callers should not rely on ordering
// or identity beyond that.
func (r *Runner) Hooks() []Hook {
	out := make([]Hook, len(r.hooks))
	for i, h := range r.hooks {
		out[i] = h.cfg
	}
	return out
}

// Run executes all matching hooks for the given event and tool, returning
// an aggregated result. The returned error is non-nil only when a hook's
// shell process failed to yield after cancellation and its goroutine had
// to be abandoned (see runOne) — a resource-leak condition worth
// surfacing on its own, distinct from an ordinary hook failure, which
// aggregate folds into DecisionNone rather than an error.
func (r *Runner) Run(ctx context.Context, eventName, sessionID, toolName, toolInputJSON string) (AggregateResult, error) {
	matching := r.matchingHooks(toolName)
	if len(matching) == 0 {
		return AggregateResult{Decision: DecisionNone}, nil
	}

	// Deduplicate exact-duplicate hook entries (e.g. the same hook
	// inherited from both a global and a project config layer). Keying
	// on the whole config, not just Command, matters: two hooks can
	// legitimately share a command string while differing in matcher,
	// timeout, or display name, and deduping by Command alone would
	// silently drop one of them, leaving the survivor's timeout and name
	// applied to what the config author intended as two separate hooks.
	seen := make(map[Hook]bool, len(matching))
	var deduped []Hook
	for _, h := range matching {
		if seen[h] {
			continue
		}
		seen[h] = true
		deduped = append(deduped, h)
	}

	envVars := BuildEnv(eventName, toolName, sessionID, r.cwd, r.projectDir, toolInputJSON)
	payload := BuildPayload(eventName, sessionID, r.cwd, toolName, toolInputJSON)

	results := make([]HookResult, len(deduped))
	errs := make([]error, len(deduped))
	var wg sync.WaitGroup
	wg.Add(len(deduped))

	for i, h := range deduped {
		go func(idx int, hook Hook) {
			defer wg.Done()
			results[idx], errs[idx] = r.runOne(ctx, hook, envVars, payload)
		}(i, h)
	}
	wg.Wait()

	agg := aggregate(results, toolInputJSON)
	agg.Hooks = make([]HookInfo, len(deduped))
	for i, h := range deduped {
		agg.Hooks[i] = HookInfo{
			Name:         h.DisplayName(),
			Matcher:      h.Matcher,
			Decision:     results[i].Decision.String(),
			Halt:         results[i].Halt,
			Reason:       results[i].Reason,
			InputRewrite: results[i].UpdatedInput != "",
		}
	}
	slog.Info(
		"Hook completed",
		"event", eventName,
		"tool", toolName,
		"hooks", len(deduped),
		"decision", agg.Decision.String(),
	)
	return agg, errors.Join(errs...)
}

// matchingHooks returns hooks whose matcher matches the tool name (or has
// no matcher, which matches everything).
func (r *Runner) matchingHooks(toolName string) []Hook {
	var matched []Hook
	for _, h := range r.hooks {
		if h.matcher == nil || h.matcher.MatchString(toolName) {
			matched = append(matched, h.cfg)
		}
	}
	return matched
}

// runOne executes a single hook command and returns its result. The
// returned error is non-nil only on the abandon path below — every other
// outcome (deny, halt, timeout, non-zero exit) is reported through the
// HookResult itself, matching how the rest of the hook pipeline treats a
// misbehaving hook as data rather than a Go error.
//
// Execution goes through Sennit's embedded POSIX shell (shell.Run) so the
// same interpreter, builtins, and coreutils are visible to hooks as to
// the bash tool. BlockFuncs are intentionally omitted: hooks are
// user-authored config that carry the same trust as a shell alias.
//
// A hook that fails to yield after its deadline has passed is abandoned
// after abandonGrace so the caller never blocks longer than
// timeout + abandonGrace. Ownership of the stdout and stderr buffers is
// strictly single-goroutine:
//   - before receiving from `done`, only the goroutine writes to them;
//   - after `done` delivers a value, the goroutine is finished and the
//     outer frame reads them;
//   - on the abandon path, the goroutine may still be writing and the
//     outer frame must not touch them again.
func (r *Runner) runOne(parentCtx context.Context, hook Hook, envVars []string, payload []byte) (HookResult, error) {
	timeout := hook.TimeoutDuration()
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	// mvdan.cc/sh does not join `cmd &` before Run returns (see
	// [shell.RunAndCapture]'s comment for the same issue), so a hook that
	// backgrounds a job can still be writing after runShell returns. Use
	// the mutex-protected SyncBuffer instead of a plain bytes.Buffer to
	// avoid a data race on the reads below.
	var stdout, stderr shell.SyncBuffer
	done := make(chan error, 1)
	go func() {
		done <- r.runShell(ctx, shell.RunOptions{
			Command: hook.Command,
			Cwd:     r.cwd,
			Env:     envVars,
			Stdin:   bytes.NewReader(payload),
			Stdout:  &stdout,
			Stderr:  &stderr,
		})
	}()

	var err error
	select {
	case err = <-done:
		// Normal path: goroutine has finished, buffers are safe to read.
	case <-ctx.Done():
		select {
		case err = <-done:
			// Interpreter yielded within the grace period; safe to read.
		case <-time.After(r.abandonFor):
			slog.Warn(
				"Hook did not yield after cancel; abandoning goroutine",
				"command", hook.Command,
				"timeout", timeout,
			)
			// The goroutine may still be writing to stdout/stderr; do
			// not read either buffer below this point.
			return HookResult{Decision: DecisionNone}, fmt.Errorf(
				"hook %q did not yield after cancellation; goroutine abandoned",
				hook.Command,
			)
		}
	}

	if shell.IsInterrupt(err) {
		// Distinguish timeout from parent cancellation.
		if parentCtx.Err() != nil {
			slog.Debug("Hook cancelled by parent context", "command", hook.Command)
		} else {
			slog.Warn("Hook timed out", "command", hook.Command, "timeout", timeout)
		}
		return HookResult{Decision: DecisionNone}, nil
	}

	if err != nil {
		exitCode := shell.ExitCode(err)
		switch exitCode {
		case 2:
			// Exit code 2 = block this tool call. Stderr is the reason.
			reason := strings.TrimSpace(stderr.String())
			if reason == "" {
				reason = "blocked by hook"
			}
			return HookResult{
				Decision: DecisionDeny,
				Reason:   reason,
			}, nil
		case HaltExitCode:
			// Exit code 49 = halt the whole turn. Stderr is the reason.
			reason := strings.TrimSpace(stderr.String())
			if reason == "" {
				reason = "turn halted by hook"
			}
			return HookResult{
				Decision: DecisionDeny,
				Halt:     true,
				Reason:   reason,
			}, nil
		default:
			// Other non-zero exits are non-blocking errors.
			slog.Warn(
				"Hook failed with non-blocking error",
				"command", hook.Command,
				"exit_code", exitCode,
				"stderr", strings.TrimSpace(stderr.String()),
				"error", err,
			)
			return HookResult{Decision: DecisionNone}, nil
		}
	}

	// Exit code 0 — parse stdout JSON.
	result := parseStdout(stdout.String())
	slog.Debug(
		"Hook executed",
		"command", hook.Command,
		"decision", result.Decision.String(),
	)
	return result, nil
}
