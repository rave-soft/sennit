package config

import "testing"

func TestDaemonOptions_EffectiveMode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts *DaemonOptions
		want string
	}{
		{"nil", nil, "off"},
		{"unset", &DaemonOptions{}, "off"},
		{"off", &DaemonOptions{Mode: "off"}, "off"},
		{"auto", &DaemonOptions{Mode: "auto"}, "auto"},
		{"unrecognized falls back to off", &DaemonOptions{Mode: "bogus"}, "off"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.opts.EffectiveMode(); got != tc.want {
				t.Errorf("EffectiveMode() = %q, want %q", got, tc.want)
			}
		})
	}
}
