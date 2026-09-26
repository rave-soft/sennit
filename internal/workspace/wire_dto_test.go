// Package workspace: this file is the PR 0.3 gate from CLIENT-SERVER.md -
// "Тест полноты DTO". Its job is to make it impossible to add a type to
// the Workspace boundary that does not survive encoding/json, since that
// boundary will eventually be served over gRPC with a JSON codec.
//
// It has three parts:
//   - collectWireTypes walks every U/C method's parameters and results
//     (plus the event payloads AppWorkspace.Subscribe delivers, and the
//     data types the S/H methods carry) and returns the set of named
//     struct/opaque types reachable from them.
//   - the walk itself fails on any forbidden shape (interface, func,
//     chan, unsafe.Pointer, a map with a non-string/non-integer key)
//     it finds along the way, unless the exact field path is allow-listed
//     below with a reason.
//   - wireSamples supplies one fully-populated value per collected type;
//     TestWireTypesRoundTripJSON checks samples exist, are not left at
//     their zero value on any exported field, and round-trip through
//     json.Marshal/Unmarshal unchanged.
package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	providerconfig "github.com/rave-soft/sennit/internal/providers/config"
	providerstate "github.com/rave-soft/sennit/internal/providers/state"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/stretchr/testify/require"
)

// eventMessage etc. name the 12 event payload types by their real type,
// only under package-local aliases so the field-allow-list keys below
// (which are computed from reflect.Type, i.e. from the real package path)
// still refer to message.Message, permission.PermissionRequest, and so on.
type (
	eventMessage                = message.Message
	eventSession                = session.Session
	eventThread                 = proto.Thread
	eventPermissionRequest      = permission.PermissionRequest
	eventPermissionNotification = permission.PermissionNotification
	eventQuestionRequest        = question.Request
	eventQuestionNotification   = question.Notification
	eventHistoryFile            = history.File
	eventSkills                 = skills.Event
	eventAgentNotification      = AgentNotification
	eventLSP                    = LSPEvent
	eventMCP                    = MCPEvent
	eventConnection             = ConnectionEvent
)

// -- Event payload types --------------------------------------------------
//
// AppWorkspace.Subscribe (internal/workspace/appws/app_workspace_lifecycle.go,
// translateEvent) turns every event this workspace publishes into one of
// 12 payload types before handing it to the UI - see CLIENT-SERVER.md's
// "Что уже готово" note and internal/ui/model's pubsub.Event[...] switch
// arms (update_prompts.go, update_session.go, update_status.go,
// update_threads.go, update_integrations.go, root.go), which is the
// second, independent place this same list is pinned. This package cannot
// import internal/workspace/appws (appws imports workspace; importing it
// back would cycle) or internal/ui (the whole point of this boundary), so
// the list is kept explicit here instead of discovered by reflection.
//
// ConnectionEvent is the 13th entry and the one exception: it has no
// translateEvent producer at all (see its own doc comment) -- it is
// synthesized client-side by grpcws's Subscribe/SubscribeWith reconnect
// loop (CLIENT-SERVER.md, PR 1.2/1.4) -- but travels the same event
// registry, so it belongs in this gate too.
//
// message.Message and permission.PermissionRequest carry their own
// MarshalJSON/UnmarshalJSON (see message/json.go, permission/permission.go)
// so the walk treats them as opaque; question.Request, question.Notification,
// history.File, skills.Event, session.Session, proto.Thread,
// permission.PermissionNotification and the four workspace.* event
// structs are plain data and get walked normally.
var eventPayloadTypes = []reflect.Type{
	reflectTypeOf[eventMessage](),
	reflectTypeOf[eventSession](),
	reflectTypeOf[eventThread](),
	reflectTypeOf[eventPermissionRequest](),
	reflectTypeOf[eventPermissionNotification](),
	reflectTypeOf[eventQuestionRequest](),
	reflectTypeOf[eventQuestionNotification](),
	reflectTypeOf[eventHistoryFile](),
	reflectTypeOf[eventSkills](),
	reflectTypeOf[eventAgentNotification](),
	reflectTypeOf[eventLSP](),
	reflectTypeOf[eventMCP](),
	reflectTypeOf[eventConnection](),
}

// -- S/H data types ---------------------------------------------------------
//
// Stream (S) and handle (H) methods are written by hand in later PRs, so
// their own signatures are not walked here - but the plain data they will
// carry across the wire is exactly the kind of type this test exists to
// guard, so it is added explicitly. AgentRunEvent/AgentRunOptions and
// proto.ShellCommandResponse are what AgentRunStream/AgentRunShellCommand
// carry; OAuthStartResult/OAuthCompletion are what StartOAuth's handle
// resolves to; proto.Thread is what AttachThread/EnterWorktree/ExitWorktree
// ultimately report about the workspace they hand back (it is already
// collected via ThreadController's U methods and the thread event above,
// listed again here only for documentation - see extraHandleAndStreamTypes).
var extraHandleAndStreamTypes = []reflect.Type{
	reflectTypeOf[AgentRunEvent](),
	reflectTypeOf[AgentRunOptions](),
	reflectTypeOf[OAuthStartResult](),
	reflectTypeOf[OAuthCompletion](),
}

func reflectTypeOf[T any]() reflect.Type {
	return reflect.TypeOf((*T)(nil)).Elem()
}

var (
	ctxType   = reflect.TypeOf((*context.Context)(nil)).Elem()
	errType   = reflect.TypeOf((*error)(nil)).Elem()
	marshaler = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	unmarshal = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
)

// isOpaque reports whether t (or *t) implements both MarshalJSON and
// UnmarshalJSON, in which case the walk trusts its own codec and samples
// it as a leaf rather than recursing into its fields. time.Time falls out
// of this automatically.
func isOpaque(t reflect.Type) bool {
	if t.Implements(marshaler) && t.Implements(unmarshal) {
		return true
	}
	pt := reflect.PointerTo(t)
	return pt.Implements(marshaler) && pt.Implements(unmarshal)
}

// fieldAllowList permits a specific field or element, named by
// "<PackagePath>.<TypeName>.<FieldName>" (or a synthetic name for a map
// element, see mapElemAllowKey), to hold a shape the walk would otherwise
// reject. Every entry needs a reason tied to a codec that exists - see
// each comment.
var fieldAllowList = map[string]string{
	// Message carries its own MarshalJSON/UnmarshalJSON (message/json.go)
	// that delegates Parts to MarshalParts/UnmarshalParts, which restore
	// all 8 concrete ContentPart implementations by a type tag - so the
	// interface field never reaches encoding/json directly. Message
	// itself is opaque (isOpaque true) so the walk never actually
	// descends into this field; listed for documentation, matching
	// CLIENT-SERVER.md PR 0.3's own allow-list example.
	"github.com/rave-soft/sennit/internal/message.Message.Parts": "message has its own codec (MarshalParts/UnmarshalParts)",
	// PermissionRequest.Params is decoded through the toolName->type
	// registry in proto.DecodePermissionParams (permission/permission.go
	// UnmarshalJSON), not by encoding/json directly. PermissionRequest is
	// itself opaque (isOpaque true) so this field is likewise never
	// reached by the walk; listed for documentation.
	"github.com/rave-soft/sennit/internal/permission.PermissionRequest.Params": "registry decode (proto.DecodePermissionParams)",
	// SetConfigField's value is written straight into the config file's
	// JSON tree at a dotted-path key (ConfigStore.writeConfigFields /
	// sjson-style patch, internal/config/store.go), not decoded back into
	// a Go value the caller's type identity matters for - the far side
	// only ever needs value's JSON encoding, matching the config/options
	// map[string]any pattern below.
	"Workspace.SetConfigField.param2": "patched into the config file as raw JSON at a path key, never decoded back to a Go type",
}

// mapAnyFieldAllowList permits a bare map[string]any value, checked by
// field path so a future unrelated map[string]any field does not silently
// inherit the exemption. Every occurrence reachable from the wire today
// comes from decoded JSON config (LSP InitOptions/Options, catwalk's
// ModelOptions.ProviderOptions, config.SelectedModel.ProviderOptions), so
// its values are JSON-native by construction rather than arbitrary Go
// values a codec would have to guess at.
var mapAnyFieldAllowList = map[string]bool{
	"github.com/rave-soft/sennit/internal/config.LSPConfig.InitOptions":         true,
	"github.com/rave-soft/sennit/internal/config.LSPConfig.Options":             true,
	"charm.land/catwalk/pkg/catwalk.ModelOptions.ProviderOptions":               true,
	"github.com/rave-soft/sennit/internal/config.SelectedModel.ProviderOptions": true,
}

// -- Forbidden types -------------------------------------------------------
//
// These types carry secrets or unexported/json:"-" runtime state that must
// never reach a remote frontend (CLIENT-SERVER.md PR 0.5's "Уточнено
// 2026-09-26" note). *config.Config itself had a hole of exactly this
// shape until FrontendConfig replaced it as Config()'s return type
// (workspace.NewFrontendConfig): RuntimeProviders was json:"-", so the old
// completeness gate never saw it, even though four UI call sites read
// RuntimeProvider(id) through it. Banning these types by identity - not
// just by the shapes they currently reach through - means a future method
// that starts returning one of them again fails here immediately, instead
// of waiting for someone to notice a json:"-" field by hand.
var forbiddenWireTypes = map[reflect.Type]string{
	reflectTypeOf[config.Config]():                 "config.Config",
	reflectTypeOf[providerstate.Provider]():        "providerstate.Provider",
	reflectTypeOf[providerconfig.ProviderConfig](): "providerconfig.ProviderConfig",
	reflectTypeOf[oauth.Token]():                   "oauth.Token",
}

// csyncMapPkgPath is internal/csync's import path; forbiddenWireTypeName
// matches any csync.Map[K, V] instantiation by package and name prefix
// rather than listing every K/V combination by hand.
const csyncMapPkgPath = "github.com/rave-soft/sennit/internal/csync"

// forbiddenWireTypeName reports the human-readable name of t if it is one
// of forbiddenWireTypes, or any csync.Map instantiation.
func forbiddenWireTypeName(t reflect.Type) (string, bool) {
	if name, ok := forbiddenWireTypes[t]; ok {
		return name, true
	}
	if t.PkgPath() == csyncMapPkgPath && strings.HasPrefix(t.Name(), "Map[") {
		return "csync." + t.Name(), true
	}
	return "", false
}

// forbiddenTypeAllowList permits a forbidden type to be reached at one
// exact field/param/result path (the same "<Type>.<Field>" or
// "Workspace.<Method>.paramN"/"resultN" key fieldKey/ownerField produce),
// each entry recording why that specific value legitimately has to travel.
// Do not add an entry to relax the rule generally - only to document one
// path that must carry the forbidden type by design.
//
// The four OAuth entries this map used to carry (a sign-in token moving
// server -> UI -> server through StartOAuth/ImportCopilot and
// CompleteOAuth/RecordAccount) are gone: CLIENT-SERVER.md PR 1.3 moved
// sign-in completion behind OAuthFlow.Wait, so the token never leaves the
// server, RecordAccount's contract type (AccountCredential) carries no
// Token field, and ImportCopilot returns only a bool.
var forbiddenTypeAllowList = map[string]string{}

// -- No hidden state on types with behavior --------------------------------
//
// A type with an exported method invites a caller to call it on whatever
// value it has in hand - including one just JSON-decoded off the wire. If
// that type also has a field encoding/json cannot see (unexported, or
// json:"-"), the caller has no way to tell whether the method's result
// depends on state the wire drops; RuntimeProviders was exactly this shape
// before FrontendConfig existed. hiddenStateMethodExemptions excludes the
// handful of method names that are codec/stringer hooks, not application
// logic, from counting as "behavior" here.
var hiddenStateMethodExemptions = map[string]bool{
	"MarshalJSON": true, "UnmarshalJSON": true,
	"MarshalText": true, "UnmarshalText": true,
	"String": true, "Error": true,
}

// hasBehavior reports whether t exports any method - by value or pointer
// receiver - beyond hiddenStateMethodExemptions.
func hasBehavior(t reflect.Type) bool {
	exports := func(rt reflect.Type) bool {
		for i := range rt.NumMethod() {
			if !hiddenStateMethodExemptions[rt.Method(i).Name] {
				return true
			}
		}
		return false
	}
	return exports(t) || exports(reflect.PointerTo(t))
}

// hiddenStateFieldAllowList permits one exact "<PkgPath>.<Type>.<Field>"
// hidden field on a type that otherwise has behavior. Every entry must
// show that no method on the type reads the field - not merely that the
// current ones happen not to - since that is what makes it safe for a
// caller working off a JSON-decoded copy.
var hiddenStateFieldAllowList = map[string]string{
	// skills.Skill's one exported method, Validate (internal/skills/skills.go),
	// checks Name/Description/Path/Compatibility only - it never reads
	// Source, so a UI working off a JSON-decoded Skill (Source stripped by
	// its own json:"-") gets the same Validate result a full, in-process
	// Skill would. Source exists solely to hand a skill's text to another
	// in-process workspace (thread inheritance, see Skill's own doc
	// comment); it was never meant to reach the wire.
	"github.com/rave-soft/sennit/internal/skills.Skill.Source": "Skill.Validate does not read Source; Source is for in-process thread handoff, never the wire",
}

// checkNoHiddenStateWithBehavior fails t if typ has behavior (hasBehavior)
// and also carries an unexported or json:"-" field not covered by
// hiddenStateFieldAllowList. Opaque types are skipped: their own codec,
// not their Go layout, is what wireRoundTripJSON already holds accountable
// for what crosses the wire.
func checkNoHiddenStateWithBehavior(t *testing.T, typ reflect.Type) {
	if isOpaque(typ) || !hasBehavior(typ) {
		return
	}
	for i := range typ.NumField() {
		f := typ.Field(i)
		key := fieldKey(typ, f.Name)
		if !f.IsExported() {
			if _, ok := hiddenStateFieldAllowList[key]; ok {
				continue
			}
			t.Errorf("wire type %s has behavior and an unexported field %q: a caller cannot tell whether a method's result depends on state a JSON round trip drops (add %q to hiddenStateFieldAllowList with proof no method reads it, or remove the field/method)", typeKey(typ), f.Name, key)
			continue
		}
		if f.Tag.Get("json") == "-" {
			if _, ok := hiddenStateFieldAllowList[key]; ok {
				continue
			}
			t.Errorf("wire type %s has behavior and a json:\"-\" field %q: a caller cannot tell whether a method's result depends on state a JSON round trip drops (add %q to hiddenStateFieldAllowList with proof no method reads it, or remove the field/method)", typeKey(typ), f.Name, key)
		}
	}
}

// wireWalker collects every named struct/opaque type reachable from the
// method set below, and fails the test on any forbidden shape found along
// the way.
type wireWalker struct {
	t        *testing.T
	types    map[reflect.Type]bool
	visiting map[reflect.Type]bool
}

func newWireWalker(t *testing.T) *wireWalker {
	return &wireWalker{t: t, types: map[reflect.Type]bool{}, visiting: map[reflect.Type]bool{}}
}

func fieldKey(t reflect.Type, fieldName string) string {
	name := t.Name()
	if t.PkgPath() != "" {
		name = t.PkgPath() + "." + name
	}
	return name + "." + fieldName
}

func typeKey(t reflect.Type) string {
	if t.PkgPath() != "" {
		return t.PkgPath() + "." + t.Name()
	}
	return t.String()
}

// walk descends into t, reached at path (a human-readable description for
// failure messages) via ownerField ("<Type>.<Field>" of the struct field
// that led here, "" at the top).
func (w *wireWalker) walk(t reflect.Type, path string, ownerField string) {
	switch t.Kind() {
	case reflect.Pointer:
		w.walk(t.Elem(), path, ownerField)
		return
	case reflect.Interface:
		if t.NumMethod() == 0 {
			// Bare `any`. Only permitted where fieldAllowList says so.
			if reason, ok := fieldAllowList[ownerField]; ok {
				_ = reason
				return
			}
		}
		w.t.Errorf("wire type %s: forbidden interface field (%s)", path, t.String())
		return
	case reflect.Func:
		w.t.Errorf("wire type %s: forbidden func field", path)
		return
	case reflect.Chan:
		w.t.Errorf("wire type %s: forbidden chan field", path)
		return
	case reflect.UnsafePointer:
		w.t.Errorf("wire type %s: forbidden unsafe.Pointer field", path)
		return
	case reflect.Slice, reflect.Array:
		w.walk(t.Elem(), path+"[]", ownerField)
		return
	case reflect.Map:
		switch t.Key().Kind() {
		case reflect.String,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		default:
			w.t.Errorf("wire type %s: map key %s is neither string nor integer", path, t.Key().String())
			return
		}
		elem := t.Elem()
		if elem.Kind() == reflect.Interface && elem.NumMethod() == 0 {
			if mapAnyFieldAllowList[ownerField] {
				return
			}
			w.t.Errorf("wire type %s: forbidden map[%s]any field (not in mapAnyFieldAllowList)", path, t.Key().String())
			return
		}
		w.walk(elem, path+"[]", ownerField)
		return
	case reflect.Struct:
		if name, forbidden := forbiddenWireTypeName(t); forbidden {
			if reason, ok := forbiddenTypeAllowList[ownerField]; ok {
				_ = reason
			} else {
				w.t.Errorf("wire type %s: forbidden type %s reachable via %q; add an entry to forbiddenTypeAllowList with a reason if it legitimately must travel, or fix the method/field that reaches it", path, name, ownerField)
				return
			}
		}
		// time.Time and every other type with its own MarshalJSON/
		// UnmarshalJSON is a leaf: trust its codec, don't walk fields.
		if isOpaque(t) {
			w.types[t] = true
			return
		}
		if w.visiting[t] {
			return // already walking this type higher up the stack (cycle guard)
		}
		w.visiting[t] = true
		defer delete(w.visiting, t)
		w.types[t] = true
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			w.walk(f.Type, path+"."+f.Name, fieldKey(t, f.Name))
		}
		return
	default:
		// Primitives (string, bool, numeric kinds) and defined types over
		// them (e.g. session.TodoStatus) round-trip through encoding/json
		// on their own; nothing to walk or sample.
		return
	}
}

// methodClasses reads wsrpc.MethodClasses's method->class ("U", "C", "S",
// "H", "X") assignments straight out of wsrpc/classes.go's source, without
// importing the wsrpc package: this file lives in package workspace's own
// internal test files, and wsrpc imports workspace, so importing it back
// here would be a cycle Go's toolchain refuses ("import cycle not allowed
// in test") -- see wire_classes_ui_guard_test.go (now package
// workspace_test) for the same problem solved the other way, by moving out
// to an external test package instead. Parsing is the only option left for
// a file that must stay in the internal package, since collectWireTypes
// below shares unexported helpers with the rest of this file's package.
func methodClasses() map[string]string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("runtime.Caller failed; cannot locate wsrpc/classes.go")
	}
	path := filepath.Join(filepath.Dir(thisFile), "wsrpc", "classes.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		panic(fmt.Sprintf("parsing %s: %v", path, err))
	}

	classes := make(map[string]string)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vspec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			nameMatches := false
			for _, ident := range vspec.Names {
				if ident.Name == "MethodClasses" {
					nameMatches = true
				}
			}
			if !nameMatches {
				continue
			}
			for _, value := range vspec.Values {
				lit, ok := value.(*ast.CompositeLit)
				if !ok {
					continue
				}
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.BasicLit)
					if !ok || key.Kind != token.STRING {
						continue
					}
					name, err := strconv.Unquote(key.Value)
					if err != nil {
						panic(err)
					}
					class, ok := kv.Value.(*ast.Ident)
					if !ok {
						continue
					}
					classes[name] = class.Name
				}
			}
		}
	}
	if len(classes) == 0 {
		panic(fmt.Sprintf("found no entries in wsrpc.MethodClasses in %s; parsing must have failed silently", path))
	}
	return classes
}

// collectWireTypes returns the set of every named struct/opaque type
// reachable from Workspace's U/C methods, the event payload types, and
// the extra S/H data types, failing t on any forbidden shape found.
func collectWireTypes(t *testing.T) map[reflect.Type]bool {
	w := newWireWalker(t)

	typ := reflect.TypeOf((*Workspace)(nil)).Elem()
	for i := range typ.NumMethod() {
		m := typ.Method(i)
		class, ok := methodClasses()[m.Name]
		if !ok || (class != "U" && class != "C") {
			continue
		}
		mt := m.Type
		for pi := range mt.NumIn() {
			pt := mt.In(pi)
			if pt == ctxType {
				continue
			}
			w.walk(pt, fmt.Sprintf("Workspace.%s param %d", m.Name, pi), fmt.Sprintf("Workspace.%s.param%d", m.Name, pi))
		}
		numOut := mt.NumOut()
		for ri := range numOut {
			rt := mt.Out(ri)
			if ri == numOut-1 && rt == errType {
				continue
			}
			w.walk(rt, fmt.Sprintf("Workspace.%s result %d", m.Name, ri), fmt.Sprintf("Workspace.%s.result%d", m.Name, ri))
		}
	}

	for _, et := range eventPayloadTypes {
		w.walk(et, "event "+et.String(), "")
	}
	for _, et := range extraHandleAndStreamTypes {
		w.walk(et, "S/H data "+et.String(), "")
	}
	// proto.ShellCommandResponse (AgentRunShellCommand's data) and
	// proto.Thread (AttachThread/EnterWorktree/ExitWorktree's data) are
	// already collected above through ThreadController's U methods and
	// AgentRunShellCommand is S-classed so its own signature is skipped -
	// walk its response type explicitly since nothing else reaches it.
	w.walk(reflectTypeOf[proto.ShellCommandResponse](), "S/H data proto.ShellCommandResponse", "")

	return w.types
}

// TestWireTypeWalkFindsNoForbiddenShapes is collectWireTypes run purely
// for its side effect (t.Errorf on a forbidden shape); the type set it
// returns is exercised by TestWireTypesRoundTripJSON below.
func TestWireTypeWalkFindsNoForbiddenShapes(t *testing.T) {
	t.Parallel()
	collectWireTypes(t)
}

// TestWireTypesRoundTripJSON is the sample/round-trip half of the gate:
// every type collectWireTypes finds needs an entry in wireSamples, that
// entry must not leave an exported field at its zero value (barring an
// explicit exemption in wireSampleZeroExemptions), and it must survive
// json.Marshal -> json.Unmarshal -> require.Equal unchanged.
func TestWireTypesRoundTripJSON(t *testing.T) {
	t.Parallel()

	types := collectWireTypes(t)
	names := make([]string, 0, len(types))
	byName := make(map[string]reflect.Type, len(types))
	for typ := range types {
		key := typeKey(typ)
		names = append(names, key)
		byName[key] = typ
	}
	sort.Strings(names)

	for _, name := range names {
		typ := byName[name]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkNoHiddenStateWithBehavior(t, typ)
			sample, ok := wireSamples[typ]
			if !ok {
				t.Fatalf("no sample registered for wire type %s in wireSamples (wire_dto_samples_test.go)", name)
			}
			wireCheckNoZeroExportedFields(t, name, reflect.ValueOf(sample))
			wireRoundTripJSON(t, name, sample, typ)
		})
	}
}

// checkNoZeroExportedFields fails t, naming the field, if v (expected to
// be an addressable-shaped sample, not necessarily a pointer) leaves any
// exported field at its Go zero value, unless wireSampleZeroExemptions
// lists a reason for it.
func wireCheckNoZeroExportedFields(t *testing.T, typeName string, v reflect.Value) {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			t.Errorf("%s: sample is a nil pointer", typeName)
			return
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return
	}
	rt := v.Type()
	if isOpaque(rt) {
		// Opaque types are sampled as a whole (their own codec is
		// trusted); the zero-field guard only makes sense for the
		// plain structs this test hand-assembles field by field.
		return
	}
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		key := fieldKey(rt, f.Name)
		fv := v.Field(i)
		if fv.IsZero() {
			if reason, ok := wireSampleZeroExemptions[key]; ok {
				_ = reason
				continue
			}
			t.Errorf("%s: sample leaves exported field %s at its zero value (add a value, or exempt it in wireSampleZeroExemptions with a reason)", typeName, key)
		}
	}
}

// wireRoundTripJSON marshals sample, unmarshals into a fresh value of the
// same type, and requires equality. sample is stored in wireSamples
// either as a plain typ value, or (for a type like csync.Map, which
// carries a sync.RWMutex and so must never be copied by value - see
// wireSamples' own entries for it) as a *typ pointer built through its
// real constructor.
//
// Both the marshal and the unmarshal go through an addressable *typ
// rather than sample directly: a handful of collected types (csync.Map,
// which backs config.Config.Providers/RuntimeProviders) implement
// MarshalJSON/UnmarshalJSON on a pointer receiver only, so calling
// json.Marshal on a bare value would silently skip their codec and
// marshal their (all-unexported) fields as "{}" instead of erroring -
// the same trap that makes a plain `error` field encode as "{}" instead
// of failing loudly. Routing every sample through a pointer, opaque or
// not, keeps one code path instead of two.
func wireRoundTripJSON(t *testing.T, typeName string, sample any, typ reflect.Type) {
	src := reflect.New(typ)
	sv := reflect.ValueOf(sample)
	if sv.Kind() == reflect.Pointer {
		src = sv // sample is already a *typ (e.g. csync.NewMap's return).
	} else {
		src.Elem().Set(sv)
	}

	data, err := json.Marshal(src.Interface())
	require.NoError(t, err, "%s: marshal", typeName)

	dst := reflect.New(typ)
	require.NoError(t, json.Unmarshal(data, dst.Interface()), "%s: unmarshal", typeName)

	if isOpaque(typ) {
		// An opaque type's own codec is what this test trusts, not its Go
		// layout - and for csync.Map specifically, taking a value copy of
		// *either* side (src.Elem().Interface() included) reads its
		// embedded sync.RWMutex's counters outside that mutex's own
		// synchronization, racing a concurrent subtest's Marshal call on
		// the same shared sample pointer (subtests run under
		// t.Parallel()). Comparing the two JSON encodings instead proves
		// exactly what this test is for (the same wire bytes come back)
		// without ever copying the value.
		data2, err := json.Marshal(dst.Interface())
		require.NoError(t, err, "%s: re-marshal", typeName)
		require.JSONEq(t, string(data), string(data2), "%s: round trip changed the JSON encoding", typeName)
		return
	}
	require.Equal(t, src.Elem().Interface(), dst.Elem().Interface(), "%s: round trip changed the value", typeName)
}
