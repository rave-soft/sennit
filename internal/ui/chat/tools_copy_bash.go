package chat

import (
	"encoding/json"
	"fmt"

	tools "github.com/rave-soft/sennit/internal/proto"
)

// formatBashResultForCopy formats bash tool results for clipboard.
func (t *baseToolMessageItem) formatBashResultForCopy() string {
	if t.result == nil {
		return ""
	}

	var meta tools.BashResponseMetadata
	if t.result.Metadata != "" {
		if err := json.Unmarshal([]byte(t.result.Metadata), &meta); err != nil {
			return t.result.Content
		}
	}

	output := meta.Output
	if output == "" && t.result.Content != tools.BashNoOutput {
		output = t.result.Content
	}

	if output == "" {
		return ""
	}

	return fmt.Sprintf("```bash\n%s\n```", output)
}
