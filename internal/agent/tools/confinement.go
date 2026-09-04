package tools

// This file holds the static bash parse that decides whether a command
// reaches outside a confined workspace. It is split out of bash.go because
// it is a self-contained analysis — parsing the command text with mvdan/sh
// and walking the resulting AST for literal absolute paths — rather than
// part of the tool's request/response handling, and it is large enough on
// its own (the doc comment on bashConfinementRefusal spells out exactly
// what the walk does and does not catch) to be worth reading in isolation.

import (
	"fmt"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/permission"
)

// bashConfinementRefusal is confinementRefusal's counterpart for the
// command text itself. The working-dir check only shuts the front door —
// it stops a command from being *rooted* outside the workspace boundary,
// but a command rooted inside it can still touch an absolute path
// elsewhere: `cp x /main/repo/…`, `echo x > /main/repo/y`. This parses
// command with mvdan/sh (the same parser package `shell` runs the command
// with) and walks the AST for literal absolute-path arguments and redirect
// targets that resolve outside the boundary.
//
// This is a static best-effort check, not a sandbox, and it is
// deliberately narrow about what it claims to catch:
//   - Only words it can resolve without running anything: plain literal
//     text, and single/double-quoted text made entirely of literals. A
//     parameter expansion ($VAR), command substitution ($(...) or `...`),
//     or arithmetic expansion anywhere in the word means the word is
//     skipped, not "resolved" — pretending to evaluate those statically
//     would be worse than not trying.
//   - Glob characters (*, ?, [) are left alone for the same reason: what
//     they expand to depends on the filesystem at the moment the command
//     actually runs, not on the literal text.
//   - A path that is relative in the command text but escapes via a
//     symlink resolved only at run time is invisible here.
//   - The command name itself (argv[0] of each simple command) is not
//     checked: `/usr/bin/env python3` names a binary to run, not a target
//     being written to.
//   - Arguments to a command known to only read them (readOnlyCommands,
//     and git's inspecting subcommands) are not checked either: the
//     boundary keeps changes in, not the thread from looking out — the
//     view/grep tools already read anywhere — and refusing `cat /etc/hosts`
//     only sent the model looking for a way around. Redirects on those
//     commands are still checked. /dev/null is never outside.
//
// None of that is a gap in this function so much as the reason this is a
// confinement boundary rather than a sandbox.
func bashConfinementRefusal(permissions permission.Requester, command string) (message string, refused, permissionRequired bool) {
	if permissions == nil {
		return "", false, false
	}
	boundary := permissions.ConfinedDir()
	if boundary == "" {
		return "", false, false
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return "", false, true
	}

	var outsidePath string
	syntax.Walk(file, func(node syntax.Node) bool {
		if word, ok := node.(*syntax.Word); ok && wordRequiresPermission(word, command) {
			permissionRequired = true
		}
		if outsidePath != "" {
			return false
		}
		switch n := node.(type) {
		case *syntax.CallExpr:
			if len(n.Args) < 2 {
				return true
			}
			// A command that only ever reads its arguments cannot carry a
			// change out of the workspace through them, so its arguments
			// are not checked — its redirects still are (a separate
			// *syntax.Redirect node), since `cat f > /outside/g` writes
			// through the redirect, not through cat.
			if readsOnlyFromArgs(n.Args) {
				return true
			}
			for _, w := range n.Args[1:] {
				if p, found := literalAbsPathOutside(w, boundary); found {
					outsidePath = p
					return false
				}
			}
		case *syntax.Redirect:
			if p, found := literalAbsPathOutside(n.Word, boundary); found {
				outsidePath = p
				return false
			}
		}
		return true
	})
	if outsidePath == "" {
		return "", false, permissionRequired
	}
	return fmt.Sprintf(
		"refusing to run: %s is outside this workspace. "+
			"This workspace is isolated to %s: bash refuses any command that names "+
			"an absolute path outside it (as an argument or a redirect target), "+
			"except arguments to read-only commands such as cat, diff, ls, grep "+
			"and git diff/log/show, and /dev/null anywhere. "+
			"To show a whole file as a diff use `git diff --no-index /dev/null <file>`; "+
			"to read a file outside the workspace use the read tool.",
		outsidePath, boundary,
	), true, permissionRequired
}

// readOnlyCommands are commands whose arguments are only ever read —
// never created, written, moved or removed — so an absolute path outside
// the boundary among them cannot carry a change out of the workspace. The
// list is deliberately conservative: a command with any writing mode at
// all (sed -i, sort -o, find -delete, xxd with an output file, tee) is
// left out, even though its common use is read-only. What a command does
// with a redirect is not its concern — redirects are checked separately.
// yq is deliberately absent even though its ordinary use is read-only:
// `yq -i expr f.yaml` edits the file in place, the same shape of exception
// this list's own rule already makes for sed -i and sort -o. jq has no
// in-place flag, so it stays. tree stays too, since plain `tree` is common
// enough to be worth keeping, but see writeDisqualifyingFlags below for the
// same problem with `tree -o`.
var readOnlyCommands = map[string]bool{
	"cat": true, "head": true, "tail": true, "less": true, "more": true,
	"wc": true, "diff": true, "cmp": true, "comm": true, "file": true, "stat": true,
	"ls": true, "tree": true, "du": true, "df": true,
	"md5sum": true, "sha1sum": true, "sha256sum": true, "sha512sum": true, "cksum": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true,
	"cut": true, "tr": true, "strings": true, "jq": true,
	"nl": true, "tac": true, "rev": true, "column": true, "fold": true, "expand": true,
	"od": true, "hexdump": true, "realpath": true, "readlink": true,
	"basename": true, "dirname": true, "test": true, "[": true,
	"which": true, "type": true, "pwd": true, "true": true, "false": true, "echo": true, "printf": true,
}

// writeDisqualifyingFlags names, per command in readOnlyCommands, flags
// that give an otherwise-read-only command a writing mode: `tree -o path`
// writes the listing to path instead of only reading the directory tree.
// A command with no entry here has no such flag, so every invocation
// readOnlyCommands allows is read-only.
var writeDisqualifyingFlags = map[string]map[string]bool{
	"tree": {"-o": true},
}

// readOnlyGitSubcommands are the git subcommands that only inspect the
// repository: none of them writes the working tree, the index, or refs,
// whatever path arguments they are given (`git diff --no-index /dev/null
// f` is the model's usual way of showing a new file whole).
var readOnlyGitSubcommands = map[string]bool{
	"diff": true, "log": true, "show": true, "status": true, "blame": true,
	"grep": true, "ls-files": true, "ls-tree": true, "cat-file": true,
	"rev-parse": true, "rev-list": true, "shortlog": true, "describe": true,
	// Not here: branch, tag, stash, worktree — each lists, but each also
	// creates or deletes.
}

// readsOnlyFromArgs reports whether the simple command args (argv, as
// syntax.Words) is one whose arguments are known to be only read — see
// readOnlyCommands. The command name must itself be literal (`$CMD ...`
// is opaque). For git, the first argument must directly be a read-only
// subcommand, optionally after --no-pager: a global option such as -C,
// --git-dir or --work-tree repoints the command at another repository,
// so any of those disqualifies.
func readsOnlyFromArgs(args []*syntax.Word) bool {
	name, ok := literalWordValue(args[0])
	if !ok {
		return false
	}
	name = filepath.Base(name)
	if readOnlyCommands[name] {
		if flags, gated := writeDisqualifyingFlags[name]; gated && hasFlag(args[1:], flags) {
			return false
		}
		return true
	}
	if name != "git" {
		return false
	}
	subcommand := ""
	rest := args[1:]
	for i, w := range args[1:] {
		arg, ok := literalWordValue(w)
		if !ok {
			return false
		}
		if arg == "--no-pager" {
			continue
		}
		subcommand = arg
		rest = args[1+i+1:]
		break
	}
	if !readOnlyGitSubcommands[subcommand] {
		return false
	}
	// A subcommand that only inspects the repository can still be handed
	// a flag that names a write target or a program to run: `git diff
	// --output=../x` truncates x, `git grep -O cmd` runs cmd on every
	// matched file (see the matching gate in safe.go). Disqualify the
	// invocation so the caller falls back to checking these arguments
	// like any other command's, instead of skipping them as read-only.
	for _, w := range rest {
		arg, ok := literalWordValue(w)
		if !ok {
			// An argument this static parse can't resolve (a
			// variable, a substitution) might itself be
			// "--output=..." or "-O": treat it the same as every
			// other unresolvable word in this function and refuse
			// to call the invocation read-only.
			return false
		}
		flag, _, _ := strings.Cut(arg, "=")
		if flag == "--output" {
			return false
		}
		if subcommand == "grep" && (flag == "-O" || flag == "--open-files-in-pager") {
			return false
		}
	}
	return true
}

// hasFlag reports whether any of args carries one of flags (compared on
// the part before "="), the same check the git branch below makes for
// --output and -O. It fails closed on an argument this static parse can't
// resolve — a variable or substitution might itself expand to the flag
// being looked for — the same reasoning as literalWordValue's other
// callers in this file.
func hasFlag(args []*syntax.Word, flags map[string]bool) bool {
	for _, w := range args {
		arg, ok := literalWordValue(w)
		if !ok {
			return true
		}
		flag, _, _ := strings.Cut(arg, "=")
		if flags[flag] {
			return true
		}
	}
	return false
}

// literalAbsPathOutside reports the absolute path w statically evaluates
// to, if any, when it resolves outside boundary. See
// bashConfinementRefusal for what this deliberately does not attempt.
func wordRequiresPermission(word *syntax.Word, command string) bool {
	if _, literal := literalWordValue(word); !literal {
		return true
	}
	start, end := int(word.Pos().Offset()), int(word.End().Offset())
	if start < 0 || end > len(command) || start >= end {
		return true
	}
	quoted := byte(0)
	escaped := false
	for index := start; index < end; index++ {
		char := command[index]
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' && quoted != '\'' {
			escaped = true
			continue
		}
		if char == '\'' || char == '"' {
			switch quoted {
			case 0:
				quoted = char
			case char:
				quoted = 0
			}
			continue
		}
		if quoted == 0 && (char == '*' || char == '?' || char == '[') {
			return true
		}
	}
	return false
}

func literalAbsPathOutside(w *syntax.Word, boundary string) (path string, found bool) {
	lit, ok := literalWordValue(w)
	if !ok || lit == "" {
		return "", false
	}
	if strings.ContainsAny(lit, "*?[") {
		return "", false
	}
	if !filepathext.SmartIsAbs(lit) {
		return "", false
	}
	// /dev/null is the one path outside every boundary that nothing can
	// be carried out through: reading it yields nothing, writing to it
	// keeps nothing. `diff /dev/null f` and `git diff --no-index /dev/null
	// f` are the standard way to show a file whole, and `cmd > /dev/null`
	// appears constantly.
	if lit == "/dev/null" {
		return "", false
	}
	_, _, outside, err := resolveWithinWorkdir(boundary, lit)
	if err != nil || !outside {
		return "", false
	}
	return lit, true
}

// literalWordValue returns w's value when every part of it is literal text
// (plain, or inside single/double quotes with no nested expansion), so the
// caller can trust it as the exact string the shell would see — no
// variable, command, or arithmetic substitution resolved or guessed at.
func literalWordValue(w *syntax.Word) (string, bool) {
	var sb strings.Builder
	for _, part := range w.Parts {
		s, ok := literalWordPart(part)
		if !ok {
			return "", false
		}
		sb.WriteString(s)
	}
	return sb.String(), true
}

// literalWordPart returns part's literal value, recursing into quotes made
// entirely of literals. Anything else (ParamExp, CmdSubst, ArithmExp,
// ExtGlob, ...) is not statically resolvable and reports false.
func literalWordPart(part syntax.WordPart) (string, bool) {
	switch p := part.(type) {
	case *syntax.Lit:
		return p.Value, true
	case *syntax.SglQuoted:
		return p.Value, true
	case *syntax.DblQuoted:
		var sb strings.Builder
		for _, sub := range p.Parts {
			s, ok := literalWordPart(sub)
			if !ok {
				return "", false
			}
			sb.WriteString(s)
		}
		return sb.String(), true
	default:
		return "", false
	}
}
