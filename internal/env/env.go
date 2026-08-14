package env

import (
	"os"
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
