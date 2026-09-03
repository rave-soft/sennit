package env

import "strings"

// herdrEnvVars are the environment variables herdr injects into panes
// so agents can report state over its Unix socket API. Subprocesses
// must not inherit these: a child process that calls herdr.Init()
// would attach to the parent's pane and, on exit, release its agent
// authority — making the status vanish. Stripping them here closes
// that gap for every subprocess Sennit starts from an inherited
// environment.
var herdrEnvVars = []string{
	"HERDR_ENV",
	"HERDR_SOCKET_PATH",
	"HERDR_PANE_ID",
}

// WithoutHerdrEnv returns env with all HERDR_* variables removed. The
// returned slice is a new allocation safe to use concurrently with the
// input. Lives here rather than in internal/shell because it is pure
// []string filtering with nothing shell-specific about it, and every
// caller that needs it — the shell package, hooks, git, MCP stdio
// servers, config's $(cmd) resolution — can depend on this leaf package
// without pulling in the shell interpreter.
func WithoutHerdrEnv(env []string) []string {
	strip := make(map[string]bool, len(herdrEnvVars))
	for _, k := range herdrEnvVars {
		strip[k] = true
	}
	result := make([]string, 0, len(env))
	for _, e := range env {
		if key, _, ok := strings.Cut(e, "="); ok && strip[key] {
			continue
		}
		result = append(result, e)
	}
	return result
}
