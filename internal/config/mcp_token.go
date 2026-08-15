package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"

	"github.com/rave-soft/braid/internal/oauth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// MCPTokenMutation reserves ownership of token persistence for one effective
// MCP server configuration. Reservations are process-local lifecycle epochs;
// they are deliberately not persisted.
type MCPTokenMutation struct {
	name          string
	epoch         uint64
	expected      MCPConfig
	expectedToken *oauth.Token
}

// ReserveMCPTokenMutation invalidates older token writers for name and returns
// a reservation only while the expected server is still enabled and unchanged.
func (s *ConfigStore) ReserveMCPTokenMutation(name string, expected MCPConfig) (MCPTokenMutation, bool) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	current, ok := s.Config().MCP[name]
	if !ok || current.Disabled || !sameMCPIdentity(current, expected) {
		return MCPTokenMutation{}, false
	}
	if s.mcpMutationEpochs == nil {
		s.mcpMutationEpochs = make(map[string]uint64)
	}
	s.mcpMutationEpochs[name]++
	return MCPTokenMutation{
		name:          name,
		epoch:         s.mcpMutationEpochs[name],
		expected:      withoutMCPToken(expected),
		expectedToken: expected.OAuthToken,
	}, true
}

// SetMCPToken conditionally persists and publishes a token if reservation is
// still the exact owner of the same enabled MCP configuration.
func (s *ConfigStore) SetMCPToken(reservation *MCPTokenMutation, token *oauth.Token) (bool, error) {
	return s.mutateMCPToken(reservation, nil, token, false)
}

// ClearMCPToken conditionally clears expectedToken. In addition to ownership,
// the current token must still be exactly the token the failing attempt used.
func (s *ConfigStore) ClearMCPToken(reservation *MCPTokenMutation, expectedToken *oauth.Token) (bool, error) {
	return s.mutateMCPToken(reservation, expectedToken, nil, true)
}

func (s *ConfigStore) mutateMCPToken(reservation *MCPTokenMutation, clearExpected, token *oauth.Token, clear bool) (bool, error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	expectedToken := reservation.expectedToken
	if clear && !reflect.DeepEqual(expectedToken, clearExpected) {
		return false, nil
	}
	current, ok := s.Config().MCP[reservation.name]
	if !ok || current.Disabled || s.mcpMutationEpochs[reservation.name] != reservation.epoch ||
		!sameMCPIdentity(current, reservation.expected) || !reflect.DeepEqual(current.OAuthToken, expectedToken) {
		return false, nil
	}

	mutated := false
	serverKey := fmt.Sprintf("mcp.%s", gjson.Escape(reservation.name))
	tokenKey := serverKey + ".oauth_token"
	err := s.atomicWrite(ScopeGlobal, func(data []byte) ([]byte, error) {
		server := gjson.GetBytes(data, serverKey)
		if !server.Exists() {
			return nil, errAtomicWriteNoop
		}
		var disk MCPConfig
		if err := json.Unmarshal([]byte(server.Raw), &disk); err != nil {
			return nil, fmt.Errorf("decode MCP config %s: %w", reservation.name, err)
		}
		if disk.Disabled || !sameMCPIdentity(disk, reservation.expected) ||
			!reflect.DeepEqual(disk.OAuthToken, expectedToken) {
			return nil, errAtomicWriteNoop
		}
		mutated = true
		if clear {
			value, err := sjson.DeleteBytes(data, tokenKey)
			return value, err
		}
		value, err := sjson.SetBytes(data, tokenKey, token)
		return value, err
	})
	if err != nil || !mutated {
		return false, err
	}

	next := s.Config().cloneForWrite()
	mcp := next.MCP[reservation.name]
	mcp.OAuthToken = token
	next.MCP[reservation.name] = mcp
	reservation.expectedToken = token
	s.setConfig(next)
	if path, pathErr := s.ConfigPath(ScopeGlobal); pathErr == nil {
		s.captureStalenessSnapshot(append(slices.Clone(s.loadedPaths), path))
	}
	return true, nil
}

func sameMCPIdentity(a, b MCPConfig) bool {
	return reflect.DeepEqual(withoutMCPToken(a), withoutMCPToken(b))
}

func withoutMCPToken(m MCPConfig) MCPConfig {
	m.OAuthToken = nil
	return m
}
