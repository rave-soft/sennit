Fetch a web URL and return its content converted to markdown (HTML/JSON only, not raw text). Large pages (>50KB) are saved to a temp file for grep/view instead of being returned inline.
{{- if .GhAvailable }} For GitHub content when an exact repo, issue, or PR link is provided, use `gh` CLI in bash instead.{{- end }}
