package cmd

import (
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

type prefsBackedWorkspace struct {
	workspace.Workspace
	store *config.ConfigStore
}

func (w prefsBackedWorkspace) ConfigStore() *config.ConfigStore { return w.store }

type prefslessWorkspace struct{ workspace.Workspace }

func TestUIPrefsStore(t *testing.T) {
	t.Parallel()

	t.Run("a workspace with a config store gets an adapter over it", func(t *testing.T) {
		t.Parallel()
		prefs, err := uiPrefsStore(prefsBackedWorkspace{})
		require.NoError(t, err)
		require.NotNil(t, prefs)
	})

	t.Run("any other workspace is an error, not a store that drops writes", func(t *testing.T) {
		t.Parallel()
		prefs, err := uiPrefsStore(prefslessWorkspace{})
		require.Error(t, err)
		require.Nil(t, prefs)
	})
}
