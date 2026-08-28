package shellconfig

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
)

// handleOption implements the `option` builtin.
//
// Usage: option <key> <value>
//
// Sets a single option field. The key is a kebab-case name; for list fields
// (context-path, disable-skill, etc.) each call appends to the list.
//
// "option reset <list-key>" wipes a list back to empty, dropping values set
// earlier in the script or via source. Values added after the reset are kept.
//
// Some config fields are phrased negatively (disable_metrics). Those are
// exposed positively — the user sets "metrics false" and it is stored as
// "disable_metrics true".
//
// Examples:
//
//	option data-directory .sennit
//	option context-path .cursorrules
//	option reset skill-path
//	option metrics false
//	option debug true
//	option auto-lsp false
//
// Boolean shortcuts: for boolean fields, omitting the value sets it to true.
func handleOption(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	b := configBuilderFromCtx(ctx)
	if b == nil {
		return nil
	}
	if len(args) < 2 {
		return usage(stderr, "usage: option <key> [value]")
	}

	key := args[1]
	o := b.section("options")

	if key == "ui" {
		return optionUI(o, args, stderr)
	}

	// "option reset <key>" wipes a list back to empty. Because the builder
	// applies operations in execution order, this is just an assignment:
	// values added after the reset are kept, earlier ones are dropped.
	if key == "reset" {
		if len(args) < 3 {
			return usage(stderr, "usage: option reset <list-key>")
		}
		target := args[2]
		spec, ok := optionSpecs[target]
		if !ok {
			return usage(stderr, fmt.Sprintf("option: unknown key %q", target))
		}
		if spec.kind != optList {
			return usage(stderr, fmt.Sprintf("option: reset only applies to list options, %q is not one", target))
		}
		o[spec.jsonKey] = []any{}
		slog.Info("Option list reset in shell config", "key", target)
		return nil
	}

	spec, ok := optionSpecs[key]
	if !ok {
		return usage(stderr, fmt.Sprintf("option: unknown key %q", key))
	}

	var val string
	if len(args) >= 3 {
		val = args[2]
	}

	return setOption(o, spec, "option: "+key, val, true, stderr)
}

// optionKind is the value type of a user-facing option key.
type optionKind int

const (
	optString optionKind = iota
	optBool
	optList
	optInt
	// optDuration is a Go duration string ("4m", "90s"): stored as the
	// string the user typed, but rejected here if it does not parse, so a
	// typo is an error at the line that made it rather than a silently
	// ignored value at the point something reads it.
	optDuration
)

// optionSpec describes one user-facing option key: the JSON field it writes,
// its value type, and how that value is validated and located. inverted
// stores a boolean's negation (disable_metrics for "metrics"). enum
// restricts a string to a fixed set. path is the child-map path the value
// nests under (e.g. "attribution", or "completions" under options.tui);
// empty stores directly on the caller's root map. implies is a sibling
// jsonKey defaulted to true, if unset, once this option is set —
// attribution-trailer-style uses it to turn attribution on by default.
// nonNegative rejects negative integers.
type optionSpec struct {
	jsonKey     string
	kind        optionKind
	inverted    bool
	enum        []string
	path        []string
	implies     string
	nonNegative bool
}

// optionSpecs maps user-facing kebab-case keys to their JSON field and type.
// This is the single source of truth for option key handling; the kind,
// enum, and path fields drive parsing and validation so there is no
// separate switch to drift out of sync.
//
// Not exhaustive by design: "option ui ..." keys live in uiOptionSpecs
// (nested under options.tui, with their own two-token key syntax), and
// "option ui keybinding" is variadic and stays a bespoke case in optionUI.
var optionSpecs = map[string]optionSpec{
	// Boolean fields (stored as-is).
	"debug":     {jsonKey: "debug", kind: optBool},
	"debug-lsp": {jsonKey: "debug_lsp", kind: optBool},
	"auto-lsp":  {jsonKey: "auto_lsp", kind: optBool},
	"progress":  {jsonKey: "progress", kind: optBool},

	// Boolean fields exposed positively but stored as their negation.
	"metrics":           {jsonKey: "disable_metrics", kind: optBool, inverted: true},
	"auto-summarize":    {jsonKey: "disable_auto_summarize", kind: optBool, inverted: true},
	"default-providers": {jsonKey: "disable_default_providers", kind: optBool, inverted: true},

	// String fields.
	"notifications":  {jsonKey: "notifications", kind: optString},
	"data-directory": {jsonKey: "data_directory", kind: optString},
	"initialize-as":  {jsonKey: "initialize_as", kind: optString},

	// Integer fields.
	"history-retention-days": {jsonKey: "history_retention_days", kind: optInt},

	// Idle auto-summarize, nested under options.auto_summarize_idle.
	"auto-summarize-idle":        {jsonKey: "enabled", kind: optBool, path: []string{"auto_summarize_idle"}},
	"auto-summarize-idle-tokens": {jsonKey: "context_tokens", kind: optInt, path: []string{"auto_summarize_idle"}, nonNegative: true},
	"auto-summarize-idle-after":  {jsonKey: "after", kind: optDuration, path: []string{"auto_summarize_idle"}},

	// List fields. Keys are singular because each call appends one value.
	"context-path":        {jsonKey: "context_paths", kind: optList},
	"global-context-path": {jsonKey: "global_context_paths", kind: optList},
	"skill-path":          {jsonKey: "skills_paths", kind: optList},
	"disable-skill":       {jsonKey: "disabled_skills", kind: optList},

	// Attribution fields, nested under options.attribution. Setting the
	// trailer style implies attribution is on unless already said otherwise.
	"attribution-trailer-style":  {jsonKey: "trailer_style", kind: optString, path: []string{"attribution"}, enum: []string{"none", "assisted-by"}, implies: "generated_with"},
	"attribution-generated-with": {jsonKey: "generated_with", kind: optBool, path: []string{"attribution"}},
}

// uiOptionSpecs maps `option ui <key>` names to their spec, nested under
// options.tui (and further under "completions" for the two completions
// fields). Unlike optionSpecs' top-level keys, these never accept the
// bare-flag boolean shorthand: "option ui compact" without a value is a
// usage error, enforced by optionUI before dispatch.
var uiOptionSpecs = map[string]optionSpec{
	"compact":     {jsonKey: "compact_mode", kind: optBool},
	"transparent": {jsonKey: "transparent", kind: optBool},
	"diff":        {jsonKey: "diff_mode", kind: optString, enum: []string{"unified", "split"}},
	"scrollbar":   {jsonKey: "scrollbar", kind: optString, enum: []string{"default", "always", "never"}},
	// Spelled out rather than taken from config.SpinnerModes: config
	// imports this package, so this package cannot import config.
	// TestUIOptionSpinnerMatchesConfig (an external test package, which
	// may import config) is what holds the two lists together.
	"spinner":               {jsonKey: "spinner", kind: optString, enum: []string{"scramble", "pulse", "dots", "none"}},
	"completions-max-depth": {jsonKey: "max_depth", kind: optInt, path: []string{"completions"}, nonNegative: true},
	"completions-max-items": {jsonKey: "max_items", kind: optInt, path: []string{"completions"}, nonNegative: true},
}

// setOption validates val against spec and stores it under root (following
// spec.path to a nested child map first), then applies spec.implies. label
// prefixes error messages, e.g. "option: debug" or "option ui compact".
// shorthand allows an empty val to mean true for booleans, matching the
// top-level bare-flag form; "option ui ..." fields pass false so an
// explicit empty value still fails bool parsing instead of defaulting.
func setOption(root map[string]any, spec optionSpec, label, val string, shorthand bool, stderr io.Writer) error {
	target := root
	for _, p := range spec.path {
		target = childMap(target, p)
	}

	if val == "" && spec.kind != optBool {
		return usage(stderr, label+" requires a value")
	}

	switch spec.kind {
	case optList:
		target[spec.jsonKey] = appendArr(target, spec.jsonKey, val)

	case optInt:
		n, err := strconv.Atoi(val)
		if err != nil || (spec.nonNegative && n < 0) {
			want := "an integer"
			if spec.nonNegative {
				want = "a non-negative integer"
			}
			return usage(stderr, fmt.Sprintf("%s expects %s, got %q", label, want, val))
		}
		target[spec.jsonKey] = n

	case optBool:
		// If no value, default to true; inverted keys store the negation,
		// so a positive key like "metrics" maps onto "disable_metrics".
		bv := true
		if val != "" || !shorthand {
			parsed, err := parseBool(val)
			if err != nil {
				return usage(stderr, fmt.Sprintf("%s expects true/false, got %q", label, val))
			}
			bv = parsed
		}
		if spec.inverted {
			bv = !bv
		}
		target[spec.jsonKey] = bv

	case optDuration:
		d, err := time.ParseDuration(val)
		if err != nil || d <= 0 {
			return usage(stderr, fmt.Sprintf("%s expects a positive duration like 4m or 90s, got %q", label, val))
		}
		target[spec.jsonKey] = val

	default: // optString
		if len(spec.enum) > 0 && !slices.Contains(spec.enum, val) {
			return usage(stderr, fmt.Sprintf("%s expects %s, got %q", label, joinEnum(spec.enum), val))
		}
		target[spec.jsonKey] = val
	}

	if spec.implies != "" {
		if _, ok := target[spec.implies]; !ok {
			target[spec.implies] = true
		}
	}

	slog.Info("Option set in shell config", "key", label, "value", target[spec.jsonKey])
	return nil
}

// joinEnum renders a set of accepted values for an error message, e.g.
// ["default", "always", "never"] -> "default, always, or never".
func joinEnum(vals []string) string {
	switch len(vals) {
	case 0:
		return ""
	case 1:
		return vals[0]
	case 2:
		return vals[0] + " or " + vals[1]
	default:
		return strings.Join(vals[:len(vals)-1], ", ") + ", or " + vals[len(vals)-1]
	}
}

// optionUI implements "option ui <key> <value>" for TUI-specific settings
// that live under options.tui rather than as top-level options.
func optionUI(options map[string]any, args []string, stderr io.Writer) error {
	if len(args) < 4 {
		return usage(stderr, "usage: option ui <compact|diff|transparent|scrollbar|spinner|completions-max-depth|completions-max-items|keybinding> <value>")
	}

	key := args[2]
	ui := childMap(options, "tui")

	// keybinding is variadic (one or more keys) and doesn't fit the
	// single-value optionSpec shape, so it stays a bespoke case.
	if key == "keybinding" {
		value := args[3]
		if len(args) < 5 {
			return usage(stderr, "usage: option ui keybinding <action> <key> [key ...]")
		}
		keys := make([]any, 0, len(args)-4)
		for _, k := range args[4:] {
			if k == "" {
				return usage(stderr, "option ui keybinding keys must not be empty")
			}
			keys = append(keys, k)
		}
		childMap(ui, "keybindings")[value] = keys
		slog.Info("UI keybinding set in shell config", "action", value, "keys", args[4:])
		return nil
	}
	if len(args) != 4 {
		return usage(stderr, "usage: option ui <compact|diff|transparent|scrollbar|spinner|completions-max-depth|completions-max-items> <value>")
	}

	spec, ok := uiOptionSpecs[key]
	if !ok {
		return usage(stderr, fmt.Sprintf("option ui: unknown key %q", key))
	}

	return setOption(ui, spec, "option ui "+key, args[3], false, stderr)
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean %q", s)
	}
}
