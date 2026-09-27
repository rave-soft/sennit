package dialog

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// serverHomeWorkspace answers Config() with a FrontendConfig carrying a
// fixed ServerHome, built directly rather than through NewFrontendConfig so
// the test controls it independently of this process's own home.
type serverHomeWorkspace struct {
	workspace.FrontendWorkspace
	serverHome string
}

func (w serverHomeWorkspace) Config() *workspace.FrontendConfig {
	return &workspace.FrontendConfig{ServerHome: w.serverHome}
}

// TestPermissions_PathShortensAgainstServerHome pins CLIENT-SERVER.md's "PR
// 3.2": the permission dialog's file/path lines are server-side absolute
// paths (the machine the tool actually ran, or would run, on) and must be
// shortened against workspace.FrontendConfig.ServerHome, never against this
// process's own $HOME (fsext.PrettyPath/home.Short).
func TestPermissions_PathShortensAgainstServerHome(t *testing.T) {
	t.Setenv("HOME", "/home/unrelated-client-user")

	sty := styles.SennitDark()
	newPermissions := func(serverHome string) *Permissions {
		com := &common.Common{
			Styles:    &sty,
			Workspace: serverHomeWorkspace{serverHome: serverHome},
		}
		perm := permission.PermissionRequest{
			ID:         "perm-path",
			ToolCallID: "tool-call-path",
			ToolName:   "bash",
			Path:       "/srv/home/u/proj/a.go",
		}
		return NewPermissions(com, perm)
	}

	t.Run("server home set shortens to tilde", func(t *testing.T) {
		out := ansi.Strip(newPermissions("/srv/home/u").renderHeader(200))
		require.Contains(t, out, "~/proj/a.go")
		require.NotContains(t, out, "/srv/home/u/proj/a.go")
	})

	t.Run("server home empty leaves the absolute path", func(t *testing.T) {
		out := ansi.Strip(newPermissions("").renderHeader(200))
		require.Contains(t, out, "/srv/home/u/proj/a.go")
	})

	t.Run("server home different from the path prefix leaves it absolute", func(t *testing.T) {
		out := ansi.Strip(newPermissions("/srv/home/someone-else").renderHeader(200))
		require.Contains(t, out, "/srv/home/u/proj/a.go")
	})

	t.Run("no workspace wired up leaves the absolute path, never the client's own HOME", func(t *testing.T) {
		p := newTestPermissions(t)
		p.permission.Path = "/srv/home/u/proj/a.go"
		out := ansi.Strip(p.renderHeader(200))
		require.Contains(t, out, "/srv/home/u/proj/a.go")
	})
}
