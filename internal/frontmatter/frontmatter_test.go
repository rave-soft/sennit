package frontmatter

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSplit covers the awkward cases the three callers share. They are the
// reason this is one function rather than three: a SKILL.md, an agent
// markdown file and a foreign file being imported must all answer these the
// same way, and none of them had a test for any of it while the code lived
// as an exported helper inside internal/skills.
func TestSplit(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		content string
		header  string
		body    string
		wantErr string
	}{
		{
			name:    "plain",
			content: "---\nname: a\n---\nbody text\n",
			header:  "name: a",
			body:    "body text\n",
		},
		{
			// Editors on Windows write both, and a byte-order mark ahead
			// of the opening fence would otherwise make the first line
			// not equal "---".
			name:    "BOM and CRLF",
			content: "\uFEFF---\r\nname: a\r\n---\r\nbody\r\n",
			header:  "name: a",
			body:    "body\n",
		},
		{
			name:    "blank lines before the fence",
			content: "\n\n---\nname: a\n---\nbody",
			header:  "name: a",
			body:    "body",
		},
		{
			name:    "empty frontmatter block",
			content: "---\n---\nbody",
			header:  "",
			body:    "body",
		},
		{
			// A horizontal rule further down must not be mistaken for a
			// closing fence's partner: the block ends at the first "---"
			// after the opening one, and everything past it is body.
			name:    "horizontal rule in the body",
			content: "---\nname: a\n---\nintro\n\n---\n\nmore",
			header:  "name: a",
			body:    "intro\n\n---\n\nmore",
		},
		{
			name:    "no frontmatter",
			content: "just a document\n",
			wantErr: "no YAML frontmatter found",
		},
		{
			name:    "unclosed",
			content: "---\nname: a\nbody with no closing fence\n",
			wantErr: "unclosed frontmatter",
		},
		{
			name:    "empty file",
			content: "",
			wantErr: "no YAML frontmatter found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			header, body, err := Split(tc.content)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.header, header)
			require.Equal(t, tc.body, body)
		})
	}
}
