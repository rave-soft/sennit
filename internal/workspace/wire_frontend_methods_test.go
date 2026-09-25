// Package workspace: the method-equivalence half of CLIENT-SERVER.md PR 0.5's
// "Уточнено 2026-09-26" gate. FrontendConfig exists so the UI can keep
// calling the same methods it called on *config.Config; this file proves
// that promise across the JSON boundary a remote client will actually see:
// every exported method, called on a value built by hand and on the same
// value after a Marshal/Unmarshal round trip, must return the same thing.
package workspace

import (
	"encoding/json"
	"reflect"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

// checkMethodsSurviveRoundTrip requires an entry in argsByMethod (the
// arguments to call a method with) or skippedMethods (a reason not to, for
// a method a generic reflect.Value comparison can't handle) for every
// exported method sample's type exports, then calls each on sample and on
// sample's JSON round trip and requires the results equal. sample must be
// a pointer so the method set includes both value- and pointer-receiver
// methods.
func checkMethodsSurviveRoundTrip(t *testing.T, sample any, argsByMethod map[string][]any, skippedMethods map[string]string) {
	t.Helper()

	data, err := json.Marshal(sample)
	require.NoError(t, err)

	rt := reflect.TypeOf(sample)
	decoded := reflect.New(rt.Elem())
	require.NoError(t, json.Unmarshal(data, decoded.Interface()))

	for i := range rt.NumMethod() {
		m := rt.Method(i)
		if reason, ok := skippedMethods[m.Name]; ok {
			_ = reason
			continue
		}
		args, ok := argsByMethod[m.Name]
		if !ok {
			t.Errorf("%s.%s has no entry in its method-args table; add one (or a skip-list entry with a reason) so this gate exercises it", rt.String(), m.Name)
			continue
		}

		call := func(recv reflect.Value) []reflect.Value {
			in := make([]reflect.Value, 0, len(args)+1)
			in = append(in, recv)
			for _, a := range args {
				in = append(in, reflect.ValueOf(a))
			}
			return m.Func.Call(in)
		}

		want := call(reflect.ValueOf(sample))
		got := call(decoded)
		require.Len(t, got, len(want), "%s.%s: result count mismatch", rt.String(), m.Name)
		for i := range want {
			require.Equal(t, want[i].Interface(), got[i].Interface(), "%s.%s result %d differs after a JSON round trip", rt.String(), m.Name, i)
		}
	}
}

// frontendConfigMethodArgs supplies the arguments to call each exported
// *FrontendConfig method with. sampleFrontendConfig (wire_dto_samples_test.go)
// is built so every one of these calls has something real to find.
var frontendConfigMethodArgs = map[string][]any{
	"GetModel":                  {"openai", "gpt-5"},
	"ProviderName":              {"openai"},
	"SelectedCatalogModel":      {},
	"GetProviderForModel":       {},
	"RememberedReasoningEffort": {"openai", "gpt-5"},
	"DefaultModelForProvider":   {"openai", []catwalk.Provider{sampleCatwalkProvider}},
	"IsConfigured":              {},
	"AgentOverride":             {"reviewer"},
	"HasCoderAgent":             {},
	"MCPServerNames":            {},
	"Provider":                  {"openai"},
	"IsDockerMCPEnabled":        {},
	"ProviderAuth":              {"openai"},
}

// frontendConfigMethodsSkipped names *FrontendConfig methods verified by a
// dedicated test instead of the generic table above, with why.
var frontendConfigMethodsSkipped = map[string]string{
	"ProvidersSeq": "returns a func value - require.Equal can't compare those; drained and compared by TestFrontendConfigProvidersSeqSurvivesRoundTrip instead",
}

func TestFrontendConfigMethodsSurviveRoundTrip(t *testing.T) {
	t.Parallel()
	checkMethodsSurviveRoundTrip(t, &sampleFrontendConfig, frontendConfigMethodArgs, frontendConfigMethodsSkipped)
}

// drainProvidersSeq collects c.ProvidersSeq() into a plain map so two
// FrontendConfig values' iterators can be compared with require.Equal.
func drainProvidersSeq(c *FrontendConfig) map[string]FrontendProvider {
	out := make(map[string]FrontendProvider, len(c.Providers))
	for id, p := range c.ProvidersSeq() {
		out[id] = p
	}
	return out
}

func TestFrontendConfigProvidersSeqSurvivesRoundTrip(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(&sampleFrontendConfig)
	require.NoError(t, err)
	var decoded FrontendConfig
	require.NoError(t, json.Unmarshal(data, &decoded))

	require.Equal(t, drainProvidersSeq(&sampleFrontendConfig), drainProvidersSeq(&decoded))
}

// FrontendProvider has no methods of its own today (ProvidersSeq above
// belongs to FrontendConfig); this still runs the generic check so a
// method added to FrontendProvider later is caught by the same gate.
func TestFrontendProviderMethodsSurviveRoundTrip(t *testing.T) {
	t.Parallel()
	checkMethodsSurviveRoundTrip(t, &sampleFrontendProvider, map[string][]any{}, nil)
}

// ProviderAuth.Expired takes a fixed "now" so its own result is
// deterministic and comparable across the two receivers.
func TestProviderAuthMethodsSurviveRoundTrip(t *testing.T) {
	t.Parallel()
	checkMethodsSurviveRoundTrip(t, &sampleProviderAuth, map[string][]any{
		"Expired": {int64(1234567890)},
	}, nil)
}
