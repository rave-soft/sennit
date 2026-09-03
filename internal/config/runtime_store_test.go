package config

import "testing"

// removeOnlyRuntimeStore implements only RemoveRuntimeConfigField, the one
// RuntimeStore method providerload (its only production caller) actually
// uses — see internal/providerload/loader.go's RemoveRuntimeConfigField
// call.
type removeOnlyRuntimeStore struct{}

func (removeOnlyRuntimeStore) RemoveRuntimeConfigField(Scope, string) {}

// TestRuntimeStore_DoesNotRequireWriteRuntimeConfigFields pins D3: before
// the fix, RuntimeStore also declared WriteRuntimeConfigFields, which had
// no caller outside tests (providerload only ever removes fields) — a
// mechanism kept alive by its own doc comment rather than by any real use.
// This line fails to compile if that method is put back on the interface,
// since removeOnlyRuntimeStore deliberately doesn't implement it.
func TestRuntimeStore_DoesNotRequireWriteRuntimeConfigFields(t *testing.T) {
	t.Parallel()

	var _ RuntimeStore = removeOnlyRuntimeStore{}
}
