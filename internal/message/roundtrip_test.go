package message

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// roundTripFixtures is one fully-populated value per ContentPart
// implementation. Every field carries a distinctive non-zero value on
// purpose: a part that survives the trip with a field silently dropped —
// a missing json tag, a case added to MarshalParts but not to
// UnmarshalParts — is the failure this pins, and a zero-valued fixture
// field cannot tell that apart from a working one.
//
// The list is checked for completeness against the source below rather
// than maintained by hand, because the cost of forgetting an entry is
// paid by users and not by this test: MarshalParts errors on a part type
// it does not know, but UnmarshalParts *skips* a wrapper type it does not
// know (deliberately — see its doc comment), so a half-wired part type
// writes fine and comes back missing from the message.
var roundTripFixtures = []ContentPart{
	ReasoningContent{
		Thinking:         "thinking",
		Signature:        "signature",
		ThoughtSignature: "thought-signature",
		ToolID:           "tool-id",
		ResponsesData: &ResponsesReasoningMetadata{
			ItemID:           "item-id",
			EncryptedContent: ptr("encrypted"),
			Summary:          []string{"summary"},
		},
		StartedAt:  11,
		FinishedAt: 22,
	},
	TextContent{Text: "text"},
	ImageURLContent{URL: "https://example.invalid/i.png", Detail: "high"},
	BinaryContent{Path: "/tmp/x.bin", MIMEType: "application/octet-stream", Data: []byte{1, 2, 3}},
	ToolCall{ID: "call-1", Name: "bash", Input: `{"command":"ls"}`, ProviderExecuted: true, Finished: true},
	ToolResult{
		ToolCallID: "call-1",
		Name:       "bash",
		Content:    "content",
		Data:       "data",
		MIMEType:   "text/plain",
		Metadata:   `{"k":"v"}`,
		IsError:    true,
	},
	Finish{Reason: FinishReasonEndTurn, Time: 42, Message: "message", Details: "details"},
	ShellCommand{Command: "echo hi", Output: "hi", ExitCode: 3},
}

func ptr[T any](v T) *T { return &v }

// TestMarshalParts_EveryPartTypeRoundTrips sends all part types through
// one blob together, the way a real message carries them, and requires
// the decoded slice to equal the original element for element.
func TestMarshalParts_EveryPartTypeRoundTrips(t *testing.T) {
	t.Parallel()

	data, err := MarshalParts(roundTripFixtures)
	require.NoError(t, err)

	parts, err := UnmarshalParts(data, "msg-1")
	require.NoError(t, err)
	require.Equal(t, roundTripFixtures, parts)

	// And once more per part on its own, so a failure names the type
	// instead of pointing at a slice diff.
	for _, part := range roundTripFixtures {
		data, err := MarshalParts([]ContentPart{part})
		require.NoError(t, err, "%T", part)

		got, err := UnmarshalParts(data, "msg-1")
		require.NoError(t, err, "%T", part)
		require.Equal(t, []ContentPart{part}, got, "%T does not survive a round trip", part)
	}
}

// TestRoundTripFixtures_LeaveNoFieldZero keeps the fixtures honest. A
// round-trip test whose fixture leaves a field at its zero value passes
// whether or not that field is carried, so the fixture — not the code —
// decides what the test covers.
func TestRoundTripFixtures_LeaveNoFieldZero(t *testing.T) {
	t.Parallel()

	for _, part := range roundTripFixtures {
		v := reflect.ValueOf(part)
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			require.False(t, v.Field(i).IsZero(),
				"%T.%s is zero in its fixture, so the round trip does not actually cover it", part, field.Name)
		}
	}
}

// TestRoundTripFixtures_CoverEveryContentPart reads the package source for
// the types that implement ContentPart — the method set is the definition,
// so a new part type is found the moment it is declared — and requires a
// fixture for each. formatMeta is excluded: it is the synthetic version
// marker, never a member of a Message's Parts.
func TestRoundTripFixtures_CoverEveryContentPart(t *testing.T) {
	t.Parallel()

	covered := map[string]bool{}
	for _, part := range roundTripFixtures {
		covered[reflect.TypeOf(part).Name()] = true
	}

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	declared := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		require.NoError(t, err)

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "isPart" || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			ident, ok := fn.Recv.List[0].Type.(*ast.Ident)
			if !ok || ident.Name == "formatMeta" {
				continue
			}
			declared++
			require.True(t, covered[ident.Name],
				"%s implements ContentPart but has no fixture in roundTripFixtures, so nothing checks that it survives being stored and read back", ident.Name)
		}
	}
	require.Equal(t, len(roundTripFixtures), declared,
		"roundTripFixtures carries an entry that no longer implements ContentPart")
}
