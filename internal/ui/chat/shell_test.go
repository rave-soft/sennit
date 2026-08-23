package chat

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// benchmarkShellOutput is a package-level sink that prevents the
// compiler from optimizing away benchmark results.
var benchmarkShellOutput string

// TestPendingShellItemRendersStreamedOutput verifies that a pending shell
// item surfaces output appended during execution, rather than hiding it
// behind the spinner until completion.
func TestPendingShellItemRendersStreamedOutput(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewPendingShellItem(&sty, "ping -c 3 localhost")

	require.NotContains(t, ansi.Strip(item.Render(80)), "bytes from",
		"freshly created pending item should have no output yet")

	item.AppendOutput("64 bytes from localhost: icmp_seq=0\n")
	item.AppendOutput("64 bytes from localhost: icmp_seq=1\n")

	rendered := ansi.Strip(item.Render(80))
	require.Contains(t, rendered, "icmp_seq=0",
		"streamed output must be visible while the command is still running")
	require.Contains(t, rendered, "icmp_seq=1")
}

// TestPendingShellItemShowsTail verifies that while streaming, the most
// recent lines are shown (tail) rather than the first lines.
func TestPendingShellItemShowsTail(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewPendingShellItem(&sty, "seq 30")

	for i := 1; i <= 30; i++ {
		item.AppendOutput(strconv.Itoa(i) + "\n")
	}

	rendered := ansi.Strip(item.Render(80))
	require.Contains(t, rendered, "30", "the newest line must be visible while streaming")
	require.Contains(t, rendered, "earlier lines", "older lines should be summarized as a count")
}

// TestCompletedShellItemRendersOutput verifies completion still shows output.
func TestCompletedShellItemRendersOutput(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewPendingShellItem(&sty, "echo hi")
	item.Complete("hi\n", 0)

	require.Contains(t, ansi.Strip(item.Render(80)), "hi")
}

func TestShellItemPreservesOutputAcrossChunks(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewPendingShellItem(&sty, "printf")
	item.AppendOutput("first\n\x1b[")
	item.AppendOutput("31msecond\x1b[0m\n")

	require.Equal(t, "first\n\x1b[31msecond\x1b[0m\n", item.output.String())
	require.Contains(t, ansi.Strip(item.RawRender(80)), "second")
}

func TestShellItemCompleteReplacesPartialOutputAndIgnoresLateChunks(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewPendingShellItem(&sty, "printf")
	item.AppendOutput("partial\n")
	item.Complete("complete\nresult\n", 0)
	item.AppendOutput("late duplicate\n")

	require.Equal(t, "complete\nresult\n", item.output.String())
	require.NotContains(t, ansi.Strip(item.RawRender(80)), "late duplicate")
}

func TestPendingShellItemPreservesANSIBoundaryInCollapsedTail(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewPendingShellItem(&sty, "seq 12")
	item.AppendOutput("1\n2\n\x1b[")
	item.AppendOutput("31m3\x1b[0m\n4\n5\n6\n7\n8\n9\n10\n11\n12\n")

	rendered := item.RawRender(80)
	require.Contains(t, ansi.Strip(rendered), "3")
	require.Contains(t, rendered, common.RemapANSI16("\x1b[31m3\x1b[0m", sty.ANSI))
	require.Contains(t, ansi.Strip(rendered), "2 earlier lines")
}

func TestCompletedShellItemShowsHeadAndAccurateCount(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewPendingShellItem(&sty, "seq 12")
	item.Complete("1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n\x1b[0m\n", 0)

	rendered := ansi.Strip(item.RawRender(80))
	lines := strings.Split(rendered, "\n")
	require.Len(t, lines, 12)
	require.Equal(t, "$ seq 12", lines[0])
	require.Equal(t, []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}, lines[1:11])
	require.Equal(t, "… 2 more lines", lines[11])
}

// TestShellItemHorizontalScrollDoesNotWidenLine pins that once a line is
// horizontally scrolled, the leading "…" marker is carved out of the
// truncation width rather than glued on afterward. Gluing it on after
// ansi.Truncate already filled cappedWidth pushes every scrolled line one
// column past the render area.
func TestShellItemHorizontalScrollDoesNotWidenLine(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewPendingShellItem(&sty, "cat wide.txt")
	item.Complete(strings.Repeat("x", 200)+"\n", 0)

	const width = 80
	cappedWidth := cappedMessageWidth(width)

	item.ScrollHorizontal(10)
	out := item.RawRender(width)

	for _, line := range strings.Split(out, "\n") {
		require.LessOrEqualf(t, ansi.StringWidth(ansi.Strip(line)), cappedWidth,
			"scrolled line must not exceed the capped width %d: %q", cappedWidth, line)
	}
}

// TestShellItemHoverableAtMatchesRawRenderLineCount pins that HoverableAt
// and RawRender agree on how many lines the trimmed output has. They used
// to trim differently (strings.TrimSpace vs. a trailing-only whitespace/
// ANSI-reset trim), so an output block could be reported clickable by
// HoverableAt while RawRender's own count disagreed about whether it was
// over the collapse threshold — or vice versa.
func TestShellItemHoverableAtMatchesRawRenderLineCount(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	item := NewPendingShellItem(&sty, "seq 11")
	// A leading blank line: TrimSpace strips it (making the block look
	// one line shorter than shellMaxCollapsedLines), while the
	// trailing-only trim used by RawRender does not.
	item.Complete("\n1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n", 0)

	rendered := ansi.Strip(item.RawRender(80))
	rawIsExpandable := strings.Contains(rendered, "more lines")

	require.Equal(t, rawIsExpandable, item.HoverableAt(0, 1, 0),
		"HoverableAt must agree with RawRender about whether the block can expand")
}

func TestShellOutputWindows(t *testing.T) {
	t.Parallel()

	const output = "1\n2\n3\n4\n5"
	require.Equal(t, "1\n2", firstLines(output, 2))
	require.Equal(t, "4\n5", lastLines(output, 2))
	require.Equal(t, output, firstLines(output, 10))
	require.Equal(t, output, lastLines(output, 10))
	require.Empty(t, firstLines(output, 0))
	require.Empty(t, lastLines(output, 0))
}

func BenchmarkShellItemAppendOutput(b *testing.B) {
	for _, chunkSize := range []int{100, 1024} {
		b.Run(strconv.Itoa(chunkSize)+"B_chunks", func(b *testing.B) {
			sty := styles.SennitDark()
			chunk := strings.Repeat("x", chunkSize-1) + "\n"
			const outputSize = 1 << 20

			b.ReportAllocs()
			b.SetBytes(outputSize)
			b.ResetTimer()
			for b.Loop() {
				item := NewPendingShellItem(&sty, "benchmark")
				for written := 0; written < outputSize; written += chunkSize {
					remaining := outputSize - written
					if remaining < chunkSize {
						item.AppendOutput(chunk[:remaining])
					} else {
						item.AppendOutput(chunk)
					}
				}
				benchmarkShellOutput = item.output.String()
			}
		})
	}
}

func BenchmarkShellItemCollapsedRender(b *testing.B) {
	sty := styles.SennitDark()
	item := NewPendingShellItem(&sty, "benchmark")
	item.AppendOutput(strings.Repeat("0123456789abcdef\n", (1<<20)/17))

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		benchmarkShellOutput = item.RawRender(120)
	}
}
