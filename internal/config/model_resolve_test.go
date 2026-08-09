package config

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

func TestParseModelString(t *testing.T) {
	tests := []struct {
		name            string
		modelStr        string
		expectedFilter  string
		expectedModelID string
		setupProviders  func() map[string]ProviderConfig
	}{
		{
			name:            "simple model with no slashes",
			modelStr:        "gpt-4o",
			expectedFilter:  "",
			expectedModelID: "gpt-4o",
			setupProviders:  setupMockProvidersForResolve,
		},
		{
			name:            "valid provider and model",
			modelStr:        "openai/gpt-4o",
			expectedFilter:  "openai",
			expectedModelID: "gpt-4o",
			setupProviders:  setupMockProvidersForResolve,
		},
		{
			name:            "model with multiple slashes and first part is invalid provider",
			modelStr:        "moonshot/kimi-k2",
			expectedFilter:  "",
			expectedModelID: "moonshot/kimi-k2",
			setupProviders:  setupMockProvidersForResolve,
		},
		{
			name:            "full path with valid provider and model with slashes",
			modelStr:        "synthetic/moonshot/kimi-k2",
			expectedFilter:  "synthetic",
			expectedModelID: "moonshot/kimi-k2",
			setupProviders:  setupMockProvidersWithSlashesForResolve,
		},
		{
			name:            "empty model string",
			modelStr:        "",
			expectedFilter:  "",
			expectedModelID: "",
			setupProviders:  setupMockProvidersForResolve,
		},
		{
			name:            "model with trailing slash but valid provider",
			modelStr:        "openai/",
			expectedFilter:  "openai",
			expectedModelID: "",
			setupProviders:  setupMockProvidersForResolve,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providers := tt.setupProviders()
			filter, modelID := ParseModelString(providers, tt.modelStr)

			require.Equal(t, tt.expectedFilter, filter, "provider filter mismatch")
			require.Equal(t, tt.expectedModelID, modelID, "model ID mismatch")
		})
	}
}

func setupMockProvidersForResolve() map[string]ProviderConfig {
	return map[string]ProviderConfig{
		"openai": {
			ID:     "openai",
			Name:   "OpenAI",
			Models: []catwalk.Model{{ID: "gpt-4o"}, {ID: "gpt-4o-mini"}},
		},
		"anthropic": {
			ID:     "anthropic",
			Name:   "Anthropic",
			Models: []catwalk.Model{{ID: "claude-3-sonnet"}, {ID: "claude-3-opus"}},
		},
	}
}

func setupMockProvidersWithSlashesForResolve() map[string]ProviderConfig {
	return map[string]ProviderConfig{
		"synthetic": {
			ID:   "synthetic",
			Name: "Synthetic",
			Models: []catwalk.Model{
				{ID: "moonshot/kimi-k2"},
				{ID: "deepseek/deepseek-chat"},
			},
		},
		"openai": {
			ID:     "openai",
			Name:   "OpenAI",
			Models: []catwalk.Model{{ID: "gpt-4o"}},
		},
	}
}

func TestFindModelMatches(t *testing.T) {
	tests := []struct {
		name             string
		modelStr         string
		expectedProvider string
		expectedModelID  string
		expectError      bool
		errorContains    string
		setupProviders   func() map[string]ProviderConfig
	}{
		{
			name:             "simple model found in one provider",
			modelStr:         "gpt-4o",
			expectedProvider: "openai",
			expectedModelID:  "gpt-4o",
			expectError:      false,
			setupProviders:   setupMockProvidersForResolve,
		},
		{
			name:             "model with slashes in ID",
			modelStr:         "moonshot/kimi-k2",
			expectedProvider: "synthetic",
			expectedModelID:  "moonshot/kimi-k2",
			expectError:      false,
			setupProviders:   setupMockProvidersWithSlashesForResolve,
		},
		{
			name:             "provider and model with slashes in ID",
			modelStr:         "synthetic/moonshot/kimi-k2",
			expectedProvider: "synthetic",
			expectedModelID:  "moonshot/kimi-k2",
			expectError:      false,
			setupProviders:   setupMockProvidersWithSlashesForResolve,
		},
		{
			name:           "model not found",
			modelStr:       "nonexistent-model",
			expectError:    true,
			errorContains:  "not found",
			setupProviders: setupMockProvidersForResolve,
		},
		{
			name:           "invalid provider specified",
			modelStr:       "nonexistent-provider/gpt-4o",
			expectError:    true,
			errorContains:  "provider",
			setupProviders: setupMockProvidersForResolve,
		},
		{
			name:          "model found in multiple providers without provider filter",
			modelStr:      "shared-model",
			expectError:   true,
			errorContains: "multiple providers",
			setupProviders: func() map[string]ProviderConfig {
				return map[string]ProviderConfig{
					"openai": {
						ID:     "openai",
						Models: []catwalk.Model{{ID: "shared-model"}},
					},
					"anthropic": {
						ID:     "anthropic",
						Models: []catwalk.Model{{ID: "shared-model"}},
					},
				}
			},
		},
		{
			name:           "empty model string",
			modelStr:       "",
			expectError:    true,
			errorContains:  "not found",
			setupProviders: setupMockProvidersForResolve,
		},
		{
			name:             "model ID casing is case-insensitive",
			modelStr:         "openai/GPT-4O",
			expectedProvider: "openai",
			expectedModelID:  "gpt-4o",
			expectError:      false,
			setupProviders:   setupMockProvidersForResolve,
		},
		{
			name:             "model ID with slash but no matching provider resolves as full model ID",
			modelStr:         "moonshot/kimi-k2",
			expectedProvider: "synthetic",
			expectedModelID:  "moonshot/kimi-k2",
			expectError:      false,
			setupProviders: func() map[string]ProviderConfig {
				// No "moonshot" provider registered, only "synthetic" hosting
				// a model whose ID itself contains a slash.
				return map[string]ProviderConfig{
					"synthetic": {
						ID:     "synthetic",
						Models: []catwalk.Model{{ID: "moonshot/kimi-k2"}},
					},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providers := tt.setupProviders()

			// Use FindModelMatches with the model as "large" and empty "small".
			matches, _, err := FindModelMatches(providers, tt.modelStr, "")
			if err != nil {
				if tt.expectError {
					require.Contains(t, err.Error(), tt.errorContains)
				} else {
					require.NoError(t, err)
				}
				return
			}

			// Validate the matches.
			match, err := ValidateModelMatches(matches, tt.modelStr, "large")

			if tt.expectError {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.errorContains)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expectedProvider, match.Provider)
				require.Equal(t, tt.expectedModelID, match.ModelID)
			}
		})
	}
}

func TestResolveModelString(t *testing.T) {
	tests := []struct {
		name             string
		modelStr         string
		expectedProvider string
		expectedModelID  string
		expectError      bool
		errorContains    string
		setupProviders   func() map[string]ProviderConfig
	}{
		{
			name:             "provider/model happy path",
			modelStr:         "openai/gpt-4o",
			expectedProvider: "openai",
			expectedModelID:  "gpt-4o",
			setupProviders:   setupMockProvidersForResolve,
		},
		{
			name:             "bare model unambiguous",
			modelStr:         "claude-3-opus",
			expectedProvider: "anthropic",
			expectedModelID:  "claude-3-opus",
			setupProviders:   setupMockProvidersForResolve,
		},
		{
			name:          "bare model ambiguous across providers",
			modelStr:      "shared-model",
			expectError:   true,
			errorContains: "multiple providers",
			setupProviders: func() map[string]ProviderConfig {
				return map[string]ProviderConfig{
					"openai":    {ID: "openai", Models: []catwalk.Model{{ID: "shared-model"}}},
					"anthropic": {ID: "anthropic", Models: []catwalk.Model{{ID: "shared-model"}}},
				}
			},
		},
		{
			name:           "provider not found",
			modelStr:       "nonexistent-provider/gpt-4o",
			expectError:    true,
			errorContains:  "provider",
			setupProviders: setupMockProvidersForResolve,
		},
		{
			name:           "model not found",
			modelStr:       "nonexistent-model",
			expectError:    true,
			errorContains:  "not found",
			setupProviders: setupMockProvidersForResolve,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providers := tt.setupProviders()
			match, err := ResolveModelString(providers, tt.modelStr)

			if tt.expectError {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.errorContains)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.expectedProvider, match.Provider)
			require.Equal(t, tt.expectedModelID, match.ModelID)
		})
	}
}

// TestModelFilterMatches_CaseInsensitiveProviderName pins the fix where
// provider name comparisons must ignore case: the previous cmd/run.go
// implementation (matchesModel) compared provider names with an exact
// string match, so a differently-cased provider filter would silently
// fail to match.
func TestModelFilterMatches_CaseInsensitiveProviderName(t *testing.T) {
	require.True(t, modelFilterMatches("gpt-4o", "OpenAI", "gpt-4o", "openai"))
	require.False(t, modelFilterMatches("gpt-4o", "Anthropic", "gpt-4o", "openai"))
}
