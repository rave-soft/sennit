package env

import (
	"os"
	"slices"
	"strings"
)

type Env interface {
	Get(key string) string
	Env() []string
}

type osEnv struct{}

// Get implements Env.
func (o *osEnv) Get(key string) string {
	return os.Getenv(key)
}

func (o *osEnv) Env() []string {
	return os.Environ()
}

func New() Env {
	return &osEnv{}
}

func Snapshot(base []string, overrides map[string]string) Env {
	values := make(map[string]string, len(base)+len(overrides))
	for _, value := range base {
		key, value, ok := strings.Cut(value, "=")
		if ok {
			values[key] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	return snapshot(values)
}

func Overlay(base Env, overrides map[string]string) Env {
	values := make(map[string]string, len(overrides))
	for key, value := range overrides {
		values[key] = value
	}
	return overlay{base: base, overrides: values}
}

type overlay struct {
	base      Env
	overrides map[string]string
}

func (o overlay) Get(key string) string {
	if value, ok := o.overrides[key]; ok {
		return value
	}
	return o.base.Get(key)
}

func (o overlay) Env() []string {
	values := make(map[string]string)
	for _, value := range o.base.Env() {
		key, value, ok := strings.Cut(value, "=")
		if ok {
			values[key] = value
		}
	}
	for key, value := range o.overrides {
		values[key] = value
	}
	return snapshot(values).Env()
}

type snapshot map[string]string

func (s snapshot) Get(key string) string {
	return s[key]
}

func (s snapshot) Env() []string {
	keys := make([]string, 0, len(s))
	for key := range s {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, key+"="+s[key])
	}
	return values
}
