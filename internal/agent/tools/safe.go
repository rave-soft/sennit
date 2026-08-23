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
// harmless *including every argument it accepts*.
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
