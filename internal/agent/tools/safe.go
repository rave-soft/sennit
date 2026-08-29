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

// isReadOnlyInvocation reports whether cmdLower — which has already matched
// entry as a prefix on a command boundary — is read-only in this particular
// invocation. Entries with no gate are read-only however they are called.
func isReadOnlyInvocation(entry, cmdLower string) bool {
	allowed, gated := argumentGatedSafeCommands[entry]
	if !gated {
		return true
	}
	for _, token := range strings.Fields(cmdLower[len(entry):]) {
		if !slices.Contains(allowed, token) {
			return false
		}
	}
	return true
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
