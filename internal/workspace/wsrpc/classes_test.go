package wsrpc

import (
	"reflect"
	"testing"

	"github.com/rave-soft/sennit/internal/workspace"
)

// TestMethodClassificationIsComplete fails, loudly, the moment
// workspace.Workspace grows a method that MethodClasses does not classify,
// or MethodClasses names a method Workspace no longer has. Moved from
// internal/workspace/wire_classes_test.go (a _test.go-only table that
// neither this package's generator nor internal/ui/model could import) —
// see classes.go's own doc comment for why the table now lives here in
// production code instead.
func TestMethodClassificationIsComplete(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeOf((*workspace.Workspace)(nil)).Elem()
	all := make(map[string]bool, typ.NumMethod())
	for i := range typ.NumMethod() {
		all[typ.Method(i).Name] = true
	}

	for name := range all {
		if _, ok := MethodClasses[name]; !ok {
			t.Errorf("workspace.Workspace.%s has no entry in wsrpc.MethodClasses (classes.go); "+
				"classify it as U, C, S, H, or X per CLIENT-SERVER.md's method-class table", name)
		}
	}
	for name, class := range MethodClasses {
		if !all[name] {
			t.Errorf("MethodClasses names %s (class %s) but workspace.Workspace has no such method any more; remove the stale entry", name, class)
		}
	}
}
