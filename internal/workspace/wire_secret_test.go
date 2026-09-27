// Package workspace: the secret-sentinel half of CLIENT-SERVER.md PR 0.5's
// "Уточнено 2026-09-26" gate. Where wire_dto_test.go proves every DTO on the
// Workspace boundary can survive JSON, this file proves the specific DTO
// Config() now returns (FrontendConfig) actually drops what it is supposed
// to: it fills a real *config.Config until every string it can reach - by
// reflection, so a field added to config later is covered automatically -
// carries a marker naming its own field path, builds a FrontendConfig from
// it, and requires every marker that survived into the JSON to be one this
// file explicitly allow-lists as safe to reach the UI.
package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	providerconfig "github.com/rave-soft/sennit/internal/providers/config"
	providerstate "github.com/rave-soft/sennit/internal/providers/state"
	"github.com/stretchr/testify/require"
)

// sentinelStringOverrides forces one exact field path to a fixed value
// instead of the generic "SENTINEL-<path>" pattern - currently only
// ProviderConfig.ProxyURL, whose password has to be provably stripped
// (RedactProxyURL, frontend_config.go) rather than merely renamed: a
// sentinel that still looks like a URL with a userinfo password is what
// actually exercises that code path.
var sentinelStringOverrides = map[string]string{
	"Config.Providers[mock].ProxyURL": "http://user:SENTINEL-pw@host:8080",
}

// sentinelFill sets every string field, map value, and slice element
// reachable from v (an addressable, settable value) to "SENTINEL-<path>"
// (or sentinelStringOverrides[path]), allocating nil pointers/slices/maps
// as needed so the walk actually reaches them. It leaves non-string
// leaves (bool, numeric, map[string]any's interface values) untouched -
// they carry no string secret to catch, and config.Config's own map[string]any
// fields (LSP InitOptions/Options) are already the wire walk's trusted
// JSON-native passthrough (see mapAnyFieldAllowList in wire_dto_test.go).
func sentinelFill(v reflect.Value, path string) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			if !v.CanSet() {
				return
			}
			v.Set(reflect.New(v.Type().Elem()))
		}
		sentinelFill(v.Elem(), path)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			return
		}
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			sentinelFill(v.Field(i), path+"."+f.Name)
		}
	case reflect.Slice:
		if v.Len() == 0 {
			if !v.CanSet() {
				return
			}
			elem := reflect.New(v.Type().Elem()).Elem()
			sentinelFill(elem, fmt.Sprintf("%s[0]", path))
			v.Set(reflect.Append(v, elem))
			return
		}
		for i := range v.Len() {
			sentinelFill(v.Index(i), fmt.Sprintf("%s[%d]", path, i))
		}
	case reflect.Map:
		if v.IsNil() {
			if !v.CanSet() {
				return
			}
			v.Set(reflect.MakeMap(v.Type()))
		}
		if v.Len() == 0 {
			key := reflect.Zero(v.Type().Key())
			if key.Kind() == reflect.String {
				key = reflect.ValueOf("sentinel-key").Convert(v.Type().Key())
			}
			val := reflect.New(v.Type().Elem()).Elem()
			sentinelFill(val, path+"[]")
			v.SetMapIndex(key, val)
			return
		}
		for _, k := range v.MapKeys() {
			// A map value from MapIndex is not addressable/settable, so
			// mutate a settable copy and write it back.
			nv := reflect.New(v.Type().Elem()).Elem()
			nv.Set(v.MapIndex(k))
			sentinelFill(nv, fmt.Sprintf("%s[%v]", path, k.Interface()))
			v.SetMapIndex(k, nv)
		}
	case reflect.String:
		if !v.CanSet() {
			return
		}
		if override, ok := sentinelStringOverrides[path]; ok {
			v.SetString(override)
			return
		}
		v.SetString("SENTINEL-" + path)
	default:
		// bool/int/float/interface/etc: not a string secret vector.
	}
}

// sentinelConfig builds a real *config.Config through the same
// throwaway-global-config pattern AGENTS.md documents ("Testing without
// real providers"), then fills every reachable string with a sentinel.
// Providers and RuntimeProviders are *csync.Map - reflection cannot set
// through their unexported internals, so each is rebuilt by hand through
// its typed API, sentinel-filling the entries first.
func sentinelConfig(t *testing.T) *config.Config {
	t.Helper()

	globalDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", filepath.Join(globalDir, "data"))
	seed := `{
	  "options": {"disable_default_providers": true},
	  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
	    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
	    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192}]}},
	  "models": {"large": {"provider": "mock", "model": "mock-model"},
	             "small": {"provider": "mock", "model": "mock-model"}}
	}`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "sennit.json"), []byte(seed), 0o644))

	workingDir := t.TempDir()
	store, err := config.LoadData(workingDir, "", false)
	require.NoError(t, err)

	cfg := *store.Config() // shallow copy: safe to repoint fields on, see below.

	// Providers: sentinel-fill the "mock" entry LoadData already built,
	// through Set rather than through the frozen live map.
	newProviders := csync.NewMap[string, providerconfig.ProviderConfig]()
	for id, pc := range cfg.Providers.Seq2() {
		v := reflect.New(reflect.TypeFor[providerconfig.ProviderConfig]()).Elem()
		v.Set(reflect.ValueOf(pc))
		sentinelFill(v, fmt.Sprintf("Config.Providers[%s]", id))
		newProviders.Set(id, v.Interface().(providerconfig.ProviderConfig))
	}
	cfg.Providers = newProviders

	// RuntimeProviders: LoadData never resolves this (that needs a real
	// credential/account pass), so build one entry from scratch, keyed to
	// the same provider id so FrontendProvider.Auth resolves against it.
	rp := reflect.New(reflect.TypeFor[providerstate.Provider]()).Elem()
	sentinelFill(rp, "Config.RuntimeProviders[mock]")
	newRuntime := csync.NewMap[string, providerstate.Provider]()
	newRuntime.Set("mock", rp.Interface().(providerstate.Provider))
	cfg.RuntimeProviders = newRuntime

	// Give Agents a user-defined entry (Prompt included) alongside
	// whatever setupAgents already put there, so the test proves Prompt
	// specifically does not leak, not merely that it was never present.
	if cfg.Agents == nil {
		cfg.Agents = map[string]config.Agent{}
	}
	agent := config.Agent{ID: "reviewer"}
	av := reflect.ValueOf(&agent).Elem()
	sentinelFill(av, "Config.Agents[reviewer]")
	cfg.Agents["reviewer"] = agent

	// Give MCP and LSP one entry each; LoadData starts both empty.
	if cfg.MCP == nil {
		cfg.MCP = config.MCPs{}
	}
	cfg.MCP["myserver"] = config.MCPConfig{}
	mcpv := reflect.New(reflect.TypeFor[config.MCPConfig]()).Elem()
	mcpv.Set(reflect.ValueOf(cfg.MCP["myserver"]))
	sentinelFill(mcpv, "Config.MCP[myserver]")
	cfg.MCP["myserver"] = mcpv.Interface().(config.MCPConfig)

	if cfg.LSP == nil {
		cfg.LSP = config.LSPs{}
	}
	cfg.LSP["gopls"] = config.LSPConfig{}
	lspv := reflect.New(reflect.TypeFor[config.LSPConfig]()).Elem()
	lspv.Set(reflect.ValueOf(cfg.LSP["gopls"]))
	sentinelFill(lspv, "Config.LSP[gopls]")
	cfg.LSP["gopls"] = lspv.Interface().(config.LSPConfig)

	// Everything else reachable off cfg - Model, RecentModels, Options
	// (including WebSearch), Permissions, Tools, Hooks, Env - by the
	// generic walk. Providers/RuntimeProviders are csync.Map: an
	// exported-field walk over them finds nothing (every field is
	// unexported) and is a no-op, so re-running it over the whole struct
	// after the manual rebuild above is safe.
	cv := reflect.ValueOf(&cfg).Elem()
	for i := range cv.NumField() {
		f := cv.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		sentinelFill(cv.Field(i), "Config."+f.Name)
	}

	return &cfg
}

// sentinelTokenPattern matches one "SENTINEL-<path>" token as sentinelFill
// writes it, stopping at the closing JSON quote or any JSON structural
// character a path segment never contains.
var sentinelTokenPattern = regexp.MustCompile(`SENTINEL-[^"\\]*`)

// wireSentinelAllowList names every field path allowed to still carry its
// sentinel once FrontendConfig has been marshaled - i.e. every place
// NewFrontendConfig deliberately forwards a config value to the UI - each
// with a one-line reason. A path reachable through more than one JSON
// route (e.g. a provider's Models, forwarded verbatim) needs only one
// entry: the check is against the sentinel value, not the number of times
// it appears.
var wireSentinelAllowList = map[string]string{
	"SENTINEL-Config.Providers[mock].ID":                               "provider id - not a secret, the UI needs it to address the provider",
	"SENTINEL-Config.Providers[mock].Name":                             "provider display name",
	"SENTINEL-Config.Providers[mock].BaseURL":                          "provider endpoint URL, not a credential",
	"SENTINEL-Config.Providers[mock].Type":                             "provider type enum (openai/anthropic/...), not a credential",
	"SENTINEL-Config.Providers[mock].Models[0].ID":                     "catalog model id",
	"SENTINEL-Config.Providers[mock].Models[0].Name":                   "catalog model display name",
	"SENTINEL-Config.Providers[mock].Models[0].DefaultReasoningEffort": "catalog model metadata",
	"SENTINEL-Config.Providers[mock].Models[0].ReasoningLevels[0]":     "catalog model metadata",
	"SENTINEL-Config.Providers[mock].Rotation.Cooldown":                "rotation policy, not a credential",
	"SENTINEL-Config.Providers[mock].Rotation.Order[0]":                "rotation policy (account id ordering)",
	"SENTINEL-Config.RuntimeProviders[mock].Account":                   "active account id - ProviderAuth.Account, not a credential",
	"SENTINEL-Config.Model.Provider":                                   "the selected provider id",
	"SENTINEL-Config.Model.Model":                                      "the selected model id",
	"SENTINEL-Config.Model.ReasoningEffort":                            "the selected model's reasoning effort",
	"SENTINEL-Config.RecentModels[0].Provider":                         "a recent model entry's provider id",
	"SENTINEL-Config.RecentModels[0].Model":                            "a recent model entry's model id",
	"SENTINEL-Config.RecentModels[0].ReasoningEffort":                  "a recent model entry's reasoning effort",
	"SENTINEL-Config.Agents[reviewer].Model":                           "FrontendAgent's model override",
	"SENTINEL-Config.Agents[reviewer].ReasoningEffort":                 "FrontendAgent's reasoning-effort override",
	"SENTINEL-Config.Options.InitializeAs":                             "FrontendConfig.InitializeAs",
	"SENTINEL-Config.Options.DisabledSkills[0]":                        "FrontendConfig.DisabledSkills",
}

// TestFrontendConfigSecretSentinel is the "Тест секретов" from
// CLIENT-SERVER.md's PR 0.5 note: every sentinel that reaches the
// marshaled FrontendConfig JSON must be on wireSentinelAllowList, and the
// ProxyURL password sentinel must never appear at all.
func TestFrontendConfigSecretSentinel(t *testing.T) {
	cfg := sentinelConfig(t)

	fc := NewFrontendConfig(cfg, nil)
	data, err := json.Marshal(fc)
	require.NoError(t, err)

	require.NotContains(t, string(data), "SENTINEL-pw",
		"provider ProxyURL's password must be stripped before it reaches the UI (see RedactProxyURL)")

	for _, tok := range sentinelTokenPattern.FindAllString(string(data), -1) {
		if reason, ok := wireSentinelAllowList[tok]; ok {
			_ = reason
			continue
		}
		t.Errorf("FrontendConfig JSON leaks %s, not on wireSentinelAllowList - a secret or hidden field reached the UI; add it to wireSentinelAllowList with a reason only if it is genuinely safe to expose", tok)
	}
}
