package accounts

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestUsage_JSONRoundTripNamesTheSameInstant pins Usage/UsageWindow's
// Unix-seconds codec: MarshalJSON writes CapturedAt/ResetsAt as Unix
// seconds, and UnmarshalJSON reads them back with time.Unix, which
// returns a time.Time in the decoding process's local Location - not
// UTC. That is deliberate, not a bug: these times are formatted for a
// person with a clock layout (see rotator.go's ErrAllExhausted.Error and
// internal/agent/provider_limit.go, both `ResetsAt.Format("15:04")`), and
// on a remote client the decoding process's local zone is the person's
// own zone, where the reset time should read as their local wall clock.
// So the only thing a round trip has to preserve is the instant, not the
// Location - this pins that with time.Time.Equal rather than
// require.Equal, which would also compare Location and fail for the
// wrong reason.
func TestUsage_JSONRoundTripNamesTheSameInstant(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("Asia/Yekaterinburg") // UTC+5, unlikely to be the test runner's own zone
	require.NoError(t, err)

	want := Usage{
		Plan: "pro",
		Primary: UsageWindow{
			UsedPercent:   42,
			WindowMinutes: 300,
			ResetsAt:      time.Date(2026, 3, 4, 15, 6, 7, 0, loc),
		},
		CapturedAt: time.Date(2026, 3, 4, 15, 6, 7, 0, loc),
	}

	data, err := json.Marshal(want)
	require.NoError(t, err)

	var got Usage
	require.NoError(t, json.Unmarshal(data, &got))

	require.True(t, want.Primary.ResetsAt.Equal(got.Primary.ResetsAt), "same instant")
	require.True(t, want.CapturedAt.Equal(got.CapturedAt), "same instant")
}
