package dialog

import (
	"image"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// longQuestionText is a question at the enforced ceiling
// (question.MaxQuestionLength), in the script that made the limit worth
// raising: Cyrillic, where the old byte-counted limit allowed barely half
// as much text as it claimed.
func longQuestionText(t *testing.T) string {
	t.Helper()
	text := strings.Repeat("Начать item-04 в live conformance harness или сперва закрыть item-03? ", 8)
	runes := []rune(text)
	require.GreaterOrEqual(t, len(runes), question.MaxQuestionLength)
	return strings.TrimSpace(string(runes[:question.MaxQuestionLength]))
}

// TestLongQuestionWrapsInEveryComponent: the limit is only defensible if
// the form actually renders a question that long. Each component wraps to
// the available width, so the text arrives as several lines with nothing
// spilling past the right edge.
func TestLongQuestionWrapsInEveryComponent(t *testing.T) {
	t.Parallel()

	const width, height = 80, 40
	text := longQuestionText(t)
	s := styles.SennitDark()

	components := map[string]InlineEditor{
		"single_choice": NewSingleChoice(&s, question.Question{
			ID: "q1", Type: question.TypeSingleChoice, Text: text,
			Description: "Пояснение.",
			Choices: []question.Choice{
				{ID: "a", Label: "Начать item-04"},
				{ID: "b", Label: "Закрыть item-03"},
			},
		}),
		"yes_no": NewYesNo(&s, question.Question{
			ID: "q1", Type: question.TypeYesNo, Text: text, Description: "Пояснение.",
		}),
		"free_text": NewFreeText(&s, question.Question{
			ID: "q1", Type: question.TypeFreeText, Text: text, Description: "Пояснение.",
		}),
	}

	for name, comp := range components {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			area := image.Rect(0, 0, width, height)
			scr := uv.NewScreenBuffer(width, height)
			require.NotPanics(t, func() { comp.Draw(scr, area) })

			rendered := ansi.Strip(scr.String())
			lines := strings.Split(rendered, "\n")
			var textLines int
			for _, line := range lines {
				require.LessOrEqual(t, len([]rune(strings.TrimRight(line, " "))), width,
					"a wrapped line must not run past the terminal")
				if strings.Contains(line, "item-04") || strings.Contains(line, "item-03") {
					textLines++
				}
			}
			require.Greater(t, textLines, 1, "a question this long has to wrap onto several lines")
			require.Contains(t, rendered, "Начать item-04", "the question must start where the reader looks")
			require.Positive(t, comp.Height(width), "the component must report the height it drew")
		})
	}
}
