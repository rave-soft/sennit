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
