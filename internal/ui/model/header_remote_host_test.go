package model

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// remoteHostTestWorkspace is countingWorkspace plus a fixed WorkingDir, so
// renderHeaderDetails has something to shorten and render next to whatever
// RemoteHost says.
type remoteHostTestWorkspace struct {
	*countingWorkspace
	workingDir string
}

func (w remoteHostTestWorkspace) WorkingDir() string { return w.workingDir }

func (w remoteHostTestWorkspace) Config() *workspace.FrontendConfig {
	return &workspace.FrontendConfig{}
}

// TestHeader_RemoteHostRendersOnlyWhenSet pins CLIENT-SERVER.md's "PR 3.2":
// the header names the machine a remote connection reaches, right next to
// the working directory, and stays exactly as it always did (no RemoteHost
// text at all) in every mode that isn't a `--remote`/`attach ssh://...`
// session — RemoteHost is empty there (Common.RemoteHost's doc comment).
func TestHeader_RemoteHostRendersOnlyWhenSet(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	ws := remoteHostTestWorkspace{countingWorkspace: &countingWorkspace{}, workingDir: "/srv/home/u/proj"}
	sess := &session.Session{}

	local := &common.Common{Styles: &sty, Workspace: ws}
	out := ansi.Strip(renderHeaderDetails(local, sess, 0, 0, false, 200, "ctrl+d", false))
	require.NotContains(t, out, "@", "no RemoteHost set: nothing names a remote machine")

	remote := &common.Common{Styles: &sty, Workspace: ws, RemoteHost: "alice@build-box"}
	out = ansi.Strip(renderHeaderDetails(remote, sess, 0, 0, false, 200, "ctrl+d", false))
	require.Contains(t, out, "alice@build-box")
}
