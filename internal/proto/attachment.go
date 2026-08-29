package proto

import (
	"encoding/base64"
	"encoding/json"
)

// Attachment represents a file attachment.
type Attachment struct {
	FilePath string `json:"file_path"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	Content  []byte `json:"content"`
}

// MarshalJSON implements the [json.Marshaler] interface, base64-encoding
// Content so a []byte survives the wire as a string rather than as a
// number array.
//
// `deadcode` reports this and UnmarshalJSON below as unreachable, and they
// are not: encoding/json finds them by interface assertion at run time,
// which no static reachability analysis can follow. AgentMessage carries
// Attachments and is marshalled on every dispatch. Deleting either — the
// 2026-08-28 review listed both as dead — would silently change how every
// attachment is encoded, and the pairing is what the round-trip test in
// agent_message_test.go pins.
func (a Attachment) MarshalJSON() ([]byte, error) {
	type Alias Attachment
	return json.Marshal(&struct {
		Content string `json:"content"`
		*Alias
	}{
		Content: base64.StdEncoding.EncodeToString(a.Content),
		Alias:   (*Alias)(&a),
	})
}

// UnmarshalJSON implements the [json.Unmarshaler] interface.
func (a *Attachment) UnmarshalJSON(data []byte) error {
	type Alias Attachment
	aux := &struct {
		Content string `json:"content"`
		*Alias
	}{
		Alias: (*Alias)(a),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	content, err := base64.StdEncoding.DecodeString(aux.Content)
	if err != nil {
		return err
	}
	a.Content = content
	return nil
}
