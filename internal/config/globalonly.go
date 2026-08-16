package config

import (
	"fmt"
	"path/filepath"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// globalOnlyKeys are the top-level config keys Sennit reads exclusively from
// the global config layers (/etc/sennit/sennit.json, ~/.config/sennit/
// sennit.json, the global sennitrc, and the machine data config). Which
// providers exist, which credentials they use, and which model is selected
// are properties of the machine and the person using it — not of a checked-out
// repository. A project that shipped its own `providers` or `model` block
// would otherwise silently repoint a colleague's session at another endpoint,
// or switch the model out from under them, just by being cloned.
//
// Keys listed here are stripped from every project-scoped layer before the
// merge, and each removal is surfaced as a doctor Problem rather than being
// dropped silently — see dropGlobalOnlyKeys.
var globalOnlyKeys = []string{"model", "recent_models", "providers"}

// globalOnlyKeyArea maps a global-only key onto the doctor Area it belongs
// to, so `sennit doctor` files the warning under the subsystem the user was
// trying to configure.
var globalOnlyKeyArea = map[string]Area{
	"model":         AreaModel,
	"recent_models": AreaModel,
	"providers":     AreaProvider,
}

// globalConfigPaths lists the config layers that are allowed to define the
// global-only keys. It is also the prefix of the path list lookupConfigs
// builds, which is what keeps the two definitions of "global" in sync.
func globalConfigPaths() []string {
	return []string{
		systemConfigPath,
		GlobalConfig(),
		shellConfigSibling(GlobalConfig()),
		GlobalConfigData(),
	}
}

// isGlobalConfigPath reports whether path is one of the global config layers.
// Anything else — a project sennit.json/sennitrc found by the upward walk, or
// the workspace config in .sennit/ — is project-scoped.
func isGlobalConfigPath(path string) bool {
	clean := filepath.Clean(path)
	for _, p := range globalConfigPaths() {
		if filepath.Clean(p) == clean {
			return true
		}
	}
	return false
}

// dropGlobalOnlyKeys removes the global-only top-level keys from a
// project-scoped config layer, returning the remaining JSON plus one Problem
// per key it dropped. Callers pass the result to loadFromBytes, so the merge
// never sees provider or model settings coming from a project.
func dropGlobalOnlyKeys(data []byte, path string) ([]byte, []Problem) {
	var problems []Problem
	for _, key := range globalOnlyKeys {
		if !gjson.GetBytes(data, key).Exists() {
			continue
		}
		out, err := sjson.DeleteBytes(data, key)
		if err != nil {
			continue
		}
		data = out
		problems = append(problems, Problem{
			Severity: SeverityWarn,
			Area:     globalOnlyKeyArea[key],
			Subject:  path,
			Message: fmt.Sprintf(
				"%q is set in a project config and was ignored: provider and model settings are global-only",
				key,
			),
			Hint: fmt.Sprintf("Move it to %s, or set it from the TUI — Sennit always writes those settings globally.", GlobalConfig()),
		})
	}
	return data, problems
}
