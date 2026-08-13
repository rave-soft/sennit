package lsp

import (
	"encoding/json"
	"errors"
	"time"
)

// ClientInfo holds the state of an LSP client. Client is runtime-only and is
// deliberately excluded from wire representations.
//
// @name proto.LSPClientInfo
type ClientInfo struct {
	Name            string      `json:"name"`
	State           ServerState `json:"state"`
	Error           error       `json:"error,omitempty"`
	Client          *Client     `json:"-"`
	DiagnosticCount int         `json:"diagnostic_count,omitempty"`
	ConnectedAt     time.Time   `json:"connected_at"`
}

// MarshalJSON implements [json.Marshaler], encoding errors as their messages.
func (i ClientInfo) MarshalJSON() ([]byte, error) {
	type alias ClientInfo
	return json.Marshal(&struct {
		Error string `json:"error,omitempty"`
		alias
	}{
		Error: func() string {
			if i.Error != nil {
				return i.Error.Error()
			}
			return ""
		}(),
		alias: alias(i),
	})
}

// UnmarshalJSON implements [json.Unmarshaler], restoring non-empty error
// messages as errors while leaving the runtime Client unset.
func (i *ClientInfo) UnmarshalJSON(data []byte) error {
	type alias ClientInfo
	var aux struct {
		Error string `json:"error,omitempty"`
		alias
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*i = ClientInfo(aux.alias)
	if aux.Error != "" {
		i.Error = errors.New(aux.Error)
	}
	return nil
}
