package cmd

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/lipgloss/v2/tree"
	"github.com/mattn/go-isatty"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/discover"
	"github.com/spf13/cobra"
)

var modelsCmd = &cobra.Command{
	Use:   "models",
	Short: "List all available models from known providers",
	Long:  `List all available models from known providers. Shows provider name and model IDs. Unconfigured providers are marked with (not configured).`,
	Example: `# List all available models
braid models

# Search models
braid models gpt5`,
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return err
		}

		dataDir, _ := cmd.Flags().GetString("data-dir")
		debug, _ := cmd.Flags().GetBool("debug")

		cfg, err := config.Init(cwd, dataDir, debug)
		if err != nil {
			return err
		}

		term := strings.ToLower(strings.Join(args, " "))

		type providerEntry struct {
			name       string
			models     []string
			configured bool
		}

		entries := make(map[string]*providerEntry)

		// Add configured providers first.
		for providerID, provider := range cfg.Config().Providers.Seq2() {
			if provider.Disable {
				continue
			}
			entry := &providerEntry{
				name:       provider.Name,
				configured: true,
			}
			for _, model := range provider.Models {
				if term != "" {
					matched := false
					for _, s := range []string{provider.ID, provider.Name, model.ID, model.Name} {
						if strings.Contains(strings.ToLower(s), term) {
							matched = true
							break
						}
					}
					if !matched {
						continue
					}
				}
				entry.models = append(entry.models, model.ID)
			}
			if len(entry.models) > 0 {
				slices.Sort(entry.models)
				entries[providerID] = entry
			}
		}

		// Add known but unconfigured providers from catwalk.
		for _, kp := range cfg.KnownProviders() {
			providerID := string(kp.ID)
			if _, exists := entries[providerID]; exists {
				continue
			}
			entry := &providerEntry{
				name:       kp.Name,
				configured: false,
			}
			for _, model := range kp.Models {
				if term != "" {
					matched := false
					for _, s := range []string{providerID, kp.Name, model.ID, model.Name} {
						if strings.Contains(strings.ToLower(s), term) {
							matched = true
							break
						}
					}
					if !matched {
						continue
					}
				}
				entry.models = append(entry.models, model.ID)
			}
			if len(entry.models) > 0 {
				slices.Sort(entry.models)
				entries[providerID] = entry
			}
		}

		var providerIDs []string
		for id := range entries {
			providerIDs = append(providerIDs, id)
		}
		sort.Strings(providerIDs)

		if len(providerIDs) == 0 && len(args) == 0 {
			return fmt.Errorf("no providers found")
		}
		if len(providerIDs) == 0 {
			return fmt.Errorf("no providers found matching %q", term)
		}

		if !isatty.IsTerminal(os.Stdout.Fd()) {
			for _, providerID := range providerIDs {
				entry := entries[providerID]
				for _, modelID := range entry.models {
					fmt.Println(providerID + "/" + modelID)
				}
			}
			return nil
		}

		t := tree.New()
		for _, providerID := range providerIDs {
			entry := entries[providerID]
			label := providerID
			if !entry.configured {
				label += " (not configured)"
			}
			providerNode := tree.Root(label)
			for _, modelID := range entry.models {
				providerNode.Child(modelID)
			}
			t.Child(providerNode)
		}

		cmd.Println(t)
		return nil
	},
}

var refreshCmd = &cobra.Command{
	Use:   "refresh [provider-id]",
	Short: "Force a model-discovery refresh for custom providers",
	Long: `Force a model-discovery refresh for custom (self-hosted) providers.

Unlike the automatic discovery that runs on load, this always re-queries the
provider's /models endpoint and overwrites the persisted model list, even if
models are already configured.

With no arguments, every custom provider (a provider with a base_url that
isn't part of the built-in catwalk catalog) is refreshed. With a
provider-id argument, only that provider is refreshed.`,
	Example: `# Refresh every custom provider
braid models refresh

# Refresh a single provider
braid models refresh my-local-llm`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return err
		}

		dataDir, _ := cmd.Flags().GetString("data-dir")
		debug, _ := cmd.Flags().GetBool("debug")

		cfg, err := config.Init(cwd, dataDir, debug)
		if err != nil {
			return err
		}

		knownIDs := make(map[string]bool)
		for _, kp := range cfg.KnownProviders() {
			knownIDs[string(kp.ID)] = true
		}

		var targets []string
		if len(args) == 1 {
			id := args[0]
			if knownIDs[id] {
				return fmt.Errorf("provider %q is a known catalog provider; refresh only applies to custom providers", id)
			}
			pc, ok := cfg.Config().Providers.Get(id)
			if !ok {
				return fmt.Errorf("provider %q not found in config", id)
			}
			if pc.BaseURL == "" {
				return fmt.Errorf("provider %q has no base_url configured", id)
			}
			targets = []string{id}
		} else {
			for id, pc := range cfg.Config().Providers.Seq2() {
				if knownIDs[id] || pc.BaseURL == "" || pc.Disable {
					continue
				}
				targets = append(targets, id)
			}
			sort.Strings(targets)
		}

		if len(targets) == 0 {
			cmd.Println("no custom providers to refresh")
			return nil
		}

		// cmd.Context() is nil unless the command was dispatched through
		// Execute(); fall back to Background() so RunE also works when
		// invoked directly (as tests do).
		baseCtx := cmd.Context()
		if baseCtx == nil {
			baseCtx = context.Background()
		}

		var hadFailure bool
		for _, id := range targets {
			pc, _ := cfg.Config().Providers.Get(id)

			// discover_models: false is a hard stop, matching the guard
			// discoverCustomProviderModels applies at load time (see
			// load.go) — refresh must not second-guess an explicit opt-out.
			if pc.AutoDiscoverModels != nil && !*pc.AutoDiscoverModels {
				hadFailure = true
				cmd.PrintErrf("discovery disabled for %s (discover_models: false); define models in the config\n", id)
				continue
			}

			// A hand-written models list must never be silently clobbered
			// by a refresh. discover_models: true is the explicit escape
			// hatch — it already means "always refresh, my models win on
			// ID conflicts" at load time, so it overrides this guard too.
			wantsDiscovery := pc.AutoDiscoverModels != nil && *pc.AutoDiscoverModels
			if pc.ModelsSource == config.ModelsSourceConfig && !wantsDiscovery {
				cmd.Printf("%s: models are explicitly defined in config; refresh skipped\n", id)
				continue
			}

			discoverCtx, cancel := context.WithTimeout(baseCtx, 3*time.Second)
			dcfg := discover.Config{
				ID:             id,
				BaseURL:        pc.BaseURL,
				APIKey:         pc.APIKey,
				ExtraHeaders:   pc.ExtraHeaders,
				ExistingModels: nil,
				ProxyURL:       pc.ProxyURL,
			}
			providerType := cmp.Or(pc.Type, catwalk.TypeOpenAICompat)

			models, discErr := discover.DiscoverModels(discoverCtx, dcfg, cfg.Resolver())
			if discErr == nil && len(models) > 0 {
				if enricher := discover.GetEnricher(string(providerType)); enricher != nil {
					models, _ = enricher.EnrichModels(discoverCtx, dcfg, cfg.Resolver(), models)
				}
			}
			cancel()

			if discErr != nil {
				hadFailure = true
				cmd.PrintErrf("%s: refresh failed: %v\n", id, discErr)
				continue
			}
			if len(models) == 0 {
				hadFailure = true
				cmd.PrintErrf("%s: refresh failed: no models returned\n", id)
				continue
			}

			existing := make(map[string]bool, len(pc.Models))
			for _, m := range pc.Models {
				existing[m.ID] = true
			}
			fresh := make(map[string]bool, len(models))
			var added, removed int
			for _, m := range models {
				fresh[m.ID] = true
				if !existing[m.ID] {
					added++
				}
			}
			for existingID := range existing {
				if !fresh[existingID] {
					removed++
				}
			}

			// Discovered models live in the global model-discovery cache,
			// not providers.<id>.models in braid.json — see
			// validateCustomProviders in internal/config/load.go.
			if err := cfg.SaveCachedProviderModels(id, models); err != nil {
				hadFailure = true
				cmd.PrintErrf("%s: refresh failed: %v\n", id, err)
				continue
			}

			cmd.Printf("%s: %d models (+%d new, -%d removed)\n", id, len(models), added, removed)
		}

		if hadFailure {
			return fmt.Errorf("one or more providers failed to refresh")
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(modelsCmd)
	modelsCmd.AddCommand(refreshCmd)
}
