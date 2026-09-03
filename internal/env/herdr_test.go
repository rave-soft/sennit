package env

import (
	"slices"
	"strings"
	"testing"
)

func TestWithoutHerdrEnv_StripsAllVars(t *testing.T) {
	t.Parallel()
	env := []string{
		"HERDR_ENV=1",
		"HERDR_SOCKET_PATH=/tmp/herdr.sock",
		"HERDR_PANE_ID=wA:p1",
		"PATH=/usr/bin",
		"HOME=/home/user",
	}
	result := WithoutHerdrEnv(env)
	for _, e := range result {
		if strings.HasPrefix(e, "HERDR_") {
			t.Errorf("herdr var not stripped: %s", e)
		}
	}
	if !slices.Contains(result, "PATH=/usr/bin") {
		t.Error("non-herdr var PATH was incorrectly removed")
	}
	if !slices.Contains(result, "HOME=/home/user") {
		t.Error("non-herdr var HOME was incorrectly removed")
	}
}

func TestWithoutHerdrEnv_EmptyInput(t *testing.T) {
	t.Parallel()
	result := WithoutHerdrEnv(nil)
	if len(result) != 0 {
		t.Errorf("expected empty result for nil input, got %v", result)
	}
}

func TestWithoutHerdrEnv_SliceIndependence(t *testing.T) {
	t.Parallel()
	env := []string{"HERDR_ENV=1", "FOO=bar"}
	result := WithoutHerdrEnv(env)
	env[1] = "FOO=baz"
	for _, e := range result {
		if e == "FOO=baz" {
			t.Error("result shares backing array with input")
		}
	}
}
