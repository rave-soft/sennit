package testenv

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	values := map[string]string{
		"EMPTY": "",
		"VALUE": "value=with=equals",
	}

	env := New(values)
	require.Equal(t, "value=with=equals", env.Get("VALUE"))
	require.Empty(t, env.Get("MISSING"))

	got := make(map[string]string)
	for _, value := range env.Env() {
		key, value, ok := strings.Cut(value, "=")
		require.True(t, ok)
		got[key] = value
	}
	require.Equal(t, values, got)
}

func TestNewNil(t *testing.T) {
	env := New(nil)
	require.Empty(t, env.Get("MISSING"))
	require.Empty(t, env.Env())
}
