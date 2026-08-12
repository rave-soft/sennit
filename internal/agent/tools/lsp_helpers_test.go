package tools

import (
	"errors"
	"testing"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/stretchr/testify/require"
)

func TestGetSymbolOffset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		symbol string
		want   int
	}{
		{"bare symbol", "Bar", 0},
		{"dot qualified", "foo.Bar", 4},
		{"double colon qualified", "Class::method", 7},
		{"backslash qualified", `ns\Func`, 3},
		{"nested dots", "a.b.C", 4},
		{"empty", "", 0},
		{"single char", "x", 0},
		{"dot at end", "foo.", 4},
		{"colon at end", "foo::", 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := getSymbolOffset(tt.symbol)
			require.Equal(t, tt.want, got, "getSymbolOffset(%q)", tt.symbol)
		})
	}
}

func TestFirstWithDefinitionSkipsEmptyAndInvalidCandidates(t *testing.T) {
	t.Parallel()

	candidates := []string{"comment", "unresolved", "definition"}
	got, err := firstWithDefinition(candidates, func(candidate string) ([]protocol.Location, error) {
		switch candidate {
		case "comment":
			return nil, errors.New("no identifier found")
		case "unresolved":
			return nil, nil
		default:
			return []protocol.Location{{}}, nil
		}
	})

	require.NoError(t, err)
	require.Equal(t, "definition", got)
}

func TestFirstWithDefinitionReturnsLastMeaningfulError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("language server unavailable")
	got, err := firstWithDefinition([]string{"empty", "error", "invalid"}, func(candidate string) ([]protocol.Location, error) {
		switch candidate {
		case "error":
			return nil, wantErr
		case "invalid":
			return nil, errors.New("no identifier found")
		default:
			return nil, nil
		}
	})

	require.ErrorIs(t, err, wantErr)
	require.Empty(t, got)
}

// TestGetSymbolOffset_DoesNotOvershoot verifies that the offset lands
// on the start of the final component, never past it.
func TestGetSymbolOffset_DoesNotOvershoot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		symbol   string
		expected string // the substring starting at the offset
	}{
		{"foo.Bar", "Bar"},
		{"Class::method", "method"},
		{`ns\Func`, "Func"},
		{"a.b.c.D", "D"},
		{"Bar", "Bar"},
	}

	for _, tc := range cases {
		offset := getSymbolOffset(tc.symbol)
		require.LessOrEqual(t, offset, len(tc.symbol),
			"offset %d exceeds symbol length %d for %q", offset, len(tc.symbol), tc.symbol)
		got := tc.symbol[offset:]
		require.Equal(t, tc.expected, got,
			"getSymbolOffset(%q) = %d, remainder = %q, want %q",
			tc.symbol, offset, got, tc.expected)
	}
}
