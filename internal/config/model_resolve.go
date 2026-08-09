package config

import (
	"fmt"
	"strings"

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

// FindModelMatches searches providers for large/small model strings and
// returns their matches. Provider and model name comparisons are
// case-insensitive.
func FindModelMatches(providers map[string]ProviderConfig, largeModel, smallModel string) ([]ModelMatch, []ModelMatch, error) {
	largeProviderFilter, largeModelID := ParseModelString(providers, largeModel)
	smallProviderFilter, smallModelID := ParseModelString(providers, smallModel)

	// Validate provider filters exist.
	for _, pf := range []struct {
		filter, label string
	}{
		{largeProviderFilter, "large"},
		{smallProviderFilter, "small"},
	} {
		if pf.filter != "" {
			if _, ok := providers[pf.filter]; !ok {
				return nil, nil, fmt.Errorf("%s model: provider %q not found in configuration. Use 'braid models' to list available models", pf.label, pf.filter)
			}
		}
	}

	// Find matching models in a single pass.
	var largeMatches, smallMatches []ModelMatch
	for name, provider := range providers {
		if provider.Disable {
			continue
		}
		for _, m := range provider.Models {
			if modelFilterMatches(largeModelID, largeProviderFilter, m.ID, name) {
				largeMatches = append(largeMatches, ModelMatch{Provider: name, ModelID: m.ID})
			}
			if modelFilterMatches(smallModelID, smallProviderFilter, m.ID, name) {
				smallMatches = append(smallMatches, ModelMatch{Provider: name, ModelID: m.ID})
			}
		}
	}

	return largeMatches, smallMatches, nil
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
