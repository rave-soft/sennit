package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/lsp"
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

// newNoLSPManager returns a manager with no LSP server configured at all,
// so findLSPClient never matches anything regardless of what grep finds.
func newNoLSPManager(t *testing.T) *lsp.Manager {
	t.Helper()
	store := configtest.NewStore(t, &config.Config{})
	manager := lsp.NewManager(store)
	t.Cleanup(func() { manager.StopAll(t.Context()) })
	return manager
}

// writeManySymbolMatches writes a single file with count occurrences of
// symbol "count", one per line, so a grep against it hits the 100-match
// cap resolveSymbolResults uses well before running out of file to
// search. Both the symbol and the count are fixed rather than
// parameters: every caller wants the same ones, and naming them here
// keeps the callers from having to agree on them separately.
// symbolMatchCount is comfortably above resolveSymbolResults' own
// 100-match cap, so a grep over the file it writes always truncates.
const symbolMatchCount = 150

func writeManySymbolMatches(t *testing.T, root string) {
	t.Helper()
	var b strings.Builder
	for i := range symbolMatchCount {
		fmt.Fprintf(&b, "count line %d\n", i)
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "data.txt"), []byte(b.String()), 0o644))
}

// TestResolveSymbolResultsReportsTruncationWithNoLSPClient is the
// regression test for the "no LSP client handles any match" branch of
// resolveSymbolResults: it used to hardcode truncated=false regardless of
// what searchFiles actually reported, so a capped grep whose every
// candidate landed in a file with no configured server silently claimed
// the search had been exhaustive.
func TestResolveSymbolResultsReportsTruncationWithNoLSPClient(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeManySymbolMatches(t, root)

	manager := newNoLSPManager(t)
	_, truncated, err := resolveSymbolResults(t.Context(), manager, "count", root)
	require.Error(t, err)
	require.True(t, isGenuineSymbolMiss(err))
	require.True(t, truncated,
		"150 occurrences over the 100-match grep cap must report truncated even though none had an LSP client")
}

// TestResolveSymbolPropagatesTruncation is the regression test for
// finding 3: resolveSymbol used to discard resolveSymbolResults' third
// return value entirely, so every caller that only goes through
// resolveSymbol (definition, rename, call hierarchy) had no way to tell
// a capped search from an exhaustive one.
func TestResolveSymbolPropagatesTruncation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeManySymbolMatches(t, root)

	manager := newNoLSPManager(t)
	_, truncated, err := resolveSymbol(t.Context(), manager, "count", root)
	require.Error(t, err)
	require.True(t, isGenuineSymbolMiss(err))
	require.True(t, truncated, "resolveSymbol must forward resolveSymbolResults' truncated flag, not drop it")
}
