package proto_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/hooks"
	"github.com/rave-soft/sennit/internal/proto"
)

// TestHookInfoParityWithDomain holds proto's copy of a hook's per-hook
// result against the runner's own. The two cannot share a definition —
// proto is the transport boundary and deliberately does not depend on
// internal/hooks, which is what lets the chat renderer decode the metadata
// without importing the hook runner — so the copy is the design, and this
// keeps it honest.
//
// A field added to hooks.HookInfo and forgotten here is not a compile
// error: internal/agent converts field by field, so the new one is simply
// never written, and the transcript quietly stops showing something the
// runner knows. The reverse direction matters too, which is why the
// comparison is by set and not by containment.
func TestHookInfoParityWithDomain(t *testing.T) {
	t.Parallel()

	require.Equal(t, fieldNames(reflect.TypeOf(hooks.HookInfo{})), fieldNames(reflect.TypeOf(proto.HookInfo{})),
		"proto.HookInfo and hooks.HookInfo must carry the same fields; internal/agent copies between them one field at a time, so a new field on either side is dropped silently")
}

func fieldNames(t reflect.Type) []string {
	names := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		names = append(names, t.Field(i).Name)
	}
	return names
}
