// Package testenv provides deterministic environment values for internal tests.
package testenv

import "github.com/rave-soft/braid/internal/env"

type mapEnv map[string]string

// New returns an Env backed by values supplied by the test.
func New(values map[string]string) env.Env {
	if values == nil {
		values = map[string]string{}
	}
	return mapEnv(values)
}

func (m mapEnv) Get(key string) string {
	return m[key]
}

func (m mapEnv) Env() []string {
	values := make([]string, 0, len(m))
	for key, value := range m {
		values = append(values, key+"="+value)
	}
	return values
}
