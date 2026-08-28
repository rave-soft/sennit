package shellconfig

import (
	"context"
	"io"
	"log/slog"
)

// handleProvider implements the `provider` builtin.
//
// Usage:
//
//	provider add <id> [--name NAME] [--type TYPE] [--api-key KEY]
//	    [--base-url URL] [--disable true|false] [--flat-rate true|false]
//	    [--discover-models true|false] [--system-prompt-prefix TEXT]
//	    [--extra-header KEY VALUE] [--extra-body JSON]
//	    [--provider-options JSON]
//	provider remove <id>   (alias: rm)
//
// "add" defines or updates a provider; repeated calls with the same <id>
// update the same entry. "remove" removes a provider and all its children.
func handleProvider(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return dispatchAddRemove(ctx, args, stderr,
		"provider", "usage: provider add <id> [flags] | provider remove <id>",
		providerAdd, providerRemove)
}

// providerAddFlags is the declarative flag surface for `provider add`.
var providerAddFlags = []flagSpec{
	{name: "--name", jsonKey: "name", kind: flagString, op: opSet},
	{name: "--type", jsonKey: "type", kind: flagString, op: opSet},
	{name: "--api-key", jsonKey: "api_key", kind: flagString, op: opSet},
	{name: "--base-url", jsonKey: "base_url", kind: flagString, op: opSet},
	{name: "--disable", jsonKey: "disable", kind: flagBool, op: opSet},
	{name: "--flat-rate", jsonKey: "flat_rate", kind: flagBool, op: opSet},
	{name: "--discover-models", jsonKey: "discover_models", kind: flagBool, op: opSet},
	{name: "--system-prompt-prefix", jsonKey: "system_prompt_prefix", kind: flagString, op: opSet},
	{name: "--extra-header", child: "extra_headers", kind: flagKeyValue, op: opSetChild},
	{name: "--extra-body", child: "extra_body", kind: flagJSONObject, op: opMergeChild},
	{name: "--provider-options", child: "provider_options", kind: flagJSONObject, op: opMergeChild},
}

func providerAdd(b *ConfigBuilder, args []string, stderr io.Writer) error {
	if len(args) < 3 {
		return usage(stderr, "usage: provider add <id> [--name NAME] [--type TYPE] [--api-key KEY] [--base-url URL] [--disable true|false] [--flat-rate true|false] [--discover-models true|false] [--system-prompt-prefix TEXT] [--extra-header KEY VALUE] [--extra-body JSON] [--provider-options JSON]")
	}
	id := args[2]
	slog.Info("Provider defined in shell config", "provider", id)
	p := childMap(b.section("providers"), id)

	if err := applyFlags(providerAddFlags, args, 3, p, "provider add", stderr); err != nil {
		return err
	}

	slog.Debug("Provider recorded", "provider", id)
	return nil
}

func providerRemove(b *ConfigBuilder, args []string, stderr io.Writer) error {
	if len(args) < 3 {
		return usage(stderr, "usage: provider remove <id>")
	}
	id := args[2]
	removeNamedEntry(b, "providers", "Provider", id)
	return nil
}
