package mcp

import (
	"github.com/rave-soft/sennit/internal/config"
)

// reinitAction describes how to reconcile one MCP server against the
// current config.
type reinitAction int

const (
	reinitDisable reinitAction = iota + 1
	reinitRemove
	reinitStart
)

// reconcile diffs running MCP state against the current config and returns
// the action to take for each server. It is a pure function: all state is
// passed in, nothing global is read or mutated, so the reconciliation
// decision can be tested with plain maps.
//
// The config a server last connected with lives on its ClientInfo (Config),
// and the config an in-flight attempt is connecting with lives on
// PendingConfig. Reconcile compares the live config against whichever is
// relevant for the server's state:
//
//   - A starting server is left alone only while it is connecting with the
//     current config. If the config changed since it started, it restarts so
//     the new config takes effect instead of being lost to the in-flight
//     attempt.
//   - A connected server restarts when its config differs from the one it
//     connected with.
//   - Every other state (new, errored, needs-auth, disabled) restarts: new
//     and disabled servers carry no config, and retrying a failed server on
//     each config write is the desired recovery path.
//
// Servers gone from config are removed entirely; enabled-in-config servers
// marked disabled are disabled.
func reconcile(current config.MCPs, running map[string]ClientInfo) map[string]reinitAction {
	actions := map[string]reinitAction{}

	// Servers no longer in config are removed entirely.
	for name := range running {
		if _, exists := current[name]; !exists {
			actions[name] = reinitRemove
		}
	}

	for name, m := range current {
		info, exists := running[name]
		if m.Disabled {
			if exists && info.State != StateDisabled {
				actions[name] = reinitDisable
			}
			continue
		}

		if exists {
			switch info.State {
			case StateStarting:
				// Restart only if the config changed since this attempt
				// started; otherwise let the in-flight attempt settle so
				// rapid writes don't pile up overlapping init goroutines.
				if info.PendingConfig != nil && mcpConfigEqual(*info.PendingConfig, m) {
					continue
				}
			case StateConnected:
				if mcpConfigEqual(info.Config, m) {
					continue
				}
			}
		}
		actions[name] = reinitStart
	}

	return actions
}
