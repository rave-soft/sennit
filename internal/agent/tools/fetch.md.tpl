Fetch raw content from a URL as text, markdown, or html (max {{ .MaxFetchSizeKB }}KB, less when the context window is nearly full); no AI processing. For analysis or extraction use agentic_fetch.
{{- if .GhAvailable }} For GitHub content when an exact repo, issue, or PR link is provided, use `gh` CLI in bash instead.{{- end }}
