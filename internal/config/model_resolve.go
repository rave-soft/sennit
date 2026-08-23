package config

import (
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	xstrings "github.com/charmbracelet/x/exp/strings"
)

// ModelMatch represents a model resolved to a specific provider.
type ModelMatch struct {
	Provider string
	ModelID  string
}

// ParseModelString parses a model string into a provider filter and model
// ID. Format: "model-name" or "provider/model-name" or
// "synthetic/moonshot/kimi-k2". Only the first component is checked
// against known provider names; if it does not match, the entire string is
// treated as the model ID (which may itself contain slashes).
func ParseModelString(providers map[string]ProviderConfig, modelStr string) (providerFilter, modelID string) {
	parts := strings.Split(modelStr, "/")
	if len(parts) == 1 {
		return "", parts[0]
	}
	// Check if the first part is a valid provider name.
	if _, ok := providers[parts[0]]; ok {
		return parts[0], strings.Join(parts[1:], "/")
	}

	// First part is not a valid provider, treat entire string as model ID.
	return "", modelStr
}

// FindModelMatches searches providers for a model string and returns its
// matches. Provider and model name comparisons are case-insensitive.
func FindModelMatches(providers map[string]ProviderConfig, modelStr string) ([]ModelMatch, error) {
	providerFilter, modelID := ParseModelString(providers, modelStr)

	if providerFilter != "" {
		if _, ok := providers[providerFilter]; !ok {
			return nil, fmt.Errorf("provider %q not found in configuration. Use 'sennit models' to list available models", providerFilter)
		}
	}

	matches := collectModelMatches(providers, providerFilter, modelID)

	return matches, nil
}

// collectModelMatches gathers every enabled provider's models that satisfy
// the requested model ID and optional provider filter. It is the shared
// body of FindModelMatches and ResolveModelString, which differ only in
// what they do with the result — one hands the matches back, the other
// insists on exactly one.
func collectModelMatches(providers map[string]ProviderConfig, providerFilter, modelID string) []ModelMatch {
	var matches []ModelMatch
	for name, provider := range providers {
		if provider.Disable {
			continue
		}
		for _, m := range provider.Models {
			if modelFilterMatches(modelID, providerFilter, m.ID, name) {
				matches = append(matches, ModelMatch{Provider: name, ModelID: m.ID})
			}
		}
	}
	return matches
}

// modelFilterMatches reports whether a model/provider pair satisfies the
// requested model ID and (optional) provider filter, case-insensitively.
func modelFilterMatches(modelFilter, providerFilter, model, provider string) bool {
	return modelFilter != "" && strings.EqualFold(model, modelFilter) &&
		(providerFilter == "" || strings.EqualFold(provider, providerFilter))
}

// ResolveModelString resolves a single "model" or "provider/model" string
// against the configured providers, exactly like FindModelMatches does for
// the large/small slots. It returns the single unambiguous match, or an
// error naming why not (provider not found / model not found / ambiguous
// across providers).
func ResolveModelString(providers map[string]ProviderConfig, modelStr string) (ModelMatch, error) {
	providerFilter, modelID := ParseModelString(providers, modelStr)
	if providerFilter != "" {
		if _, ok := providers[providerFilter]; !ok {
			return ModelMatch{}, fmt.Errorf("provider %q not found in configuration", providerFilter)
		}
	}

	matches := collectModelMatches(providers, providerFilter, modelID)

	return ValidateModelMatches(matches, modelID, "agent")
}

// ValidateModelMatches ensures exactly one match exists, returning a
// descriptive error otherwise.
func ValidateModelMatches(matches []ModelMatch, modelID, label string) (ModelMatch, error) {
	switch {
	case len(matches) == 0:
		return ModelMatch{}, fmt.Errorf("%s model %q not found", label, modelID)
	case len(matches) > 1:
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.Provider
		}
		return ModelMatch{}, fmt.Errorf(
			"%s model: model %q found in multiple providers: %s. Please specify provider using 'provider/model' format",
			label,
			modelID,
			xstrings.EnglishJoin(names, true),
		)
	}
	return matches[0], nil
}

func (c *Config) defaultModelSelection(knownProviders []catwalk.Provider) (model SelectedModel, err error) {
	if len(knownProviders) == 0 && c.Providers.Len() == 0 {
		err = fmt.Errorf("no providers configured, please configure at least one provider")
		return model, err
	}

	// Use the first provider enabled based on the known providers order
	// if no provider found that is known use the first provider configured
	for _, p := range knownProviders {
		providerConfig, ok := c.Providers.Get(string(p.ID))
		if !ok || providerConfig.Disable {
			continue
		}
		defaultModel := c.GetModel(string(p.ID), p.DefaultLargeModelID)
		if defaultModel == nil {
			slog.Warn("Default model not found for provider", "model", p.DefaultLargeModelID, "provider", p.ID)
			if len(providerConfig.Models) == 0 {
				return model, fmt.Errorf("default model %s not found for provider %s", p.DefaultLargeModelID, p.ID)
			}
			defaultModel = &providerConfig.Models[0]
		}
		model = SelectedModel{
			Provider:        string(p.ID),
			Model:           defaultModel.ID,
			MaxTokens:       defaultModel.DefaultMaxTokens,
			ReasoningEffort: defaultModel.DefaultReasoningEffort,
		}
		return model, err
	}

	enabledProviders := c.EnabledProviders()
	slices.SortFunc(enabledProviders, func(a, b ProviderConfig) int {
		return strings.Compare(a.ID, b.ID)
	})

	if len(enabledProviders) == 0 {
		err = fmt.Errorf("no providers configured, please configure at least one provider")
		return model, err
	}

	providerConfig := enabledProviders[0]
	if len(providerConfig.Models) == 0 {
		err = fmt.Errorf("provider %s has no models configured", providerConfig.ID)
		return model, err
	}
	defaultModel := c.GetModel(providerConfig.ID, providerConfig.Models[0].ID)
	model = SelectedModel{
		Provider:  providerConfig.ID,
		Model:     defaultModel.ID,
		MaxTokens: defaultModel.DefaultMaxTokens,
	}
	return model, err
}

// resolvedModel holds the result of resolving the user-configured model
// selection against the provider catalog.
type resolvedModel struct {
	Model    SelectedModel
	Fallback bool // true if Model was corrected to a default
}

// resolveSelectedModel validates the user's configured model selection
// against the provider catalog, falling back to a default when the model ID
// is invalid. It is pure resolution logic: it does not mutate the store or
// touch disk. The caller assigns the result to cfg.Model and persists any
// fallback correction as appropriate.
func resolveSelectedModel(cfg *Config, knownProviders []catwalk.Provider) (resolvedModel, error) {
	var result resolvedModel
	def, err := cfg.defaultModelSelection(knownProviders)
	if err != nil {
		return result, fmt.Errorf("failed to select default model: %w", err)
	}
	selected := def

	modelSelected := cfg.Model
	// The zero SelectedModel{} is the "unset" sentinel (Config.Model has no
	// map to check key presence against anymore), so any field the user set
	// — even just max_tokens, with provider/model left to inherit the
	// default — marks the model as configured.
	modelConfigured := !reflect.DeepEqual(modelSelected, SelectedModel{})
	if modelConfigured {
		if modelSelected.Model != "" {
			selected.Model = modelSelected.Model
		}
		if modelSelected.Provider != "" {
			selected.Provider = modelSelected.Provider
		}
		model := cfg.GetModel(selected.Provider, selected.Model)
		if model == nil {
			cfg.addProblem(Problem{
				Severity: SeverityError,
				Area:     AreaModel,
				Subject:  modelSelected.Provider + "/" + modelSelected.Model,
				Message: fmt.Sprintf(
					"configured main model %s/%s not found — falling back to %s/%s",
					modelSelected.Provider, modelSelected.Model, def.Provider, def.Model,
				),
				Hint: "run 'sennit models' to see available provider/model pairs",
			})
			selected = def
			result.Fallback = true
		} else {
			if modelSelected.MaxTokens > 0 {
				selected.MaxTokens = modelSelected.MaxTokens
			} else {
				selected.MaxTokens = model.DefaultMaxTokens
			}
			if modelSelected.ReasoningEffort != "" {
				selected.ReasoningEffort = modelSelected.ReasoningEffort
			} else {
				selected.ReasoningEffort = model.DefaultReasoningEffort
			}
			selected.Think = modelSelected.Think
			if modelSelected.Temperature != nil {
				selected.Temperature = modelSelected.Temperature
			}
			if modelSelected.TopP != nil {
				selected.TopP = modelSelected.TopP
			}
			if modelSelected.TopK != nil {
				selected.TopK = modelSelected.TopK
			}
			if modelSelected.FrequencyPenalty != nil {
				selected.FrequencyPenalty = modelSelected.FrequencyPenalty
			}
			if modelSelected.PresencePenalty != nil {
				selected.PresencePenalty = modelSelected.PresencePenalty
			}
			if modelSelected.ProviderOptions != nil {
				selected.ProviderOptions = maps.Clone(modelSelected.ProviderOptions)
			}
		}
	}

	result.Model = selected
	return result, nil
}
