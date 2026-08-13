package swagger

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAliasSchemasPreserveWireContracts(t *testing.T) {
	t.Parallel()

	var spec struct {
		Definitions map[string]struct {
			Properties map[string]struct {
				Type string `json:"type"`
				Ref  string `json:"$ref"`
			} `json:"properties"`
		} `json:"definitions"`
	}
	require.NoError(t, json.Unmarshal([]byte(SwaggerInfo.ReadDoc()), &spec))

	require.Contains(t, spec.Definitions, "proto.LSPClientInfo")
	lspClient := spec.Definitions["proto.LSPClientInfo"]
	require.Equal(t, map[string]struct {
		Type string `json:"type"`
		Ref  string `json:"$ref"`
	}{
		"connected_at":     {Type: "string"},
		"diagnostic_count": {Type: "integer"},
		"error":            {},
		"name":             {Type: "string"},
		"state":            {Ref: "#/definitions/lsp.ServerState"},
	}, lspClient.Properties)
	require.NotContains(t, lspClient.Properties, "client")

	require.Contains(t, spec.Definitions, "proto.Todo")
	todo := spec.Definitions["proto.Todo"]
	require.Contains(t, todo.Properties, "status")
	require.Equal(t, "string", todo.Properties["status"].Type)
}
