package workspace

import (
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/shell"
)

// This file holds the contract's own names for backend values the TUI
// renders, so internal/ui does not import internal/shell or
// internal/providers/accounts just to spell a type. Same reasoning as
// LSPClientInfo and MCPPrompt: what crosses this boundary is data, and the
// packages that produce it should not be in the UI's dependency graph.
// See TestDomainPackageDoesNotDependOnAgentTransitively for the guard that
// enforces the shape of that graph.

// BackgroundJobCounts summarizes the background shell jobs a workspace is
// running. Mirrors shell.BackgroundJobCounts.
type BackgroundJobCounts struct {
	Active    int
	Completed int
}

// MaxBackgroundJobs is the cap the sidebar renders against.
const MaxBackgroundJobs = shell.MaxBackgroundJobs

// Usage and UsageWindow are the provider's rate-limit snapshot. These are
// true aliases rather than copies: CurrentPlanUsage already returns
// accounts.Usage, and the values are plain data with no behavior beyond
// UsageWindow.Known, so an alias keeps the UI off the accounts import
// without a conversion nobody would benefit from.
type (
	Usage       = accounts.Usage
	UsageWindow = accounts.UsageWindow
)

// RotateOn names when a provider rotates to another account, if at all.
type RotateOn int

const (
	// RotateNever means the provider does not rotate accounts.
	RotateNever RotateOn = iota
	// RotateThreshold rotates once remaining allowance drops below a
	// configured percentage.
	RotateThreshold
	// RotateRateLimit rotates when the provider reports a rate limit, and
	// treats the account as cooling down for a configured period.
	RotateRateLimit
)

// AccountCapabilities describes what a provider's accounts support, as far
// as the settings dialog needs to know. Mirrors the parts of
// accounts.Capabilities the UI renders; the auth-kind field is
// deliberately not carried, since nothing in the UI reads it.
type AccountCapabilities struct {
	// Usage reports whether the provider quotes a rate-limit snapshot.
	Usage bool
	// RotateOn is the provider's rotation trigger.
	RotateOn RotateOn
}

// DefaultMinRemainingPercent and DefaultCooldown are the defaults the
// settings dialog pre-fills for a rotating provider.
const (
	DefaultMinRemainingPercent = accounts.DefaultMinRemainingPercent
	DefaultCooldown            = accounts.DefaultCooldown
)
