package shell

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSyncBufferUnderHeadCapIsUntruncated(t *testing.T) {
	t.Parallel()
	var sb syncBuffer
	content := strings.Repeat("x", MaxSyncBufferHead/2)
	n, err := sb.Write([]byte(content))
	require.NoError(t, err)
	require.Equal(t, len(content), n)
	require.Equal(t, content, sb.String())
	require.Equal(t, len(content), sb.Len())
}

func TestSyncBufferOverCapRetainsHeadAndTailWithMarker(t *testing.T) {
	t.Parallel()
	var sb syncBuffer

	// Build content well past both caps, written in chunks so the ring
	// buffer's wraparound path is exercised.
	total := MaxSyncBufferHead + MaxSyncBufferTail + 4*1024*1024
	full := make([]byte, total)
	for i := range full {
		full[i] = byte('a' + i%26)
	}
	const chunk = 4096
	for off := 0; off < len(full); off += chunk {
		end := min(off+chunk, len(full))
		n, err := sb.Write(full[off:end])
		require.NoError(t, err)
		require.Equal(t, end-off, n)
	}

	got := sb.String()
	require.LessOrEqual(t, len(got), MaxSyncBufferHead+MaxSyncBufferTail+128)

	wantHead := string(full[:MaxSyncBufferHead])
	wantTail := string(full[len(full)-MaxSyncBufferTail:])
	require.True(t, strings.HasPrefix(got, wantHead), "head mismatch")
	require.True(t, strings.HasSuffix(got, wantTail), "tail mismatch")

	dropped := len(full) - MaxSyncBufferHead - MaxSyncBufferTail
	marker := fmt.Sprintf("... [%d bytes dropped] ...", dropped)
	require.Contains(t, got, marker)
	require.Equal(t, len(got), sb.Len())
}

// TestSyncBufferJustOverHeadCapReproducesExactly covers the seam between
// "fits entirely in head" and "way past both caps": a stream that crosses
// MaxSyncBufferHead by a small margin must still round-trip byte-for-byte,
// because head+tail still cover the whole thing without a gap. This is the
// case a wrong tail-ring seed silently breaks (double-counting or losing
// the bytes written just before the crossing).
func TestSyncBufferJustOverHeadCapReproducesExactly(t *testing.T) {
	t.Parallel()

	total := MaxSyncBufferHead + 1024
	full := make([]byte, total)
	for i := range full {
		full[i] = byte(i % 251)
	}

	var sb syncBuffer
	n, err := sb.Write(full)
	require.NoError(t, err)
	require.Equal(t, len(full), n)
	require.Equal(t, string(full), sb.String())
	require.Equal(t, len(full), sb.Len())
}

// TestSyncBufferSmallWriteDoesNotAllocateTailRing pins the fix for the
// regression where any write, however small, eagerly allocated the 256 KiB
// tail ring. bash's synchronous path creates a BackgroundShell (two
// syncBuffers) per invocation, not just for backgrounded commands, so that
// allocation must stay deferred until the stream actually outgrows head.
func TestSyncBufferSmallWriteDoesNotAllocateTailRing(t *testing.T) {
	t.Parallel()

	var sb syncBuffer
	_, err := sb.Write([]byte("hello"))
	require.NoError(t, err)
	require.Nil(t, sb.tail, "tail ring must stay unallocated while the stream fits in head")
	require.Equal(t, "hello", sb.String())
}

// TestSyncBufferLenMatchesStringLength checks the O(1) arithmetic Len()
// against len(String()) both when nothing was dropped and when the
// dropped-byte marker is present, since a formula (not len(String())
// itself) computes it now.
func TestSyncBufferLenMatchesStringLength(t *testing.T) {
	t.Parallel()

	t.Run("no drop", func(t *testing.T) {
		t.Parallel()
		var sb syncBuffer
		_, err := sb.Write([]byte(strings.Repeat("y", MaxSyncBufferHead+1024)))
		require.NoError(t, err)
		require.Equal(t, len(sb.String()), sb.Len())
	})

	t.Run("with drop", func(t *testing.T) {
		t.Parallel()
		var sb syncBuffer
		_, err := sb.Write([]byte(strings.Repeat("z", 2*(MaxSyncBufferHead+MaxSyncBufferTail))))
		require.NoError(t, err)
		require.Equal(t, len(sb.String()), sb.Len())
	})
}

func TestSyncBufferChunkedWritesMatchSingleWrite(t *testing.T) {
	t.Parallel()

	total := MaxSyncBufferHead + 1024
	full := make([]byte, total)
	for i := range full {
		full[i] = byte(i % 251)
	}

	var whole syncBuffer
	_, err := whole.Write(full)
	require.NoError(t, err)

	var chunked syncBuffer
	const chunk = 97 // deliberately not a multiple of the cap
	for off := 0; off < len(full); off += chunk {
		end := min(off+chunk, len(full))
		_, err := chunked.Write(full[off:end])
		require.NoError(t, err)
	}

	// Assert against the source bytes, not only against whole.String():
	// comparing the two syncBuffers to each other would pass even if both
	// were wrong the same way, and the chunked path is the only one that
	// exercises seeding the tail ring from head at the crossing write.
	require.Equal(t, string(full), chunked.String())
	require.Equal(t, whole.String(), chunked.String())
}

func TestSyncBufferConcurrentWriteAndString(t *testing.T) {
	t.Parallel()

	var sb syncBuffer
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 200 {
				_, _ = sb.WriteString("payload-line\n")
			}
		})
	}
	wg.Go(func() {
		for range 200 {
			_ = sb.String()
			_ = sb.Len()
		}
	})
	wg.Wait()
}
