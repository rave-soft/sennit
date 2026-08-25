package shellconfig_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/shellconfig"
)

// TestUIOptionSpinnerMatchesConfig holds `option ui spinner`'s accepted
// values to the ones the config actually understands.
//
// This package cannot import config — config imports it, to run sennitrc —
// so the enum in uiOptionSpecs is a hand-copied list. An external test
// package has no such restriction, which is why this file is
// shellconfig_test rather than shellconfig: it is the only place the two
// lists can be compared at all.
func TestUIOptionSpinnerMatchesConfig(t *testing.T) {
	t.Parallel()

	for _, mode := range config.SpinnerModes {
		out, err := shellconfig.LoadShellConfig(t.Context(), "sennitrc", []byte("option ui spinner "+mode+"\n"))
		require.NoError(t, err, "sennitrc must accept the configured mode %q", mode)
		require.Contains(t, string(out), mode)
	}

	_, err := shellconfig.LoadShellConfig(t.Context(), "sennitrc", []byte("option ui spinner disco\n"))
	require.Error(t, err, "a mode config does not know must be refused at parse time")
}
