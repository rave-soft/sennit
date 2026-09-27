package chat

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/message"
	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// serverHomeConfig is a CustomAgentConfig that reports a fixed ServerHome,
// standing in for workspace.FrontendConfig against a remote daemon.
type serverHomeConfig struct{ home string }

func (c serverHomeConfig) AgentOverride(string) (string, string, bool) { return "", "", false }
func (c serverHomeConfig) MCPServerNames() []string                    { return nil }
func (c serverHomeConfig) ServerHomeDir() string                       { return c.home }

// TestToolRenderer_ShortensAgainstServerHome pins CLIENT-SERVER.md's "PR
// 3.2": a tool call's path is a server-side absolute path (the machine the
// tool actually ran on), and must be shortened against
// workspace.FrontendConfig.ServerHome, never against this process's own
// $HOME. HOME is set to an unrelated directory so any accidental fallback
// to fsext.PrettyPath/home.Short (which shorten against os.UserHomeDir())
// would show up as a wrong "~" or a wrong absolute path.
func TestToolRenderer_ShortensAgainstServerHome(t *testing.T) {
	t.Setenv("HOME", "/home/unrelated-client-user")

	sty := styles.SennitDark()
	tc := message.ToolCall{
		ID:       "tc-read",
		Name:     tools.ReadToolName,
		Input:    `{"file_path":"/srv/home/u/proj/a.go"}`,
		Finished: true,
	}

	t.Run("server home set shortens to tilde", func(t *testing.T) {
		item := NewToolMessageItem(&sty, "m1", tc, nil, false, serverHomeConfig{home: "/srv/home/u"})
		out := ansi.Strip(item.Render(120))
		require.Contains(t, out, "~/proj/a.go")
		require.NotContains(t, out, "/srv/home/u/proj/a.go")
	})

	t.Run("server home empty leaves the absolute path", func(t *testing.T) {
		item := NewToolMessageItem(&sty, "m1", tc, nil, false, serverHomeConfig{home: ""})
		out := ansi.Strip(item.Render(120))
		require.Contains(t, out, "/srv/home/u/proj/a.go")
	})

	t.Run("server home different from the path prefix leaves it absolute", func(t *testing.T) {
		item := NewToolMessageItem(&sty, "m1", tc, nil, false, serverHomeConfig{home: "/srv/home/someone-else"})
		out := ansi.Strip(item.Render(120))
		require.Contains(t, out, "/srv/home/u/proj/a.go")
	})

	t.Run("nil config leaves the absolute path, never the client's own HOME", func(t *testing.T) {
		item := NewToolMessageItem(&sty, "m1", tc, nil, false, nil)
		out := ansi.Strip(item.Render(120))
		require.Contains(t, out, "/srv/home/u/proj/a.go")
	})
}
