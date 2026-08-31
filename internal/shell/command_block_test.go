package shell

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommandBlocking(t *testing.T) {
	tests := []struct {
		name        string
		blockFuncs  []BlockFunc
		command     string
		shouldBlock bool
	}{
		{
			name: "block simple command",
			blockFuncs: []BlockFunc{
				func(args []string) bool {
					return len(args) > 0 && args[0] == "curl"
				},
			},
			command:     "curl https://example.com",
			shouldBlock: true,
		},
		{
			name: "allow non-blocked command",
			blockFuncs: []BlockFunc{
				func(args []string) bool {
					return len(args) > 0 && args[0] == "curl"
				},
			},
			command:     "echo hello",
			shouldBlock: false,
		},
		{
			name:        "block curl behind wrapper options with values",
			blockFuncs:  []BlockFunc{CommandsBlocker([]string{"curl"})},
			command:     "env -u FOO curl https://example.com",
			shouldBlock: true,
		},
		{
			name:        "block curl behind nice adjustment",
			blockFuncs:  []BlockFunc{CommandsBlocker([]string{"curl"})},
			command:     "nice -n 10 curl https://example.com",
			shouldBlock: true,
		},
		{
			name:        "block curl behind xargs count",
			blockFuncs:  []BlockFunc{CommandsBlocker([]string{"curl"})},
			command:     "xargs -n 1 curl https://example.com",
			shouldBlock: true,
		},
		{
			name: "block subcommand",
			blockFuncs: []BlockFunc{
				func(args []string) bool {
					return len(args) >= 2 && args[0] == "brew" && args[1] == "install"
				},
			},
			command:     "brew install wget",
			shouldBlock: true,
		},
		{
			name: "allow different subcommand",
			blockFuncs: []BlockFunc{
				func(args []string) bool {
					return len(args) >= 2 && args[0] == "brew" && args[1] == "install"
				},
			},
			command:     "brew list",
			shouldBlock: false,
		},
		{
			name: "block npm global install with -g",
			blockFuncs: []BlockFunc{
				ArgumentsBlocker("npm", []string{"install"}, []string{"-g"}),
			},
			command:     "npm install -g typescript",
			shouldBlock: true,
		},
		{
			name: "block npm global install with --global",
			blockFuncs: []BlockFunc{
				ArgumentsBlocker("npm", []string{"install"}, []string{"--global"}),
			},
			command:     "npm install --global typescript",
			shouldBlock: true,
		},
		{
			name: "allow npm local install",
			blockFuncs: []BlockFunc{
				ArgumentsBlocker("npm", []string{"install"}, []string{"-g"}),
				ArgumentsBlocker("npm", []string{"install"}, []string{"--global"}),
			},
			command:     "npm install typescript",
			shouldBlock: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a temporary directory for each test
			tmpDir := t.TempDir()

			shell := NewShell(&Options{
				WorkingDir: tmpDir,
				BlockFuncs: tt.blockFuncs,
			})

			_, _, err := shell.Exec(t.Context(), tt.command)

			if tt.shouldBlock {
				if err == nil {
					t.Errorf("Expected command to be blocked, but it was allowed")
				} else if !strings.Contains(err.Error(), "not allowed for security reasons") {
					t.Errorf("Expected security error, got: %v", err)
				}
			} else {
				// For non-blocked commands, we might get other errors (like command not found)
				// but we shouldn't get the security error
				if err != nil && strings.Contains(err.Error(), "not allowed for security reasons") {
					t.Errorf("Command was unexpectedly blocked: %v", err)
				}
			}
		})
	}
}

func TestArgumentsBlocker(t *testing.T) {
	tests := []struct {
		name        string
		cmd         string
		args        []string
		flags       []string
		input       []string
		shouldBlock bool
	}{
		// Basic command blocking
		{
			name:        "block exact command match",
			cmd:         "npm",
			args:        []string{"install"},
			flags:       nil,
			input:       []string{"npm", "install", "package"},
			shouldBlock: true,
		},
		{
			name:        "allow different command",
			cmd:         "npm",
			args:        []string{"install"},
			flags:       nil,
			input:       []string{"yarn", "install", "package"},
			shouldBlock: false,
		},
		{
			name:        "allow different subcommand",
			cmd:         "npm",
			args:        []string{"install"},
			flags:       nil,
			input:       []string{"npm", "list"},
			shouldBlock: false,
		},

		// Flag-based blocking
		{
			name:        "block with single flag",
			cmd:         "npm",
			args:        []string{"install"},
			flags:       []string{"-g"},
			input:       []string{"npm", "install", "-g", "typescript"},
			shouldBlock: true,
		},
		{
			name:        "block with flag in different position",
			cmd:         "npm",
			args:        []string{"install"},
			flags:       []string{"-g"},
			input:       []string{"npm", "install", "typescript", "-g"},
			shouldBlock: true,
		},
		{
			name:        "allow without required flag",
			cmd:         "npm",
			args:        []string{"install"},
			flags:       []string{"-g"},
			input:       []string{"npm", "install", "typescript"},
			shouldBlock: false,
		},
		{
			name:        "block with multiple flags",
			cmd:         "pip",
			args:        []string{"install"},
			flags:       []string{"--user"},
			input:       []string{"pip", "install", "--user", "--upgrade", "package"},
			shouldBlock: true,
		},

		// Complex argument patterns
		{
			name:        "block multi-arg subcommand",
			cmd:         "yarn",
			args:        []string{"global", "add"},
			flags:       nil,
			input:       []string{"yarn", "global", "add", "typescript"},
			shouldBlock: true,
		},
		{
			name:        "allow partial multi-arg match",
			cmd:         "yarn",
			args:        []string{"global", "add"},
			flags:       nil,
			input:       []string{"yarn", "global", "list"},
			shouldBlock: false,
		},

		// Edge cases
		{
			name:        "handle empty input",
			cmd:         "npm",
			args:        []string{"install"},
			flags:       nil,
			input:       []string{},
			shouldBlock: false,
		},
		{
			name:        "handle command only",
			cmd:         "npm",
			args:        []string{"install"},
			flags:       nil,
			input:       []string{"npm"},
			shouldBlock: false,
		},
		{
			name:        "block pacman with -S flag",
			cmd:         "pacman",
			args:        nil,
			flags:       []string{"-S"},
			input:       []string{"pacman", "-S", "package"},
			shouldBlock: true,
		},
		{
			name:        "allow pacman without -S flag",
			cmd:         "pacman",
			args:        nil,
			flags:       []string{"-S"},
			input:       []string{"pacman", "-Q", "package"},
			shouldBlock: false,
		},

		// `go test -exec`
		{
			name:        "go test exec",
			cmd:         "go",
			args:        []string{"test"},
			flags:       []string{"-exec"},
			input:       []string{"go", "test", "-exec", "bash -c 'echo hello'"},
			shouldBlock: true,
		},
		{
			name:        "go test exec",
			cmd:         "go",
			args:        []string{"test"},
			flags:       []string{"-exec"},
			input:       []string{"go", "test", `-exec="bash -c 'echo hello'"`},
			shouldBlock: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocker := ArgumentsBlocker(tt.cmd, tt.args, tt.flags)
			result := blocker(tt.input)
			require.Equal(t, tt.shouldBlock, result,
				"Expected block=%v for input %v", tt.shouldBlock, tt.input)
		})
	}
}

func TestCommandsBlocker(t *testing.T) {
	tests := []struct {
		name        string
		banned      []string
		input       []string
		shouldBlock bool
	}{
		{
			name:        "block single banned command",
			banned:      []string{"curl"},
			input:       []string{"curl", "https://example.com"},
			shouldBlock: true,
		},
		{
			name:        "allow non-banned command",
			banned:      []string{"curl", "wget"},
			input:       []string{"echo", "hello"},
			shouldBlock: false,
		},
		{
			name:        "block from multiple banned",
			banned:      []string{"curl", "wget", "nc"},
			input:       []string{"wget", "https://example.com"},
			shouldBlock: true,
		},
		{
			name:        "handle empty input",
			banned:      []string{"curl"},
			input:       []string{},
			shouldBlock: false,
		},
		{
			name:        "case sensitive matching",
			banned:      []string{"curl"},
			input:       []string{"CURL", "https://example.com"},
			shouldBlock: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocker := CommandsBlocker(tt.banned)
			result := blocker(tt.input)
			require.Equal(t, tt.shouldBlock, result,
				"Expected block=%v for input %v", tt.shouldBlock, tt.input)
		})
	}
}

// TestCommandsBlocker_UnwrapsPathsAndWrapperPrefixes is the regression
// test for CommandsBlocker comparing bannedCommands against args[0]
// verbatim: /usr/bin/curl, env FOO=1 curl, timeout 5 curl and xargs curl
// all missed the deny list because none of them literally was "curl". A
// name that merely contains a banned command as a substring — curlywurly
// — must still be let through: this is a name match, not a substring scan.
func TestCommandsBlocker_UnwrapsPathsAndWrapperPrefixes(t *testing.T) {
	tests := []struct {
		name        string
		input       []string
		shouldBlock bool
	}{
		{"absolute path to banned command", []string{"/usr/bin/curl", "https://example.com"}, true},
		{"relative path to banned command", []string{"./curl", "https://example.com"}, true},
		{"env with var assignment", []string{"env", "FOO=1", "curl", "https://example.com"}, true},
		{"env with flag before command", []string{"env", "-i", "curl", "https://example.com"}, true},
		{"timeout with duration", []string{"timeout", "5", "curl", "https://example.com"}, true},
		{"timeout with flag and duration", []string{"timeout", "--signal=KILL", "5", "curl", "x"}, true},
		{"xargs wrapping banned command", []string{"xargs", "curl"}, true},
		{"nice wrapping sudo", []string{"nice", "sudo", "ls"}, true},
		{"substring of a banned name is not blocked", []string{"curlywurly", "x"}, false},
		{"unbanned command through env is not blocked", []string{"env", "echo", "hi"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocker := CommandsBlocker([]string{"curl", "sudo"})
			result := blocker(tt.input)
			require.Equal(t, tt.shouldBlock, result,
				"Expected block=%v for input %v", tt.shouldBlock, tt.input)
		})
	}
}

func TestSplitArgsFlags(t *testing.T) {
	tests := []struct {
		name      string
		input     []string
		wantArgs  []string
		wantFlags []string
	}{
		{
			name:      "only args",
			input:     []string{"install", "package", "another"},
			wantArgs:  []string{"install", "package", "another"},
			wantFlags: []string{},
		},
		{
			name:      "only flags",
			input:     []string{"-g", "--verbose", "-f"},
			wantArgs:  []string{},
			wantFlags: []string{"-g", "--verbose", "-f"},
		},
		{
			name:      "mixed args and flags",
			input:     []string{"install", "-g", "package", "--verbose"},
			wantArgs:  []string{"install", "package"},
			wantFlags: []string{"-g", "--verbose"},
		},
		{
			name:      "empty input",
			input:     []string{},
			wantArgs:  []string{},
			wantFlags: []string{},
		},
		{
			name:      "single dash flag",
			input:     []string{"-S", "package"},
			wantArgs:  []string{"package"},
			wantFlags: []string{"-S"},
		},
		{
			name:      "flag with equals sign",
			input:     []string{"-exec=bash", "package"},
			wantArgs:  []string{"package"},
			wantFlags: []string{"-exec"},
		},
		{
			name:      "long flag with equals sign",
			input:     []string{"--config=/path/to/config", "run"},
			wantArgs:  []string{"run"},
			wantFlags: []string{"--config"},
		},
		{
			name:      "flag with complex value",
			input:     []string{`-exec="bash -c 'echo hello'"`, "test"},
			wantArgs:  []string{"test"},
			wantFlags: []string{"-exec"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, flags := splitArgsFlags(tt.input)
			require.Equal(t, tt.wantArgs, args, "args mismatch")
			require.Equal(t, tt.wantFlags, flags, "flags mismatch")
		})
	}
}
