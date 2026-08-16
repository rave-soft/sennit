package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFormatPlanUsage(t *testing.T) {
	t.Parallel()

	in := func(d time.Duration) time.Time { return time.Now().Add(d) }

	tests := []struct {
		name    string
		plan    string
		windows []PlanWindow
		want    string
	}{
		{
			name:    "weekly window, as a plus plan reports it",
			plan:    "plus",
			windows: []PlanWindow{{UsedPercent: 6, WindowMinutes: 10080, ResetsAt: in(80 * time.Hour)}},
			want:    "Plus · 6% of weekly limit, resets in 3d",
		},
		{
			name: "the fullest window is the one worth showing",
			plan: "pro",
			windows: []PlanWindow{
				{UsedPercent: 12, WindowMinutes: 10080, ResetsAt: in(100 * time.Hour)},
				{UsedPercent: 80, WindowMinutes: 300, ResetsAt: in(90 * time.Minute)},
			},
			want: "Pro · 80% of 5h limit, resets in 1h",
		},
		{
			// Truncated, not rounded: a countdown that understates the
			// wait sends you back a little early, which is the harmless
			// direction to be wrong in.
			name:    "a reset under an hour is still worth a number",
			plan:    "business",
			windows: []PlanWindow{{UsedPercent: 99, WindowMinutes: 60, ResetsAt: in(4*time.Minute + 30*time.Second)}},
			want:    "Business · 99% of hourly limit, resets in 4m",
		},
		{
			name:    "no reset time, no invented one",
			plan:    "plus",
			windows: []PlanWindow{{UsedPercent: 20, WindowMinutes: 1440}},
			want:    "Plus · 20% of daily limit",
		},
		{
			name:    "a plan with no windows is still worth naming",
			plan:    "plus",
			windows: nil,
			want:    "Plus",
		},
		{
			name:    "a reset already past is not reported as negative",
			plan:    "plus",
			windows: []PlanWindow{{UsedPercent: 5, WindowMinutes: 10080, ResetsAt: in(-time.Hour)}},
			want:    "Plus · 5% of weekly limit",
		},
		{
			name:    "nothing known, nothing rendered",
			plan:    "",
			windows: nil,
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, FormatPlanUsage(tt.plan, tt.windows))
		})
	}
}
