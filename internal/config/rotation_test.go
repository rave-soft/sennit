package config

import (
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/providers/accounts"
)

func TestRotationConfig_ToPolicy(t *testing.T) {
	tests := []struct {
		name string
		cfg  *RotationConfig
		want accounts.RotationPolicy
	}{
		{
			// A nil receiver returns the zero RotationPolicy, not
			// DefaultCooldown spelled out: Rotator itself treats a
			// zero Cooldown as "use the default" (see its cooldown()
			// method), so ToPolicy doesn't need to duplicate that.
			name: "nil config returns the zero policy",
			cfg:  nil,
			want: accounts.RotationPolicy{},
		},
		{
			// EffectiveCooldown, unlike the raw field, always resolves
			// to a concrete duration - that's the one case where
			// ToPolicy's output differs from a bare field copy.
			name: "zero-value config resolves cooldown to the default",
			cfg:  &RotationConfig{},
			want: accounts.RotationPolicy{Cooldown: accounts.DefaultCooldown},
		},
		{
			name: "fields carry through",
			cfg: &RotationConfig{
				Enabled:             true,
				MinRemainingPercent: 20,
				Cooldown:            "5m",
				Order:               []string{"work", "personal"},
			},
			want: accounts.RotationPolicy{
				MinRemainingPercent: 20,
				Cooldown:            5 * time.Minute,
				Order:               []string{"work", "personal"},
			},
		},
		{
			name: "unparseable cooldown falls back to the default",
			cfg:  &RotationConfig{Cooldown: "not-a-duration"},
			want: accounts.RotationPolicy{Cooldown: accounts.DefaultCooldown},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.ToPolicy()
			if got.MinRemainingPercent != tt.want.MinRemainingPercent {
				t.Errorf("MinRemainingPercent = %d, want %d", got.MinRemainingPercent, tt.want.MinRemainingPercent)
			}
			if got.Cooldown != tt.want.Cooldown {
				t.Errorf("Cooldown = %v, want %v", got.Cooldown, tt.want.Cooldown)
			}
			if len(got.Order) != len(tt.want.Order) {
				t.Fatalf("Order = %v, want %v", got.Order, tt.want.Order)
			}
			for i := range got.Order {
				if got.Order[i] != tt.want.Order[i] {
					t.Errorf("Order[%d] = %q, want %q", i, got.Order[i], tt.want.Order[i])
				}
			}
		})
	}
}
