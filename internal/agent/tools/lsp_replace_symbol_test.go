package tools

import (
	"testing"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/stretchr/testify/require"
)

// TestSymbolRangeEndLine is the regression test for eating one line too
// many (or, for add_after, inserting one line too late) when an LSP range
// ends at column 0: per protocol.Range's own doc comment, a range spanning
// through the end of line 11 (0-indexed) is reported as
// End:{Line:12, Character:0} - the start of the FOLLOWING line, not a
// position on line 12 itself.
func TestSymbolRangeEndLine(t *testing.T) {
	t.Parallel()

	t.Run("end at column 0 on a later line backs off to the true last line", func(t *testing.T) {
		t.Parallel()
		rng := protocol.Range{
			Start: protocol.Position{Line: 5, Character: 0},
			End:   protocol.Position{Line: 12, Character: 0},
		}
		require.Equal(t, 11, symbolRangeEndLine(rng))
	})

	t.Run("end mid-line is unaffected", func(t *testing.T) {
		t.Parallel()
		rng := protocol.Range{
			Start: protocol.Position{Line: 5, Character: 0},
			End:   protocol.Position{Line: 12, Character: 4},
		}
		require.Equal(t, 12, symbolRangeEndLine(rng))
	})

	t.Run("a single-line range ending at column 0 is not adjusted", func(t *testing.T) {
		t.Parallel()
		// Start == End.Line guards against a genuinely empty/zero-width
		// range being pushed to a negative line.
		rng := protocol.Range{
			Start: protocol.Position{Line: 5, Character: 0},
			End:   protocol.Position{Line: 5, Character: 0},
		}
		require.Equal(t, 5, symbolRangeEndLine(rng))
	})
}
