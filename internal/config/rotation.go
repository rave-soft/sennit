package config

import "github.com/rave-soft/sennit/internal/providers/accounts"

// ToPolicy converts r into the accounts package's rotation type. The two
// types stay deliberately distinct — RotationConfig is what a user writes
// in sennitrc/sennit.json, RotationPolicy is what accounts.Rotator
// consumes internally — so this conversion is a real boundary crossing,
// not two names for the same struct: a field renamed or reordered on one
// side fails to compile here instead of silently misrouting a value. Safe
// to call on a nil r; returns the zero RotationPolicy, which Rotator reads
// as "use every default".
func (r *RotationConfig) ToPolicy() accounts.RotationPolicy {
	if r == nil {
		return accounts.RotationPolicy{}
	}
	return accounts.RotationPolicy{
		MinRemainingPercent: r.MinRemainingPercent,
		Cooldown:            r.EffectiveCooldown(),
		Order:               r.Order,
	}
}
