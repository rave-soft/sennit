package chat

import (
	"testing"
)

func TestHumanizedToolName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "snake case", input: "mcp_github_get", want: "Mcp Github Get"},
		{name: "kebab case", input: "web-fetch", want: "Web Fetch"},
		{name: "mixed", input: "job_output-tool", want: "Job Output Tool"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := humanizedToolName(tt.input); got != tt.want {
				t.Fatalf("humanizedToolName() = %q, want %q", got, tt.want)
			}
		})
	}
}
