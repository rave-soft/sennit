package proto

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
