package accounts

import "time"

// DefaultMinRemainingPercent is the remaining-allowance threshold a
// RotateThreshold provider rotates at when the user hasn't set one of
// their own: "remaining drops below 10%" (i.e. used_percent >= 90).
//
// This lives here, not in internal/config alongside the field it defaults,
// so the UI (which offers this value as a placeholder), config validation
// (which fills it in for the effective-threshold computation), and the
// rotator (phase 5, which will actually compare usage against it) all read
// the same constant instead of three copies drifting apart.
const DefaultMinRemainingPercent = 10

// DefaultCooldown is how long a RotateRateLimit account is treated as
// exhausted after a 429 with no Retry-After header, when the user hasn't
// configured a cooldown of their own.
const DefaultCooldown = 10 * time.Minute
