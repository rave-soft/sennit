package proto_test

import (
	"encoding/json"
	"testing"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/stretchr/testify/require"
)

func TestAgentMessageAttachmentJSONRoundTrip(t *testing.T) {
	t.Parallel()

	original := proto.AgentMessage{
		SessionID: "session-1",
		RunID:     "run-1",
		Prompt:    "inspect attachment",
		Attachments: []proto.Attachment{{
			FilePath: "/tmp/file.txt",
			FileName: "file.txt",
			MimeType: "text/plain",
			Content:  []byte{0, 1, 2, 255},
		}},
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"session_id":"session-1",
		"run_id":"run-1",
		"prompt":"inspect attachment",
		"attachments":[{
			"file_path":"/tmp/file.txt",
			"file_name":"file.txt",
			"mime_type":"text/plain",
			"content":"AAEC/w=="
		}]
	}`, string(data))

	var decoded proto.AgentMessage
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, original, decoded)
}
