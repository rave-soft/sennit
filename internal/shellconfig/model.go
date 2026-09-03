package shellconfig

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// handleModel implements the `model` builtin.
//
// Usage:
//
//	model add <provider>/<id> [--name NAME] [--context-window N]
//	    [--default-max-tokens N] [--can-reason true|false]
//	    [--supports-images true|false] [--price-input F]
//	    [--price-output F] [--price-cache-create F]
//	    [--price-cache-hit F] [--reasoning-effort low|medium|high]
//	model remove <provider>/<id>   (alias: rm)
//	model [<provider>/<id>] [--think] [--reasoning-effort L]
//	    [--max-tokens N] [--temperature F] [--top-p F] [--top-k N]
//	    [--frequency-penalty F] [--presence-penalty F]
//	    [--provider-options JSON]
//
// "add" registers a model on an existing provider (the provider must have
// been declared with `provider add` first). "remove" removes it. Given a
// <provider>/<id>, the bare form sets the selected model; given no argument
// it prints the current selection as <provider>/<id>. The old `model large`
// and `model small` slot syntax is rejected: Sennit now selects a single
// model, not a large/small pair.
func handleModel(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	b := configBuilderFromCtx(ctx)
	if b == nil {
		return nil
	}
	if len(args) < 2 {
		return modelPrint(b, stdout)
	}

	switch args[1] {
	case "add":
		return modelAdd(b, args, stderr)
	case "remove", "rm":
		return modelRemove(b, args, stderr)
	case "large", "small":
		return usage(stderr, "model: slots are gone; use model <provider/id>")
	default:
		return modelSet(b, args, stderr)
	}
}

// splitProviderModel splits "provider/id" on the first slash. Model ids may
// themselves contain slashes, so only the first separates provider from id.
func splitProviderModel(s string) (provider, id string, ok bool) {
	provider, id, found := strings.Cut(s, "/")
	if !found || provider == "" || id == "" {
		return "", "", false
	}
	return provider, id, true
}

// modelAddFlags is the declarative flag surface for `model add`.
var modelAddFlags = []flagSpec{
	{name: "--name", jsonKey: "name", kind: flagString, op: opSet},
	{name: "--context-window", jsonKey: "context_window", kind: flagInt, op: opSet, validate: func(v any) error {
		// A negative window is not "unknown" the way 0 is - it makes
		// stopOnContextWindow's summarizeBuffer arithmetic go negative,
		// so remaining tokens read as permanently below threshold and a
		// session summarizes on every single step. Reject it here
		// rather than let a typo (e.g. a stray "-1") reach that code.
		if n := v.(int64); n < 0 {
			return fmt.Errorf("--context-window must not be negative, got %d", n)
		}
		return nil
	}},
	{name: "--default-max-tokens", jsonKey: "default_max_tokens", kind: flagInt, op: opSet},
	{name: "--can-reason", jsonKey: "can_reason", kind: flagBool, op: opSet},
	{name: "--supports-images", jsonKey: "supports_attachments", kind: flagBool, op: opSet},
	{name: "--price-input", jsonKey: "cost_per_1m_in", kind: flagFloat, op: opSet},
	{name: "--price-output", jsonKey: "cost_per_1m_out", kind: flagFloat, op: opSet},
	{name: "--price-cache-create", jsonKey: "cost_per_1m_in_cached", kind: flagFloat, op: opSet},
	{name: "--price-cache-hit", jsonKey: "cost_per_1m_out_cached", kind: flagFloat, op: opSet},
	{name: "--reasoning-effort", jsonKey: "default_reasoning_effort", kind: flagString, op: opSet},
}

func modelAdd(b *ConfigBuilder, args []string, stderr io.Writer) error {
	if len(args) < 3 {
		return usage(stderr, "usage: model add <provider>/<id> [--name NAME] [--context-window N] [--default-max-tokens N] [--can-reason true|false] [--supports-images true|false] [--price-input F] [--price-output F] [--price-cache-create F] [--price-cache-hit F] [--reasoning-effort low|medium|high]")
	}
	provider, id, ok := splitProviderModel(args[2])
	if !ok {
		return usage(stderr, fmt.Sprintf("model add: expected <provider>/<id>, got %q", args[2]))
	}

	providers := b.section("providers")
	if _, exists := providers[provider]; !exists {
		return usage(stderr, fmt.Sprintf("model add: provider %q does not exist (declare it with `provider add %s` first)", provider, provider))
	}

	model := map[string]any{"id": id}
	if err := applyFlags(modelAddFlags, args, 3, model, "model add", stderr); err != nil {
		return err
	}

	// providers[provider] may be a tombstone left by an earlier `provider
	// remove` in this script: plain childMap would hand back the
	// {__sennit_tombstone: ...} wrapper itself, and writing "models" beside
	// it corrupts the entry so ParseTombstone rejects the whole config.
	// addLocal already knows how to unwrap a tombstone into its
	// replacement map, and is a no-op passthrough for the common case where
	// `provider add` already resolved this id earlier in the same script.
	p := b.addLocal(providers, "providers", provider)
	// Re-adding a model id replaces the existing entry, matching the
	// update-in-place behavior of `provider add` and `lsp add`.
	modelsArr, _ := p["models"].([]any)
	kept := filterOutByField(modelsArr, "id", id)
	p["models"] = append(kept, model)

	slog.Info("Model added in shell config", "provider", provider, "model", id)
	return nil
}

func modelRemove(b *ConfigBuilder, args []string, stderr io.Writer) error {
	if len(args) < 3 {
		return usage(stderr, "usage: model remove <provider>/<id>")
	}
	provider, id, ok := splitProviderModel(args[2])
	if !ok {
		return usage(stderr, fmt.Sprintf("model remove: expected <provider>/<id>, got %q", args[2]))
	}

	providers := b.section("providers")
	p, exists := providers[provider].(map[string]any)
	if !exists {
		return nil
	}
	// providers[provider] may be a tombstone left by an earlier `provider
	// remove` in this script: a plain type assertion still succeeds (the
	// wrapper is itself a map[string]any), and writing "models" beside
	// __sennit_tombstone corrupts the entry so ParseTombstone rejects the
	// whole config once it round-trips through JSON. The in-builder
	// representation stores the marker as a Tombstone value directly (not
	// yet the JSON-shaped map[string]any ParseTombstone parses), matching
	// the check addLocal already does. The provider is already gone as far
	// as this script is concerned, so removing one of its models is a
	// no-op — same as the !exists case above — rather than resurrecting
	// the provider entry by writing into (or past) the tombstone.
	if _, tombstoned := p[TombstoneKey].(Tombstone); tombstoned {
		return nil
	}
	modelsArr, _ := p["models"].([]any)
	p["models"] = filterOutByField(modelsArr, "id", id)

	slog.Info("Model removed in shell config", "provider", provider, "model", id)
	return nil
}

// modelSelectFlags is the declarative flag surface for the bare `model`
// select form.
var modelSelectFlags = []flagSpec{
	{name: "--think", jsonKey: "think", kind: flagBoolTrue, op: opSet},
	{name: "--reasoning-effort", jsonKey: "reasoning_effort", kind: flagString, op: opSet},
	{name: "--max-tokens", jsonKey: "max_tokens", kind: flagInt, op: opSet},
	{name: "--temperature", jsonKey: "temperature", kind: flagFloat, op: opSet},
	{name: "--top-p", jsonKey: "top_p", kind: flagFloat, op: opSet, validate: func(v any) error {
		f := v.(float64)
		if f < 0 || f > 1 {
			return fmt.Errorf("--top-p expects a value between 0 and 1, got %v", f)
		}
		return nil
	}},
	{name: "--top-k", jsonKey: "top_k", kind: flagInt, op: opSet},
	{name: "--frequency-penalty", jsonKey: "frequency_penalty", kind: flagFloat, op: opSet},
	{name: "--presence-penalty", jsonKey: "presence_penalty", kind: flagFloat, op: opSet},
	{name: "--provider-options", child: "provider_options", kind: flagJSONObject, op: opMergeChild},
}

// modelPrint prints the current selection as <provider>/<id>, or nothing if
// no model is configured yet.
func modelPrint(b *ConfigBuilder, stdout io.Writer) error {
	if sel, ok := b.root["model"].(map[string]any); ok {
		provider, _ := sel["provider"].(string)
		id, _ := sel["model"].(string)
		if provider != "" && id != "" {
			fmt.Fprintln(stdout, provider+"/"+id)
		}
	}
	return nil
}

// modelSet handles the bare `model <provider>/<id> [flags]` form, which sets
// the single selected model.
func modelSet(b *ConfigBuilder, args []string, stderr io.Writer) error {
	provider, id, ok := splitProviderModel(args[1])
	if !ok {
		return usage(stderr, fmt.Sprintf("model: expected <provider>/<id>, got %q", args[1]))
	}

	sel := b.section("model")
	sel["provider"] = provider
	sel["model"] = id

	// The no-slot form is "model <provider/id> [flags]": only two tokens
	// precede the flags, unlike the three-token prefix most builtins have.
	if err := applyFlags(modelSelectFlags, args, 2, sel, "model", stderr); err != nil {
		return err
	}

	slog.Info("Model selected in shell config", "provider", provider, "model", id)
	return nil
}
