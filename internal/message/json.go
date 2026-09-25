package message

import "encoding/json"

// messageJSON is the wire shape [Message.MarshalJSON] and
// [Message.UnmarshalJSON] delegate to. Parts is carried as the same
// type-tagged blob [MarshalParts]/[UnmarshalParts] already produce for
// SQLite, so a message crossing the workspace/UI boundary as JSON uses
// the exact same part encoding the DB does rather than a second one.
type messageJSON struct {
	ID                  string          `json:"id"`
	Role                MessageRole     `json:"role"`
	SessionID           string          `json:"session_id"`
	Parts               json.RawMessage `json:"parts"`
	Model               string          `json:"model"`
	Provider            string          `json:"provider"`
	CreatedAt           int64           `json:"created_at"`
	UpdatedAt           int64           `json:"updated_at"`
	IsSummaryMessage    bool            `json:"is_summary_message"`
	Origin              Origin          `json:"origin"`
	SummaryBeforeTokens int64           `json:"summary_before_tokens"`
	SummaryAfterTokens  int64           `json:"summary_after_tokens"`
}

// MarshalJSON implements [json.Marshaler]. Parts is left sealed (see
// [ContentPart]), so it cannot be marshaled by reflection; it goes
// through [MarshalParts] instead, exactly as the DB does.
func (m Message) MarshalJSON() ([]byte, error) {
	partsData, err := MarshalParts(m.Parts)
	if err != nil {
		return nil, err
	}
	return json.Marshal(messageJSON{
		ID:                  m.ID,
		Role:                m.Role,
		SessionID:           m.SessionID,
		Parts:               partsData,
		Model:               m.Model,
		Provider:            m.Provider,
		CreatedAt:           m.CreatedAt,
		UpdatedAt:           m.UpdatedAt,
		IsSummaryMessage:    m.IsSummaryMessage,
		Origin:              m.Origin,
		SummaryBeforeTokens: m.SummaryBeforeTokens,
		SummaryAfterTokens:  m.SummaryAfterTokens,
	})
}

// UnmarshalJSON implements [json.Unmarshaler], the counterpart to
// MarshalJSON above. A nil or absent "parts" field decodes to an empty,
// non-nil slice — the same normalization [UnmarshalParts] already
// applies to an empty parts array, so a Message with nil Parts and one
// with empty Parts produce the same JSON and decode back identically.
func (m *Message) UnmarshalJSON(data []byte) error {
	var aux messageJSON
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	parts := []ContentPart{}
	if len(aux.Parts) > 0 {
		var err error
		parts, err = UnmarshalParts(aux.Parts, aux.ID)
		if err != nil {
			return err
		}
	}

	*m = Message{
		ID:                  aux.ID,
		Role:                aux.Role,
		SessionID:           aux.SessionID,
		Parts:               parts,
		Model:               aux.Model,
		Provider:            aux.Provider,
		CreatedAt:           aux.CreatedAt,
		UpdatedAt:           aux.UpdatedAt,
		IsSummaryMessage:    aux.IsSummaryMessage,
		Origin:              aux.Origin,
		SummaryBeforeTokens: aux.SummaryBeforeTokens,
		SummaryAfterTokens:  aux.SummaryAfterTokens,
	}
	return nil
}
