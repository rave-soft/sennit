package tools

import (
	"runtime"
	"slices"
	"strings"
)

var safeCommands = []string{
	// Bash builtins and core utils
	"cal",
	"date",
	"df",
	"du",
	"echo",
	"env",
	"free",
	"groups",
	"hostname",
	"id",
	"kill",
	"killall",
	"ls",
	"nice",
	"nohup",
	"printenv",
	"ps",
	"pwd",
	"set",
	"time",
	"timeout",
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
