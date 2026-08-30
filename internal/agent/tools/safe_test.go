package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestContainsCommandChaining(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"plain ls", "ls -la", false},
		{"plain echo", "echo hello world", false},
		{"plain pwd", "pwd", false},
		{"plain git status", "git status", false},
		{"ls with redirect", "ls > /tmp/out", true},
		{"ls with pipe", "ls | grep foo", true},
		{"ls with double ampersand", "ls && echo done", true},
		{"ls with semicolon", "ls; echo done", true},
		{"ls with pipe pipe", "ls || echo fail", true},
		{"ls with backticks", "ls `echo foo`", true},
		{"ls with subshell", "ls $(echo foo)", true},
		{"ls with background ampersand", "ls & echo done", true},
		{"rm -rf with && ls (rm first)", "rm -rf / && ls", true},
		{"redirect with ampersand gt", "ls &> /dev/null", true},
		{"redirect with gt ampersand", "ls >& /dev/null", true},
		{"simple kill", "kill 1234", false},
		{"kill with pipe", "kill 1234 | echo foo", true},
		{"git log", "git log --oneline", false},
		{"git log with pipe", "git log | head", true},
		{"input redirect", "wc -l < notes.txt", true},
		{"process substitution", "diff <(ls a) b", true},
		{"newline smuggles a second command", "echo hi\nrm -rf dir", true},
		{"append redirect into rc file", "echo payload >> ~/.bashrc", true},
		{"empty string", "", false},
		{"dollar sign in argument", "echo $HOME", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := containsCommandChaining(tt.input)
			assert.Equal(t, tt.expected, got, "containsCommandChaining(%q)", tt.input)
		})
	}
}

// TestIsSafeReadOnlyCommand_FlagGateClosesTheThreeBypasses is the
// regression test for three ways a command matching a safeCommands prefix
// used to run with no permission prompt at all, because nothing checked
// its flags: `git ls-remote --upload-pack=...` runs an arbitrary program
// for a local remote, `git grep -O.../-Orm/--open-files-in-pager=...` runs
// a named program on every matched file, and `git diff/log/show
// --output=...` truncates and writes an arbitrary path.
func TestIsSafeReadOnlyCommand_FlagGateClosesTheThreeBypasses(t *testing.T) {
	t.Parallel()

	tests := []string{
		`git ls-remote --upload-pack='touch /tmp/pwned; git-upload-pack' .`,
		`git grep -O'touch /tmp/pwned2 --' hello`,
		`git grep -Orm hello`,
		`git grep --open-files-in-pager=cmd hello`,
		`git diff --output=../pwned3 HEAD`,
		`git log --output=../pwned4 -1`,
		`git show --output=x HEAD`,
		`git diff -U5 HEAD`,
	}

	for _, command := range tests {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			assert.False(t, isSafeReadOnlyCommand(command), "%q must not be classified read-only", command)
		})
	}
}

// TestIsSafeReadOnlyCommand_OrdinaryUsageStaysPromptFree pins the other
// side of the flag gate: the ordinary, genuinely read-only invocations of
// the gated commands must not start asking for a permission prompt just
// because a gate now exists.
func TestIsSafeReadOnlyCommand_OrdinaryUsageStaysPromptFree(t *testing.T) {
	t.Parallel()

	tests := []string{
		"git status",
		"git diff",
		"git diff --stat HEAD~1 -- internal/",
		"git log --oneline -n 5",
		"git log -5",
		"git log -3",
		"git show --stat HEAD",
		"git show -1",
		"git grep -n foo",
		"git grep -e pattern -- .",
		"git grep -E pattern",
		"git grep -F -i pattern",
		"git diff -M -C HEAD~1",
		"git ls-remote --heads origin",
		"git branch -v",
		"git tag -l",
	}

	for _, command := range tests {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			assert.True(t, isSafeReadOnlyCommand(command), "%q is read-only and must not require a prompt", command)
		})
	}
}

// TestFlagsAllowed_ShortFlagClusters exercises the cluster rule directly:
// every letter in a short-flag cluster must itself be allow-listed, so a
// dangerous flag can't ride through hidden behind a safe one.
func TestFlagsAllowed_ShortFlagClusters(t *testing.T) {
	t.Parallel()

	allowed := []string{"-n", "-i", "-l", "-w", "-e"}

	assert.True(t, flagsAllowed(allowed, "-n -i pattern", false))
	assert.True(t, flagsAllowed(allowed, "-nil pattern", false), "cluster of all-allowed letters")
	assert.False(t, flagsAllowed(allowed, "-orm pattern", false), "cluster with a disallowed letter")
	assert.False(t, flagsAllowed(allowed, "-io pattern", false), "cluster with a disallowed letter")
	assert.False(t, flagsAllowed(allowed, "--unknown-flag", false))
	assert.True(t, flagsAllowed(allowed, "-e pattern -- some/path", false), "-- ends flag checking")
	assert.True(t, flagsAllowed(allowed, "-e -- --not-a-flag-after-double-dash", false), "-- ends flag checking for what follows")
}

// TestFlagsAllowed_CaseIsPreserved is the regression test for checking
// flags against a lower-cased command: that collapsed -E/-e, -M/-m, and
// every other case-distinguished pair onto one token, so half of every
// gated table's uppercase entries could never match a real invocation, and
// -O (grep's dangerous "open in pager") could hide behind -o (harmless
// "only matching"). flagsAllowed must see flags in their original case.
func TestFlagsAllowed_CaseIsPreserved(t *testing.T) {
	t.Parallel()

	allowed := []string{"-n", "-i", "-e", "-E"}

	assert.True(t, flagsAllowed(allowed, "-E pattern", false), "uppercase entry must match its own case")
	assert.False(t, flagsAllowed(allowed, "-e -O pattern", false), "-O must not be treated as -e/-E just because it lower-cases near them")
}

// TestFlagsAllowed_DigitClusters is the regression test for
// [digitCountFlagCommands]: `git log -5`/`git show -1` used to prompt
// because a short flag made only of digits wasn't allow-listed and had no
// cluster to fall back on. digitsOK must admit digits in any cluster
// position ("-n5", meaning -n 5) without loosening non-digit letters:
// "-U5" must still fail on the "U".
func TestFlagsAllowed_DigitClusters(t *testing.T) {
	t.Parallel()

	allowed := []string{"-n", "-u"}

	assert.True(t, flagsAllowed(allowed, "-5", true), "bare digit count")
	assert.True(t, flagsAllowed(allowed, "-13", true), "multi-digit count")
	assert.True(t, flagsAllowed(allowed, "-n5", true), "-n5 means -n 5")
	assert.False(t, flagsAllowed(allowed, "-U5", true), "U is not allow-listed; digitsOK must not rescue it")
	assert.False(t, flagsAllowed(allowed, "-5", false), "digitsOK false must not admit a digit flag")
}
