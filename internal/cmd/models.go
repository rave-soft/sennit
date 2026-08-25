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
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/modelcache"
	"github.com/rave-soft/sennit/internal/discover"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/spf13/cobra"
)

var modelsCmd = &cobra.Command{
	Use:   "models",
	Short: "List all available models from known providers",
	Long:  `List all available models from known providers. Shows provider name and model IDs. Unconfigured providers are marked with (not configured).`,
	Example: `# List all available models
sennit models

# Search models
sennit models gpt5`,
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		debug, _ := cmd.Flags().GetBool("debug")
		_, cfg, err := initConfig(cmd, debug)
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
				models:     matchingModelIDs(provider.ID, provider.Name, provider.Models, term),
			}
			if len(entry.models) > 0 {
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
				models:     matchingModelIDs(providerID, kp.Name, kp.Models, term),
			}
			if len(entry.models) > 0 {
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

// matchingModelIDs returns the sorted model IDs of models that pass the
// search term, whether the provider is a configured one (provider.Models)
// or a catwalk-known one not yet in the user's config (kp.Models) — both
// shapes are searched the same way, against provider ID, provider name,
// model ID, and model name. An empty term matches everything.
func matchingModelIDs(providerID, providerName string, models []catwalk.Model, term string) []string {
	var ids []string
	for _, model := range models {
		if term != "" {
			matched := false
			for _, s := range []string{providerID, providerName, model.ID, model.Name} {
				if strings.Contains(strings.ToLower(s), term) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		ids = append(ids, model.ID)
	}
	slices.Sort(ids)
	return ids
}

var refreshCmd = &cobra.Command{
	Use:   "refresh [provider-id]",
	Short: "Force a model-discovery refresh for custom providers",
	Long: `Force a model-discovery refresh for custom (self-hosted) providers.

Unlike the automatic discovery that runs on load, this always re-queries the
provider's /models endpoint and overwrites the persisted model list, even if
models are already configured.

With no arguments, every custom provider (a provider with a base_url that
isn't part of the built-in catwalk catalog) is refreshed, plus Codex when
signed in. With a provider-id argument, only that provider is refreshed.

Codex is the one catalog provider this covers: its model list is per-account
and fetched from its own backend, so it would otherwise only be re-read by
signing in again.`,
	Example: `# Refresh every custom provider (and Codex, if signed in)
sennit models refresh

# Refresh a single provider
sennit models refresh my-local-llm

# Re-read the Codex model list
sennit models refresh codex`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		debug, _ := cmd.Flags().GetBool("debug")
		_, cfg, err := initConfig(cmd, debug)
		if err != nil {
			return err
		}

		knownIDs := make(map[string]bool)
		for _, kp := range cfg.KnownProviders() {
			knownIDs[string(kp.ID)] = true
		}

		baseCtx := cmdContext(cmd)

		// Codex is a catalog provider, so it is not discovered against a
		// /models endpoint like the custom ones below — but its list is
		// per-account, fetched from its own backend, and until now only
		// written at sign-in. Refresh is where re-reading it belongs.
		var refreshCodex bool
		if len(args) == 1 && args[0] == codex.ProviderID {
			if !codexConfigured(cfg) {
				return fmt.Errorf("not signed in to Codex; run `sennit login codex`")
			}
			return refreshCodexModels(baseCtx, cmd, cfg)
		}
		if len(args) == 0 {
			refreshCodex = codexConfigured(cfg)
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

		if len(targets) == 0 && !refreshCodex {
			cmd.Println("no custom providers to refresh")
			return nil
		}

		var hadFailure bool
		if refreshCodex {
			if err := refreshCodexModels(baseCtx, cmd, cfg); err != nil {
				hadFailure = true
				cmd.PrintErrf("%s: refresh failed: %v\n", codex.ProviderID, err)
			}
		}
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
					models = enricher.EnrichModels(discoverCtx, dcfg, cfg.Resolver(), models)
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

			added, removed := diffModelIDs(pc.Models, models)

			// Discovered models live in the global model-discovery cache,
			// not providers.<id>.models in sennit.json — see
			// validateCustomProviders in internal/config/load.go.
			globalDataPath, err := cfg.ConfigPath(config.ScopeGlobal)
			if err != nil {
				hadFailure = true
				cmd.PrintErrf("%s: refresh failed: %v\n", id, err)
				continue
			}
			if err := modelcache.New(globalDataPath).Save(id, models); err != nil {
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

// diffModelIDs compares a provider's currently configured model list against
// a freshly discovered/fetched one and reports how many IDs are new and how
// many dropped out. It only compares by ID; a caller that also cares about
// per-model field changes (Codex's context-window updates, for instance)
// walks the fresh list itself and uses this just for the counts.
func diffModelIDs(existing, fresh []catwalk.Model) (added, removed int) {
	existingIDs := make(map[string]bool, len(existing))
	for _, m := range existing {
		existingIDs[m.ID] = true
	}
	freshIDs := make(map[string]bool, len(fresh))
	for _, m := range fresh {
		freshIDs[m.ID] = true
		if !existingIDs[m.ID] {
			added++
		}
	}
	for id := range existingIDs {
		if !freshIDs[id] {
			removed++
		}
	}
	return added, removed
}

func init() {
	rootCmd.AddCommand(modelsCmd)
	modelsCmd.AddCommand(refreshCmd)
}
