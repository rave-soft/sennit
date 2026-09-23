package cmd

import (
	"context"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/modelsrefresh"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/spf13/cobra"
)

// codexConfigured reports whether there is a Codex login to refresh models
// for. Refresh is about re-reading a list that already exists; signing in is
// `sennit login codex`.
func codexConfigured(cfg *config.ConfigStore) bool {
	return modelsrefresh.CodexConfigured(cfg)
}

// refreshCodexModels re-reads the Codex model list through
// modelsrefresh.RefreshCodex and prints the outcome.
func refreshCodexModels(ctx context.Context, cmd *cobra.Command, cfg *config.ConfigStore) error {
	result, err := modelsrefresh.RefreshCodex(ctx, cfg)
	if err != nil {
		return err
	}
	if result.Err != nil {
		return result.Err
	}
	for _, c := range result.ContextWindowChanges {
		cmd.Printf("  %s: context window %d → %d\n", c.ID, c.Old, c.New)
	}
	cmd.Printf("%s: %d models (+%d new, -%d removed, %d updated)\n",
		codex.ProviderID, result.Models, result.Added, result.Removed, len(result.ContextWindowChanges))
	return nil
}
