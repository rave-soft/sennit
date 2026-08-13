package lsp

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClientInfoJSONContract(t *testing.T) {
	t.Parallel()

	connectedAt := time.Date(2025, time.January, 2, 3, 4, 5, 0, time.FixedZone("UTC+3", 3*60*60))
	info := ClientInfo{
		Name:            "gopls",
		State:           StateReady,
		Error:           errors.New("connection failed"),
		Client:          &Client{},
		DiagnosticCount: 2,
		ConnectedAt:     connectedAt,
	}

	data, err := json.Marshal(info)
	require.NoError(t, err)
	require.JSONEq(t, `{"name":"gopls","state":2,"error":"connection failed","diagnostic_count":2,"connected_at":"2025-01-02T03:04:05+03:00"}`, string(data))
	require.NotContains(t, string(data), "Client")
	require.NotContains(t, string(data), "client")

	var restored ClientInfo
	require.NoError(t, json.Unmarshal(data, &restored))
	require.Equal(t, info.Name, restored.Name)
	require.Equal(t, info.State, restored.State)
	require.Equal(t, info.DiagnosticCount, restored.DiagnosticCount)
	require.True(t, info.ConnectedAt.Equal(restored.ConnectedAt))
	require.EqualError(t, restored.Error, "connection failed")
	require.Nil(t, restored.Client)
}

func TestClientInfoJSONOmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(ClientInfo{Name: "gopls", State: StateUnstarted})
	require.NoError(t, err)
	require.JSONEq(t, `{"name":"gopls","state":0,"connected_at":"0001-01-01T00:00:00Z"}`, string(data))
}
