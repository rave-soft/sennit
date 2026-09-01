package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOAuthPlatformsHaveCompleteRoutingAndAliases(t *testing.T) {
	completions := oauthPlatformCompletions()
	for _, platform := range oauthPlatforms {
		require.NotEmpty(t, platform.ID)
		require.NotEmpty(t, platform.DisplayName)
		require.NotNil(t, platform.Login)
		require.NotNil(t, platform.Logout)

		resolved, ok := resolveOAuthPlatform(platform.ID)
		require.True(t, ok)
		require.Equal(t, platform.ID, resolved.ID)
		require.Contains(t, completions, platform.ID)
		for _, alias := range platform.Aliases {
			resolved, ok := resolveOAuthPlatform(alias)
			require.True(t, ok)
			require.Equal(t, platform.ID, resolved.ID)
			require.Contains(t, completions, alias)
		}
	}
}
