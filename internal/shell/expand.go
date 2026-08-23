package shell

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// maxInnerStderrBytes bounds how much stderr from a failing $(...) is
// surfaced in the returned error, to avoid leaking a secret that happened
// to be embedded in a failing inner command.
const maxInnerStderrBytes = 512

// NoUnset controls whether ExpandValue treats unset variables as an
// error. Default false matches bash: $UNSET expands to "". Store true
// to re-enable strict mode globally. Not exposed in sennit.json; this is
// an internal escape hatch in case the lenient default turns out to be
// the wrong call.
//
// Declared atomic because ExpandValue is invoked concurrently (multiple
// MCP / LSP / provider loads in flight at startup, hook execution, etc.)
// and an unsynchronised read/write pair is a data race under the Go
// memory model regardless of test-level happens-before reasoning. The
// atomic load on the hot path is negligible against the cost of parsing
// and running through mvdan.
var NoUnset atomic.Bool

// ExpandValue expands shell-style substitutions in a single config value.
//
// Supported constructs match the bash tool:
//
//   - $VAR and ${VAR}.
//   - ${VAR:-default} / ${VAR:+alt} / ${VAR:?msg}.
//   - $(command) with full quoting and nesting.
//   - escaped and quoted strings ("...", '...').
//
// Trust boundary: $(...) here runs with no BlockFunc deny-list. That
// deny-list (internal/agent/tools.blockFuncs) exists to stop an LLM's
// bash tool calls from running commands the user didn't ask for; it has
// no bearing on ExpandValue, whose input is the config author's own
// sennitrc/sennit.json — the same person who can already run arbitrary
// commands via hooks, mcp, and lsp entries in that same file. Denying a
// subset of commands inside $(...) here would be inconsistent (every
// other config mechanism stays unrestricted) without closing any real
// gap, so ExpandValue intentionally runs $(...) unrestricted rather
// than threading a deny-list through.
//
// Contract:
//
//   - Returns exactly one string. No field splitting, no globbing, no
//     pathname generation. Multi-word command output is preserved
//     verbatim; it is never split into multiple values.
//   - Nounset is off by default, matching bash: unset variables expand
//     to "". Opt in to strict behaviour per-reference with
//     ${VAR:?msg}, which errors loudly when VAR is unset regardless of
//     the global toggle. Flip the global default via
//     shell.NoUnset.Store(true) as an internal escape hatch.
//   - Embedded whitespace and newlines in the input are preserved
//     verbatim. Command substitution strips trailing newlines only
//     (POSIX), never leading or internal whitespace.
//   - Errors wrap the failing inner command's exit code and a bounded
//     prefix of its stderr. Callers that surface the error to users
//     should additionally scrub it for the original template text.
func ExpandValue(ctx context.Context, value string, env []string) (string, error) {
	// Fast path: a value with no shell metacharacters expands to itself.
	// Parsing and running it through the interpreter would produce the
	// same string at significant cost. Most config values (literal API
	// keys, fixed URLs) hit this path, which matters because ExpandValue
	// runs over every provider/MCP/LSP value on each config reload.
	if !strings.ContainsAny(value, "$`\\'\"") {
		return value, nil
	}

	// Parse the value as a here-doc style word: no word splitting, no
	// globbing, but full support for $VAR, ${VAR...}, $(...), and
	// quoted/escaped strings.
	word, err := syntax.NewParser().Document(strings.NewReader(value))
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}

	// Build a minimal Shell value purely to reuse its handler chain
	// (builtins, optional Go coreutils) inside $(...). blockFuncs is
	// left nil — see the trust-boundary note on ExpandValue above.
	// We deliberately skip NewShell so the passed-in env is used
	// verbatim, with no SENNIT/AGENT/AI_AGENT injection: callers of
	// ExpandValue control the env, and nounset must treat any name
	// not in env as unset.
	//
	// The working directory is only needed for $(...) command
	// substitution, so we resolve it lazily on first use. Values that
	// only reference variables or use quoting (the common case) avoid
	// the os.Getwd() syscall entirely.
	s := &Shell{
		env:    env,
		logger: noopLogger{},
	}
	cwdResolved := false

	strict := NoUnset.Load()

	var stderrBuf bytes.Buffer
	cfg := &expand.Config{
		Env:     expand.ListEnviron(env...),
		NoUnset: strict,
		CmdSubst: func(w io.Writer, cs *syntax.CmdSubst) error {
			if !cwdResolved {
				s.cwd, _ = os.Getwd()
				cwdResolved = true
			}
			stderrBuf.Reset()
			runnerOpts := []interp.RunnerOption{
				interp.StdIO(nil, w, &stderrBuf),
				interp.Interactive(false),
				interp.Env(expand.ListEnviron(env...)),
				interp.Dir(s.cwd),
				execHandlerOption(s.blockFuncs),
			}
			if strict {
				// Match the outer NoUnset: an unset $VAR inside
				// $(...) is also an error, not a silent empty.
				runnerOpts = append(runnerOpts, interp.Params("-u"))
			}
			runner, rerr := interp.New(runnerOpts...)
			if rerr != nil {
				return rerr
			}
			if rerr := runner.Run(ctx, &syntax.File{Stmts: cs.Stmts}); rerr != nil {
				return wrapCmdSubstErr(rerr, stderrBuf.Bytes())
			}
			return nil
		},
		// ReadDir / ReadDir2 left nil: globbing is disabled.
	}

	return expand.Document(cfg, word)
}

// wrapCmdSubstErr attaches a bounded prefix of the inner command's stderr
// to the original error, if any.
func wrapCmdSubstErr(err error, stderrBytes []byte) error {
	msg := sanitizeStderr(stderrBytes)
	if msg == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, msg)
}

// sanitizeStderr trims, bounds, and scrubs non-printable bytes from the
// stderr of a failing command so the result is safe to include in an
// error message shown to the user.
//
// This walks runes, not bytes: a command's stderr is frequently non-ASCII
// (a translated error message, a path with accented characters, etc.), and
// ranging over the []byte would test each UTF-8 continuation byte against
// the ASCII printable range on its own, scrubbing every multi-byte rune to
// a run of '?'. Only genuinely non-printable runes and invalid encoding
// are replaced.
func sanitizeStderr(b []byte) string {
	b = bytes.TrimRight(b, "\n")
	if len(b) > maxInnerStderrBytes {
		b = b[:maxInnerStderrBytes]
	}
	var out strings.Builder
	out.Grow(len(b))
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		switch {
		case r == '\t' || r == '\n' || unicode.IsPrint(r):
			out.WriteRune(r)
		default:
			// Covers control characters and invalid UTF-8 (DecodeRune
			// returns utf8.RuneError with size 1 for a single bad byte).
			out.WriteByte('?')
		}
		i += size
	}
	return out.String()
}
