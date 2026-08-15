package proto

import (
	"fmt"
)

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

// UnmarshalText implements the [encoding.TextUnmarshaler] interface.
func (s *MCPState) UnmarshalText(data []byte) error {
	switch string(data) {
	case "disabled":
		*s = MCPStateDisabled
	case "starting":
		*s = MCPStateStarting
	case "connected":
		*s = MCPStateConnected
	case "error":
		*s = MCPStateError
	case "needs auth":
		*s = MCPStateNeedsAuth
	default:
		return fmt.Errorf("unknown mcp state: %s", data)
	}
	return nil
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
