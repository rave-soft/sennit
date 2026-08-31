package tools

import (
	"runtime"
	"slices"
	"strings"
)

// safeCommands lists commands whose invocation is read-only enough to run
// without a permission prompt. Membership is decided by prefix match (see
// bash.go), which is why nothing here may be a *wrapper*: `env`, `nice`,
// `nohup`, `time` and `timeout` all take another command as their
// arguments, so listing them made `timeout 5 rm -rf ~` match as read-only
// and skip the prompt entirely — an outright permission bypass, since
// bannedCommands does not carry `rm`. `kill`/`killall` are not read-only
// in the first place. Anything added here must be a command that is
// harmless *including every argument it accepts* — or, where that holds
// only for some argument forms, gated in [argumentGatedSafeCommands].
var safeCommands = []string{
	// Bash builtins and core utils
	"cal",
	"date",
	"df",
	"du",
	"echo",
	"free",
	"groups",
	"hostname",
	"id",
	"ls",
	"printenv",
	"ps",
	"pwd",
	"set",
	"top",
	"type",
	"uname",
	"unset",
	"uptime",
	"whatis",
	"whereis",
	"which",
	"whoami",

	// Git
	"git blame",
	"git branch",
	"git config --get",
	"git config --list",
	"git describe",
	"git diff",
	"git grep",
	"git log",
	"git ls-files",
	"git ls-remote",
	"git remote",
	"git rev-parse",
	"git shortlog",
	"git show",
	"git status",
	"git tag",
}

// argumentGatedSafeCommands lists the entries of [safeCommands] whose
// prefix names a read-only command in its bare form but a mutating one as
// soon as arguments arrive. `git branch` lists branches; `git branch -D x`
// deletes one. `git tag` lists tags; `git tag v9` creates one and `git tag
// -d v9` deletes one. `git remote` lists remotes; `git remote remove
// origin` drops one. All three matched by prefix and ran with no prompt at
// all — the same shape of bypass the wrapper entries had, and just as
// invisible, since bannedCommands carries no git.
//
// The value is the complete set of tokens that may follow the prefix.
// Every remaining token must be in it, not just the first: git accepts
// `git branch -v -D x`, so checking only the token after the prefix would
// let a read-only flag escort a destructive one past the gate. Only
// valueless flags are listed, which is why `git branch --contains main`
// asks — the alternative is teaching this table which flags consume the
// next token, and a prompt costs less than that table being subtly wrong.
var argumentGatedSafeCommands = map[string][]string{
	"git branch": {"-a", "--all", "-r", "--remotes", "-v", "-vv", "--verbose", "-l", "--list", "--show-current"},
	"git tag":    {"-l", "--list", "-n"},
	"git remote": {"-v", "--verbose"},
}

// flagGatedSafeCommands lists the entries of [safeCommands] that take
// arbitrary revs, paths, or patterns as bare arguments — so, unlike
// [argumentGatedSafeCommands], their whole token can't be checked against a
// fixed list — but that still carry flags able to name a program to run or
// a file to write. `git ls-remote --upload-pack='touch /pwn'` runs the
// quoted string as the upload-pack program for a local remote; `git grep
// -Ovim` (or `-Orm`, or `--open-files-in-pager=cmd`) opens every matched
// file in the named program; `git diff/log/show --output=path` truncates
// and writes path. All three matched a safeCommands prefix and ran with no
// prompt at all, because nothing here checked flags, only whole tokens.
//
// The value is the set of flags — the part before "=", so
// `--output=path` is checked as `--output` — that are known to be
// genuinely read-only for this command. Every token that starts with "-"
// must be in it (a short cluster like "-in" is allowed only if every
// letter in it is, so a dangerous flag can't hide behind a safe one in the
// same cluster); a bare "--" ends flag checking, since everything after it
// is a path/rev/pattern; non-flag tokens are never checked here — that's
// what makes this gate work for commands with open-ended arguments. An
// unknown flag fails the gate, on purpose: a prompt costs less than this
// table being subtly wrong.
//
// isReadOnlyInvocation checks flags against the original-case remainder of
// the command, not the lower-cased copy used for the prefix match: git's
// short flags are case-sensitive (`-e` is grep's pattern flag, `-E` is
// extended-regexp), so deciding a flag's identity after lower-casing it
// would blur exactly the distinctions this table exists to make — the same
// hazard `-O`/`-o` demonstrated, generalized to every flag here.
var flagGatedSafeCommands = map[string][]string{
	// --upload-pack/-u names the program ls-remote runs against a local
	// or ssh remote; never allow-list it. Everything else here only
	// selects which refs are listed.
	"git ls-remote": {
		"--heads", "--tags", "--refs", "--get-url", "--symref",
		"--sort", "-q", "--quiet", "--exit-code",
	},
	// -O/--open-files-in-pager names a program to run on every matched
	// file; never allow-list it.
	"git grep": {
		"-n", "--line-number", "-i", "--ignore-case", "-l", "--files-with-matches",
		"-L", "--files-without-match", "-w", "--word-regexp", "-v", "--invert-match",
		"-c", "--count", "-e", "-h", "-a", "--text", "-E", "--extended-regexp",
		"-G", "--basic-regexp", "-P", "--perl-regexp", "-F", "--fixed-strings",
		"-r", "--recursive", "--no-recursive", "-I",
	},
	// --output/--output-indicator-* redirect the diff/log/show output to
	// a named file; never allow-list them.
	"git diff": {
		"--stat", "--name-only", "--name-status", "--cached", "--staged",
		"-p", "-u", "--patch", "--unified", "--color", "--no-color",
		"--word-diff", "--summary", "--numstat", "--shortstat",
		"--compact-summary", "-M", "-C", "--find-renames", "--find-copies",
		"--minimal", "--no-index", "-R", "-b", "-w", "--ignore-space-change",
	},
	"git log": {
		"--oneline", "-n", "--max-count", "--graph", "--stat", "--name-only",
		"--name-status", "--format", "--pretty", "--all", "--since", "--until",
		"--author", "--grep", "-p", "--patch", "--reverse", "--merges",
		"--no-merges", "--first-parent", "--decorate", "--abbrev-commit",
		"--date", "-i",
	},
	"git show": {
		"--stat", "--name-only", "--name-status", "-p", "--patch",
		"--format", "--pretty", "--summary", "--oneline",
	},
}

// digitCountFlagCommands are the [flagGatedSafeCommands] entries where a
// short flag made entirely of digits (`-5`, `-13`) is a count or a
// merge-stage selector — `git log -5`, `git show -1` — rather than
// anything that could name a file or a program. It is allowed on any
// cluster position (so `-n5`, meaning `-n 5`, works too), but only digits:
// `-U5` (diff's context-line count) still fails, since `U` is not
// allow-listed and this does not relax that.
var digitCountFlagCommands = map[string]bool{
	"git log":  true,
	"git show": true,
	"git diff": true,
}

// isReadOnlyInvocation reports whether entry — matched as a prefix of
// cmdLower on a command boundary — is read-only for this invocation of the
// original, not-lower-cased command. Entries with no gate are read-only
// however they are called. Both gates apply where both are configured for
// entry. entry is itself lower-case ASCII, so len(entry) locates the same
// byte offset in command as in cmdLower.
func isReadOnlyInvocation(entry, cmdLower, command string) bool {
	if allowed, gated := argumentGatedSafeCommands[entry]; gated {
		for _, token := range strings.Fields(cmdLower[len(entry):]) {
			if !slices.Contains(allowed, token) {
				return false
			}
		}
	}
	if allowedFlags, gated := flagGatedSafeCommands[entry]; gated {
		if !flagsAllowed(allowedFlags, command[len(entry):], digitCountFlagCommands[entry]) {
			return false
		}
	}
	return true
}

// flagsAllowed reports whether every flag token in rest (a command's
// original-case arguments, already split on whitespace) is in allowed —
// see [flagGatedSafeCommands]. Non-flag tokens (revs, paths, patterns) pass
// freely, which is the whole point of this gate over
// [argumentGatedSafeCommands]. digitsOK additionally admits digit runes
// anywhere in a flag or cluster — see [digitCountFlagCommands].
func flagsAllowed(allowed []string, rest string, digitsOK bool) bool {
	// strings.Fields does not understand shell quoting. A quoted flag such
	// as `"--output=../pwned"` would otherwise look like a non-flag token
	// and bypass this gate. There is no need to parse quoted arguments here:
	// the safe path is an optimization, so reject quoting and ask instead.
	if strings.ContainsAny(rest, "'\"") {
		return false
	}
	endOfFlags := false
	for _, token := range strings.Fields(rest) {
		if endOfFlags {
			continue
		}
		if token == "--" {
			endOfFlags = true
			continue
		}
		if !strings.HasPrefix(token, "-") {
			continue
		}
		flag, _, _ := strings.Cut(token, "=")
		if slices.Contains(allowed, flag) {
			continue
		}
		// A long flag that isn't allow-listed outright has no cluster
		// to fall back on: fail closed. So does a bare "-".
		if strings.HasPrefix(flag, "--") || len(flag) < 2 {
			return false
		}
		// A short-flag cluster ("-abc") is only as safe as its least
		// safe letter: every one of them must itself be an
		// allow-listed short flag (or, where digitsOK, a digit), or
		// the cluster fails. This also covers a lone short flag
		// ("-q"): the loop runs once.
		for _, r := range flag[1:] {
			if digitsOK && r >= '0' && r <= '9' {
				continue
			}
			if !slices.Contains(allowed, "-"+string(r)) {
				return false
			}
		}
	}
	return true
}

// isSafeReadOnlyCommand reports whether command is read-only enough to run
// without a permission prompt: no chaining metacharacter, and a prefix
// match on [safeCommands] whose gates (if any) all pass for this particular
// invocation.
func isSafeReadOnlyCommand(command string) bool {
	if containsCommandChaining(command) {
		return false
	}
	cmdLower := strings.ToLower(command)
	for _, safe := range safeCommands {
		if !strings.HasPrefix(cmdLower, safe) {
			continue
		}
		if len(cmdLower) != len(safe) && cmdLower[len(safe)] != ' ' && cmdLower[len(safe)] != '-' {
			continue
		}
		// A failed gate does not break: it means this entry does not
		// make the command read-only, not that no later entry can.
		if isReadOnlyInvocation(safe, cmdLower, command) {
			return true
		}
	}
	return false
}

var chainingMetacharacters = []string{
	";",
	"|",
	"&", // also covers "&&" and "&>"
	"$(",
	"`",
	// Redirections: a read-only command with ">" writes an arbitrary file,
	// and "<(" runs an arbitrary command. "<" alone is harmless, but it is
	// cheaper to prompt than to distinguish it from "<(".
	">",
	"<",
	"\n",
}

// containsCommandChaining reports whether s contains shell metacharacters
// that enable command chaining, substitution, or redirection. A command
// containing any of these is never treated as safe/read-only, so it always
// goes through the permission request.
func containsCommandChaining(s string) bool {
	return slices.ContainsFunc(chainingMetacharacters, func(c string) bool {
		return strings.Contains(s, c)
	})
}

func init() {
	if runtime.GOOS == "windows" {
		safeCommands = append(
			safeCommands,
			// Windows-specific commands
			"ipconfig",
			"nslookup",
			"ping",
			"systeminfo",
			"tasklist",
			"where",
		)
	}
}
