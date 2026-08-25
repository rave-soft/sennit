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

// MarshalJSON implements the [json.Marshaler] interface.
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
