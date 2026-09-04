package proto

import (
	"sort"
	"strings"
)

// MCPToolNamePrefix is the prefix internal/agent/tools/mcp-tools.go builds
// every MCP tool's name from: "mcp_" + server + "_" + tool.
const MCPToolNamePrefix = "mcp_"

// SplitMCPToolName splits an MCP tool's composite name ("mcp_<server>_<tool>",
// see internal/agent/tools.Tool.Name) into its server and tool parts.
//
// The server name is a config key, not a generated identifier, so it may
// itself contain underscores — the boundary between server and tool cannot
// be inferred from the string alone. knownServers, when non-empty, is
// matched greedily (longest name first, so "my_server" wins over "my" when
// both are configured) against the "mcp_" prefix to find the real
// boundary. When name matches none of them — knownServers is empty (a
// caller with no config in hand), or the tool's session predates a server
// rename/removal — this falls back to the historical "first underscore"
// split so a name still renders as *something* rather than an error.
func SplitMCPToolName(name string, knownServers []string) (server, tool string, ok bool) {
	rest, ok := strings.CutPrefix(name, MCPToolNamePrefix)
	if !ok {
		return "", "", false
	}

	sorted := append([]string(nil), knownServers...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, s := range sorted {
		if s == "" {
			continue
		}
		if cut, ok := strings.CutPrefix(rest, s+"_"); ok && cut != "" {
			return s, cut, true
		}
	}

	// Fallback: the boundary this codebase used before knownServers
	// existed. Wrong whenever the server name itself contains an
	// underscore, but stable — it always splits at the same place for a
	// given name, so an unrecognized old session renders consistently
	// rather than erroring.
	server, tool, ok = strings.Cut(rest, "_")
	if !ok || server == "" || tool == "" {
		return "", "", false
	}
	return server, tool, true
}

// MCPState represents the current state of an MCP client.
type MCPState int

const (
	MCPStateDisabled MCPState = iota
	MCPStateStarting
	MCPStateConnected
	MCPStateError
	MCPStateNeedsAuth
)

// MarshalText implements the [encoding.TextMarshaler] interface.
func (s MCPState) MarshalText() ([]byte, error) {
	return []byte(s.String()), nil
}

// String returns the string representation of the MCPState.
func (s MCPState) String() string {
	switch s {
	case MCPStateDisabled:
		return "disabled"
	case MCPStateStarting:
		return "starting"
	case MCPStateConnected:
		return "connected"
	case MCPStateError:
		return "error"
	case MCPStateNeedsAuth:
		return "needs auth"
	default:
		return "unknown"
	}
}
