package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"reflect"

	"github.com/rave-soft/sennit/internal/oauth"
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
	ctx, cancel := configWriteContext()
	defer cancel()
	return s.SetMCPTokenContext(ctx, reservation, token)
}

func (s *ConfigStore) SetMCPTokenContext(ctx context.Context, reservation *MCPTokenMutation, token *oauth.Token) (bool, error) {
	return s.mutateMCPToken(ctx, reservation, nil, token, false)
}

// ClearMCPToken conditionally clears expectedToken. In addition to ownership,
// the current token must still be exactly the token the failing attempt used.
func (s *ConfigStore) ClearMCPToken(reservation *MCPTokenMutation, expectedToken *oauth.Token) (bool, error) {
	ctx, cancel := configWriteContext()
	defer cancel()
	return s.mutateMCPToken(ctx, reservation, expectedToken, nil, true)
}

func (s *ConfigStore) mutateMCPToken(ctx context.Context, reservation *MCPTokenMutation, clearExpected, token *oauth.Token, clear bool) (bool, error) {
	if err := lockMutexContext(ctx, &s.reloadMu); err != nil {
		return false, err
	}
	defer s.reloadMu.Unlock()
	if err := lockMutexContext(ctx, &s.writeMu); err != nil {
		return false, err
	}
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

	serverKey := fmt.Sprintf("mcp.%s", gjson.Escape(reservation.name))
	tokenKey := serverKey + ".oauth_token"

	scope := s.mcpTokenScope(serverKey)
	if _, pathErr := s.ConfigPath(scope); pathErr != nil {
		// No file to target for the chosen scope (e.g. no workspace config
		// path configured). This is a skip, not a hard failure: it keeps
		// the same (false, nil) contract as every other guard above, and
		// still says why the token was not persisted.
		slog.Warn("Skipped persisting MCP OAuth token: no writable config scope",
			"server", reservation.name, "scope", scope, "error", pathErr)
		return false, nil
	}

	mutated := false
	skipReason := ""
	// The write and the staleness-snapshot refresh happen under one
	// fileStaleness mutex section, exactly as SetConfigFields and
	// updateLockedErr do, so a concurrent ConfigStaleness()/watcher poll can
	// never land between the on-disk write and the refresh and mistake this
	// process's own token write for an external change. Before this fix the
	// refresh ran after setConfig below, using
	// CaptureStalenessSnapshot(loadedPaths+path) — narrowing the tracked set
	// to only the files that loaded, and doing so well after the write had
	// already landed on disk, wide open to that race. addAndRefreshLocked
	// instead restats every path Load/reloadFromDisk already tracked
	// (including global layers absent on disk) and only adds the scope's own
	// path if it is somehow not already a member.
	s.staleness.mu.Lock()
	err := s.atomicWriteContext(ctx, scope, func(data []byte) ([]byte, error) {
		if scope == ScopeGlobal {
			// The full declaration is expected to live in this file, so the
			// identity guard (command/url/etc. unchanged since reservation)
			// still applies here, mirroring the in-memory checks above.
			server := gjson.GetBytes(data, serverKey)
			if !server.Exists() {
				skipReason = "server declaration no longer present in the global config"
				return nil, errAtomicWriteNoop
			}
			var disk MCPConfig
			if err := json.Unmarshal([]byte(server.Raw), &disk); err != nil {
				return nil, fmt.Errorf("decode MCP config %s: %w", reservation.name, err)
			}
			if disk.Disabled || !sameMCPIdentity(disk, reservation.expected) ||
				!reflect.DeepEqual(disk.OAuthToken, expectedToken) {
				skipReason = "on-disk MCP config changed since reservation"
				return nil, errAtomicWriteNoop
			}
			mutated = true
			if clear {
				return sjson.DeleteBytes(data, tokenKey)
			}
			return sjson.SetBytes(data, tokenKey, token)
		}

		// ScopeWorkspace: the server may be declared only in a
		// project-scoped config, so this file can hold nothing but a
		// token-only overlay (mcp.<name>.oauth_token) with no identity
		// fields to compare against. sameMCPIdentity cannot apply here;
		// the epoch/ownership/token checks already taken above under
		// writeMu are what guards this write, plus comparing against
		// whatever token the overlay already holds.
		diskToken := gjson.GetBytes(data, tokenKey)
		var current *oauth.Token
		if diskToken.Exists() {
			current = &oauth.Token{}
			if err := json.Unmarshal([]byte(diskToken.Raw), current); err != nil {
				return nil, fmt.Errorf("decode MCP token %s: %w", reservation.name, err)
			}
		}
		if !reflect.DeepEqual(current, expectedToken) {
			skipReason = "on-disk MCP token overlay changed since reservation"
			return nil, errAtomicWriteNoop
		}
		mutated = true
		if clear {
			return sjson.DeleteBytes(data, tokenKey)
		}
		return sjson.SetBytes(data, tokenKey, token)
	})
	if err == nil && mutated {
		path, pathErr := s.ConfigPath(scope)
		if pathErr != nil {
			path = ""
		}
		s.staleness.addAndRefreshLocked(path)
	}
	s.staleness.mu.Unlock()
	if err != nil || !mutated {
		if err == nil {
			slog.Warn("Skipped persisting MCP OAuth token", "server", reservation.name, "scope", scope, "reason", skipReason)
		}
		return false, err
	}

	next := s.Config().cloneForWrite()
	mcp := next.MCP[reservation.name]
	mcp.OAuthToken = token
	next.MCP[reservation.name] = mcp
	reservation.expectedToken = token
	s.setConfig(next)
	return true, nil
}

// mcpTokenScope picks the scope a token write for serverKey (a
// "mcp.<name>" gjson path) should target: ScopeGlobal when the server is
// declared there, ScopeWorkspace otherwise. A server declared only in a
// project-scoped config (./sennit.json, .sennit/sennit.json, a sennitrc)
// has no entry in the global data file, and the workspace config is the
// only other file Sennit is allowed to write — so the token is persisted
// there as a token-only overlay (mcp.<name>.oauth_token), which the merge
// pipeline layers onto the project's declaration on the next load.
//
// Caller must hold writeMu (read is best-effort: atomicWrite re-verifies
// the target file's contents once the scope is chosen).
func (s *ConfigStore) mcpTokenScope(serverKey string) Scope {
	if path, err := s.ConfigPath(ScopeGlobal); err == nil {
		if data, err := os.ReadFile(path); err == nil && gjson.GetBytes(data, serverKey).Exists() {
			return ScopeGlobal
		}
	}
	return ScopeWorkspace
}

func sameMCPIdentity(a, b MCPConfig) bool {
	return reflect.DeepEqual(withoutMCPToken(a), withoutMCPToken(b))
}

func withoutMCPToken(m MCPConfig) MCPConfig {
	m.OAuthToken = nil
	return m
}
