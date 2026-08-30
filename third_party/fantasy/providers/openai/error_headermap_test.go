package openai

import (
	"net/http"
	"testing"
)

// toHeaderMap must produce a lowercase-keyed map deterministically.
// Ranging over an http.Header (map[string][]string) and writing a
// lowercase alias back into the same map being ranged is undefined by the
// Go spec, so the map is populated in a single pass instead. Kept in a file
// of its own, not in upstream's error_test.go, so a subtree pull never
// conflicts on it.
func TestToHeaderMap_LowercasesKeysDeterministically(t *testing.T) {
	t.Parallel()

	h := http.Header{}
	h.Set("Retry-After", "30")
	h.Set("Retry-After-Ms", "1500")

	got := toHeaderMap(h)

	if got["retry-after"] != "30" {
		t.Errorf("toHeaderMap()[\"retry-after\"] = %q, want \"30\"", got["retry-after"])
	}
	if got["retry-after-ms"] != "1500" {
		t.Errorf("toHeaderMap()[\"retry-after-ms\"] = %q, want \"1500\"", got["retry-after-ms"])
	}
}
