// Package mcpid holds identifiers for built-in MCP servers that both the
// config layer and the UI layer need to agree on. It is a leaf package —
// it imports nothing else from this repo — specifically so that
// internal/config and internal/ui/chat can both reference DockerMCPName
// without either depending on the other for it.
package mcpid

// DockerMCPName is the name of the Docker MCP configuration.
const DockerMCPName = "docker"
