package threadspawn

import (
	"reflect"
	"testing"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/thread"
)

// fieldsExcludedFromDTOs lists thread.Thread fields that none of the
// three hand-written flattenings (proto.Thread in
// internal/workspace/appws/protoconv.go, tools.ThreadInfo in
// agenttool.go, tools.TaskInfo in tasktool.go) carry, and why: they are
// outbox bookkeeping for delivering a delegation's terminal event exactly
// once (see thread.Delegation), not state any caller of these DTOs needs.
var fieldsExcludedFromDTOs = map[string]bool{
	"CompletionPending": true,
	"CompletionDepth":   true,
	"TerminalAt":        true,
}

// TestThreadFieldsCoveredByDTOs guards the three hand-written flattenings
// of thread.Thread against silent drift: a new thread.Thread field that
// lands in none of them, and isn't in fieldsExcludedFromDTOs above, fails
// this test instead of quietly going missing from whichever DTO the next
// person forgot to update.
func TestThreadFieldsCoveredByDTOs(t *testing.T) {
	fieldNames := func(v any) map[string]bool {
		names := map[string]bool{}
		for _, f := range reflect.VisibleFields(reflect.TypeOf(v)) {
			names[f.Name] = true
		}
		return names
	}
	covered := fieldNames(proto.Thread{})
	for name := range fieldNames(tools.ThreadInfo{}) {
		covered[name] = true
	}
	for name := range fieldNames(tools.TaskInfo{}) {
		covered[name] = true
	}

	for _, f := range reflect.VisibleFields(reflect.TypeOf(thread.Thread{})) {
		// Skip the promoted Delegation field itself; VisibleFields also
		// lists it alongside the fields it promotes, and it isn't a leaf
		// value any DTO carries on its own.
		if f.Anonymous {
			continue
		}
		if fieldsExcludedFromDTOs[f.Name] {
			continue
		}
		if !covered[f.Name] {
			t.Errorf("thread.Thread field %q is not carried by proto.Thread, tools.ThreadInfo, or tools.TaskInfo; add it to whichever DTO needs it, or to fieldsExcludedFromDTOs with a reason", f.Name)
		}
	}
}
