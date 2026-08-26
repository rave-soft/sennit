package codex

import "github.com/rave-soft/sennit/internal/providers/accounts"

// Snapshot converts the usage this package parses off Codex responses into
// the provider-neutral shape the account store persists.
//
// The dependency runs codex -> accounts, never the other way: accounts is a
// leaf package (see its doc comment) that must stay ignorant of any
// specific provider, while codex is exactly the kind of provider-specific
// package meant to depend on shared types instead of duplicating them.
func (u Usage) Snapshot() accounts.Usage {
	return accounts.Usage{
		Plan: u.Plan,
		Primary: accounts.UsageWindow{
			UsedPercent:   u.Primary.UsedPercent,
			WindowMinutes: u.Primary.WindowMinutes,
			ResetsAt:      u.Primary.ResetsAt,
		},
		Secondary: accounts.UsageWindow{
			UsedPercent:   u.Secondary.UsedPercent,
			WindowMinutes: u.Secondary.WindowMinutes,
			ResetsAt:      u.Secondary.ResetsAt,
		},
		CapturedAt: u.CapturedAt,
	}
}
