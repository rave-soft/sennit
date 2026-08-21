package agent

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
	"gopkg.in/dnaeon/go-vcr.v4/pkg/recorder"
)

// TestTestEnvWorkingDirIgnoresTMPDIR is the direct, fast check on the fix
// itself: testEnv's working directory must stay rooted at
// canonicalTestTempRoot no matter what $TMPDIR says. os.TempDir() honors
// $TMPDIR, which is "/tmp" on Linux CI runners but a per-run path like
// "/var/folders/xx/yy/T" on macOS runners — and that root ends up baked
// verbatim into VCR cassette content (see TestCoderAgentWorkingDirIsOSIndependent
// for the end-to-end replay). Swap canonicalTestTempRoot back for
// os.TempDir() in testEnv and this test fails immediately, since the forced
// $TMPDIR below no longer matches.
func TestTestEnvWorkingDirIgnoresTMPDIR(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "var", "folders", "xx", "yy", "T"))
	require.NotEqual(t, canonicalTestTempRoot, os.TempDir(), "test setup: TMPDIR override did not take effect")

	env := testEnv(t)
	// Exact match, not just a prefix check: the forced $TMPDIR above happens
	// to nest under the real /tmp too (t.TempDir() defaults there on this
	// box), so a bare HasPrefix(workingDir, "/tmp/") would pass even with
	// the fix reverted to os.TempDir(). Pin the whole path instead.
	require.Equal(t, filepath.Join(canonicalTestTempRoot, "sennit-test-", t.Name()), env.workingDir)
}

// TestCoderAgentWorkingDirIsOSIndependent is the closest thing to a real
// macOS run available on this box: it replays the real committed "ls_tool"
// cassette against a working directory built the same way testEnv now
// builds one (rooted at canonicalTestTempRoot, not os.TempDir()) while
// $TMPDIR is forced to a macOS-shaped value. The cassette bakes that working
// directory verbatim into a tool-role message (the ls tool echoes back the
// absolute directory it listed) — unlike the system prompt, tool-role
// content is matched byte-strict, not normalized (see jsonBodyEqual and
// normalizeForMatch) — so this only replays clean because the directory is
// canonical. Point workingDir at an os.TempDir()-based path instead (what
// testEnv built before this fix) and the replay fails with the same
// "requested interaction not found" retry error CI reported.
func TestCoderAgentWorkingDirIsOSIndependent(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "var", "folders", "xx", "yy", "T"))

	// Reuse the real committed "ls tool" cassette (and the working directory
	// it was recorded under) directly, rather than deriving them from
	// t.Name() via cassetteName/setupAgent/testEnv — this test's own name
	// isn't "TestCoderAgent/glm-5.1/ls_tool", so that derivation would look
	// in the wrong places.
	cfg, err := resolveTestVCRConfig("", "", "", "")
	require.NoError(t, err)
	wantWorkingDir := filepath.Join(canonicalTestTempRoot, "sennit-test-", "TestCoderAgent", defaultFixtureModel, "ls_tool")
	agent, env := setupAgentWithVCR(t, cfg, filepath.Join("TestCoderAgent", defaultFixtureModel, "ls_tool"), wantWorkingDir, nil)
	require.Equal(t, wantWorkingDir, env.workingDir)

	session, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)
	res, err := agent.Run(t.Context(), SessionAgentCall{
		Prompt:          "use ls to list the files in the current directory",
		SessionID:       session.ID,
		MaxOutputTokens: 10000,
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	msgs, err := env.messages.List(t.Context(), session.ID)
	require.NoError(t, err)
	var lsTCID string
	foundLS := false
	for _, msg := range msgs {
		if msg.Role == message.Assistant {
			for _, tc := range msg.ToolCalls() {
				if tc.Name == tools.LSToolName {
					lsTCID = tc.ID
				}
			}
		}
		if msg.Role == message.Tool {
			for _, tr := range msg.ToolResults() {
				if tr.ToolCallID == lsTCID {
					foundLS = true
					require.Contains(t, tr.Content, "main.go")
				}
			}
		}
	}
	require.True(t, foundLS, "expected an ls tool result")
}

// TestCoderAgentFixtureCassettesAreByteIdentical verifies a representative
// agent flow produces exactly the same cassettes in isolated recording roots.
// It records a normal reply followed by a fetch tool multi-turn, exercising the
// actual coder prompt, tool schema, agent setup, VCR hooks, and fixture server.
func TestCoderAgentFixtureCassettesAreByteIdentical(t *testing.T) {
	if testing.Short() {
		t.Skip("records representative agent fixture cassettes")
	}

	first := t.TempDir()
	second := t.TempDir()
	runRepresentativeCoderAgentFixtureFlow(t, first, "first")
	runRepresentativeCoderAgentFixtureFlow(t, second, "second")

	firstFiles := cassetteFiles(t, first)
	secondFiles := cassetteFiles(t, second)
	require.Equal(t, firstFiles, secondFiles, "cassette file sets differ")
	for _, name := range firstFiles {
		firstBytes, err := os.ReadFile(filepath.Join(first, name))
		require.NoError(t, err)
		secondBytes, err := os.ReadFile(filepath.Join(second, name))
		require.NoError(t, err)
		require.Equalf(t, firstBytes, secondBytes, "cassette %q differs; a volatile field was recorded", name)
	}
}

func runRepresentativeCoderAgentFixtureFlow(t *testing.T, cassetteRoot, cassetteBasename string) {
	t.Helper()
	cfg := testVCRConfig{
		Mode:         recorder.ModeRecordOnly,
		CassetteRoot: cassetteRoot,
		Model:        defaultFixtureModel,
	}
	agent, env := setupAgentWithVCR(t, cfg, cassetteBasename, "", func() string {
		return "deterministic_flow"
	})

	session, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)
	for _, prompt := range []string{
		"Hello",
		"fetch the content from " + fixtureResourceURL(env, "/fetch") + " and tell me if it contains the word 'John Doe'",
	} {
		result, err := agent.Run(t.Context(), SessionAgentCall{
			Prompt:          prompt,
			SessionID:       session.ID,
			MaxOutputTokens: 10000,
		})
		require.NoError(t, err)
		require.NotNil(t, result)
	}

	messages, err := env.messages.List(t.Context(), session.ID)
	require.NoError(t, err)
	fetchResult := false
	for _, msg := range messages {
		if msg.Role != message.Tool {
			continue
		}
		for _, result := range msg.ToolResults() {
			if result.Name == tools.FetchToolName {
				fetchResult = true
				require.False(t, result.IsError, "fetch result: %s", result.Content)
				require.Contains(t, result.Content, "John Doe fixture content")
			}
		}
	}
	require.True(t, fetchResult, "expected fetch tool result")
}

func cassetteFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(t, err)
	sort.Strings(files)
	return files
}
