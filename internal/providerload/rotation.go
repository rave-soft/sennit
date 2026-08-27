package providerload

import (
	"fmt"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/providers/accounts"
)

// validateRotationConfigs checks every provider's rotation settings
// against what its accounts.Capabilities actually support, and against
// the plain range/format constraints on the fields themselves. A bad
// setting is reported as a config.Problem and the offending field is
// cleared — never dropped along with the whole provider: a rotation typo
// must not lock a user out of an otherwise-working provider.
func (l *Loader) validateRotationConfigs(cfg *config.Config) {
	for id, providerConfig := range cfg.Providers.Seq2() {
		if providerConfig.Rotation == nil {
			continue
		}
		caps := accounts.CapabilitiesOf(id)
		// Work on a copy: providerConfig.Rotation is a pointer, and
		// cfg.Providers.Seq2() hands back the same one every other
		// reader of this config snapshot may also be holding — mutating
		// it in place would be an invisible side effect on callers that
		// have no idea this validation pass ran.
		rotation := *providerConfig.Rotation
		changed := false

		if rotation.MinRemainingPercent != 0 {
			switch {
			case caps.RotateOn != accounts.RotateThreshold:
				cfg.AddRuntimeProblem(providerProblem(id, fmt.Sprintf(
					"provider %s does not report how much of its limit is left; rotation for it triggers on HTTP 429, not a remaining-allowance threshold",
					id), "remove rotation.min_remaining_percent for this provider"))
				rotation.MinRemainingPercent = 0
				changed = true
			case rotation.MinRemainingPercent < 1 || rotation.MinRemainingPercent > 99:
				cfg.AddRuntimeProblem(providerProblem(id, fmt.Sprintf(
					"provider %s rotation.min_remaining_percent must be between 1 and 99", id),
					"static check only; fix the value and reload"))
				rotation.MinRemainingPercent = 0
				changed = true
			}
		}

		if rotation.Cooldown != "" {
			switch d, err := time.ParseDuration(rotation.Cooldown); {
			case caps.RotateOn != accounts.RotateRateLimit:
				cfg.AddRuntimeProblem(providerProblem(id, fmt.Sprintf(
					"provider %s reports how much of its limit is left; rotation for it triggers on that threshold, not a cooldown",
					id), "remove rotation.cooldown for this provider"))
				rotation.Cooldown = ""
				changed = true
			case err != nil || d <= 0:
				cfg.AddRuntimeProblem(providerProblem(id, fmt.Sprintf(
					"provider %s rotation.cooldown must be a positive duration (e.g. \"10m\")", id),
					"static check only; fix the value and reload"))
				rotation.Cooldown = ""
				changed = true
			}
		}

		if changed {
			providerConfig.Rotation = &rotation
			cfg.Providers.Set(id, providerConfig)
		}
	}
}
