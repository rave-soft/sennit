package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewCoordinatorRequiresBackgroundShellManager(t *testing.T) {
	coordinator, err := NewCoordinator(context.Background(), CoordinatorOptions{})
	require.Nil(t, coordinator)
	require.ErrorIs(t, err, errBackgroundShellsRequired)
}
