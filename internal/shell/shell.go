// Package shell provides cross-platform shell execution capabilities.
//
// This package provides Shell instances for executing commands with their own
// working directory and environment. Each shell execution is independent.
//
// WINDOWS COMPATIBILITY:
// This implementation provides POSIX shell emulation (mvdan.cc/sh/v3) even on
// Windows. Commands should use forward slashes (/) as path separators to work
// correctly on all platforms.
package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/charmbracelet/x/exp/slice"
	"github.com/rave-soft/sennit/internal/brand"
	sennitenv "github.com/rave-soft/sennit/internal/env"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// ShellType represents the type of shell to use
type ShellType int

const (
	ShellTypePOSIX ShellType = iota
	ShellTypeCmd
	ShellTypePowerShell
)

// SennitEnvMarkers returns a fresh slice of the environment variables that
// Sennit unconditionally sets on every shell it spawns — both the interactive
// bash tool's [Shell] and the hook runner's [Run] calls. Tools that want to
// detect "am I being invoked by an AI agent?" can check any of these.
// Keeping them in one place guarantees the two shell surfaces cannot drift.
// A fresh slice is returned on every call so callers may append freely.
func SennitEnvMarkers() []string {
	return []string{
		brand.EnvName + "=1",
		"AGENT=" + brand.Slug,
		"AI_AGENT=" + brand.Slug,
	}
}

// Logger interface for optional logging
type Logger interface {
	InfoPersist(msg string, keysAndValues ...any)
}

// noopLogger is a logger that does nothing
type noopLogger struct{}

func (noopLogger) InfoPersist(msg string, keysAndValues ...any) {}

// BlockFunc is a function that determines if a command should be blocked
type BlockFunc func(args []string) bool

// Shell provides cross-platform shell execution with optional state persistence
type Shell struct {
	env        []string
	cwd        string
	mu         sync.Mutex
	logger     Logger
	blockFuncs []BlockFunc
}

// Options for creating a new shell
type Options struct {
	WorkingDir string
	Env        []string
	Logger     Logger
	BlockFuncs []BlockFunc
}

// NewShell creates a new shell instance with the given options
func NewShell(opts *Options) *Shell {
	if opts == nil {
		opts = &Options{}
	}

	cwd := opts.WorkingDir
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	env := opts.Env
	if env == nil {
		env = os.Environ()
	}

	// Strip herdr pane-ownership vars so subprocesses (including test
	// binaries and nested sennit instances) can't attach to or release
	// the parent pane's agent authority.
	env = sennitenv.WithoutHerdrEnv(env)

	// Allow tools to detect execution by Sennit.
	env = append(env, SennitEnvMarkers()...)

	logger := opts.Logger
	if logger == nil {
		logger = noopLogger{}
	}

	return &Shell{
		cwd:        cwd,
		env:        env,
		logger:     logger,
		blockFuncs: opts.BlockFuncs,
	}
}

// Exec executes a command in the shell
func (s *Shell) Exec(ctx context.Context, command string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.exec(ctx, command)
}

// ExecStream executes a command in the shell with streaming output to provided writers
func (s *Shell) ExecStream(ctx context.Context, command string, stdout, stderr io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.execStream(ctx, command, stdout, stderr)
}

// GetWorkingDir returns the current working directory
func (s *Shell) GetWorkingDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cwd
}

// SetWorkingDir sets the working directory
func (s *Shell) SetWorkingDir(dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Verify the directory exists
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("directory does not exist: %w", err)
	}

	s.cwd = dir
	return nil
}

// GetEnv returns a copy of the environment variables
func (s *Shell) GetEnv() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	env := make([]string, len(s.env))
	copy(env, s.env)
	return env
}

// SetEnv sets an environment variable
func (s *Shell) SetEnv(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Update or add the environment variable
	keyPrefix := key + "="
	for i, env := range s.env {
		if strings.HasPrefix(env, keyPrefix) {
			s.env[i] = keyPrefix + value
			return
		}
	}
	s.env = append(s.env, keyPrefix+value)
}

// SetBlockFuncs sets the command block functions for the shell
func (s *Shell) SetBlockFuncs(blockFuncs []BlockFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blockFuncs = blockFuncs
}

// CommandsBlocker creates a BlockFunc that blocks exact command matches.
// This is the model-facing deny list, not a sandbox boundary — nothing it
// blocks was ever on the permission-prompt-free safe list, so a command
// that slips past it still gets a permission prompt. The point of matching
// carefully here is only that the deny list means what it says.
func CommandsBlocker(cmds []string) BlockFunc {
	bannedSet := make(map[string]struct{})
	for _, cmd := range cmds {
		bannedSet[cmd] = struct{}{}
	}

	return func(args []string) bool {
		if len(args) == 0 {
			return false
		}
		_, ok := bannedSet[realCommandName(args)]
		return ok
	}
}

// commandWrapperPrefixes are argv[0] names that run another command rather
// than doing anything themselves. Comparing bannedCommands against args[0]
// verbatim let `env curl …`, `timeout 5 curl …`, `xargs curl` and a plain
// path like `/usr/bin/curl` all reach a denied command with no match, since
// none of them is literally "curl". realCommandName unwraps these to find
// the program actually being run.
var commandWrapperPrefixes = map[string]bool{
	"env": true, "nice": true, "nohup": true, "timeout": true,
	"xargs": true, "command": true, "exec": true,
}

// realCommandName returns the base name of the program args actually runs,
// unwrapping a chain of command-wrapper prefixes (env, nice, nohup,
// timeout, xargs, command, exec) first. It is a best-effort match for the
// deny list, not a sandbox: an option this doesn't recognize, or one of
// these wrappers used in an unusual way, simply stops the unwrap early and
// the wrapper's own name is matched instead.
func realCommandName(args []string) string {
	for len(args) > 0 && commandWrapperPrefixes[filepath.Base(args[0])] {
		wrapper := filepath.Base(args[0])
		args = args[1:]
		switch wrapper {
		case "env":
			// `env FOO=1 BAR=2 -u BAZ curl …`: skip assignments and
			// options. -u and -C consume one separate argument.
			for len(args) > 0 {
				if isEnvAssignment(args[0]) {
					args = args[1:]
					continue
				}
				if !strings.HasPrefix(args[0], "-") {
					break
				}
				option := args[0]
				args = args[1:]
				if (option == "-u" || option == "--unset" || option == "-C" || option == "--chdir") && len(args) > 0 {
					args = args[1:]
				}
			}
		case "timeout":
			// `timeout --signal=KILL 5 curl …`: skip flags, then
			// the duration itself — it is a separate token, not
			// part of the command.
			for len(args) > 0 && strings.HasPrefix(args[0], "-") {
				args = args[1:]
			}
			if len(args) > 0 {
				args = args[1:]
			}
		case "nice", "xargs":
			// Both wrappers accept options with separate values (-n 10).
			// Consume the documented value-taking forms before locating the
			// command they will execute.
			for len(args) > 0 && strings.HasPrefix(args[0], "-") {
				option := args[0]
				args = args[1:]
				if wrapperOptionTakesValue(wrapper, option) && len(args) > 0 {
					args = args[1:]
				}
			}
		default:
			// nohup, command, exec only need their leading flags skipped.
			for len(args) > 0 && strings.HasPrefix(args[0], "-") {
				args = args[1:]
			}
		}
	}
	if len(args) == 0 {
		return ""
	}
	return filepath.Base(args[0])
}

// isEnvAssignment reports whether tok has the shell's VAR=value shape:
// an identifier (letters, digits, underscore, not starting with a digit)
// followed by "=". env accepts any number of these before the command it
// runs.
// wrapperOptionTakesValue reports value-taking options that may appear before
// the wrapped command. Options in their --name=value form carry their value
// already, so only their separate-value spelling is listed.
func wrapperOptionTakesValue(wrapper, option string) bool {
	switch wrapper {
	case "nice":
		return option == "-n" || option == "--adjustment"
	case "xargs":
		switch option {
		case "-n", "-s", "-P", "-E", "-e", "-I", "-i", "-L", "-l", "-a", "-d":
			return true
		}
	}
	return false
}

func isEnvAssignment(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := tok[i]
		isLetter := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		if isLetter || (i > 0 && isDigit) {
			continue
		}
		return false
	}
	return true
}

// ArgumentsBlocker creates a BlockFunc that blocks specific subcommand
func ArgumentsBlocker(cmd string, args []string, flags []string) BlockFunc {
	return func(parts []string) bool {
		if len(parts) == 0 || parts[0] != cmd {
			return false
		}

		argParts, flagParts := splitArgsFlags(parts[1:])
		if len(argParts) < len(args) || len(flagParts) < len(flags) {
			return false
		}

		argsMatch := slices.Equal(argParts[:len(args)], args)
		flagsMatch := slice.IsSubset(flags, flagParts)

		return argsMatch && flagsMatch
	}
}

func splitArgsFlags(parts []string) (args []string, flags []string) {
	args = make([]string, 0, len(parts))
	flags = make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.HasPrefix(part, "-") {
			// Extract flag name before '=' if present
			flag := part
			if before, _, ok := strings.Cut(part, "="); ok {
				flag = before
			}
			flags = append(flags, flag)
		} else {
			args = append(args, part)
		}
	}
	return args, flags
}

// newInterp creates a new interpreter with the current shell state. A nil
// stdin is equivalent to an empty input stream.
func (s *Shell) newInterp(stdin io.Reader, stdout, stderr io.Writer) (*interp.Runner, error) {
	return newRunner(s.cwd, s.env, stdin, stdout, stderr, s.blockFuncs)
}

// updateShellFromRunner updates the shell from the interpreter after execution.
func (s *Shell) updateShellFromRunner(runner *interp.Runner) {
	s.cwd = runner.Dir
	s.env = s.env[:0]
	for name, vr := range runner.Vars {
		if vr.Exported {
			s.env = append(s.env, name+"="+vr.Str)
		}
	}
}

// execCommon is the shared implementation for executing commands
func (s *Shell) execCommon(ctx context.Context, command string, stdout, stderr io.Writer) (err error) {
	var runner *interp.Runner
	defer func() {
		panicked := false
		if r := recover(); r != nil {
			panicked = true
			err = fmt.Errorf("command execution panic: %v", r)
		}
		// Not after a panic: the runner may have died before it populated
		// Vars, and updateShellFromRunner rebuilds s.env from exactly
		// that map — so one panicking command wiped the shell's
		// environment for every command after it.
		if runner != nil && !panicked {
			s.updateShellFromRunner(runner)
		}
		s.logger.InfoPersist("command finished", "command", command, "err", err)
	}()

	line, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return fmt.Errorf("could not parse command: %w", err)
	}

	runner, err = s.newInterp(nil, stdout, stderr)
	if err != nil {
		return fmt.Errorf("could not run command: %w", err)
	}

	err = runner.Run(ctx, line)
	return err
}

// exec executes commands using a cross-platform shell interpreter.
func (s *Shell) exec(ctx context.Context, command string) (string, string, error) {
	// SyncBuffer, not bytes.Buffer: mvdan.cc/sh does not join a
	// backgrounded job before Run returns, so a command containing
	// "cmd &" can still be writing here after these are read below —
	// the same race Run and the hook runner use SyncBuffer for.
	var stdout, stderr SyncBuffer
	err := s.execCommon(ctx, command, &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

// execStream executes commands using POSIX shell emulation with streaming output
func (s *Shell) execStream(ctx context.Context, command string, stdout, stderr io.Writer) error {
	return s.execCommon(ctx, command, stdout, stderr)
}

// IsInterrupt checks if an error is due to interruption
func IsInterrupt(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// ExitCode extracts the exit code from an error
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := errors.AsType[interp.ExitStatus](err); ok {
		return int(exitErr)
	}
	return 1
}
