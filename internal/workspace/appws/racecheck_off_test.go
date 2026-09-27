//go:build !race

package appws

// raceDetectorEnabled mirrors the `race` build constraint the go command
// sets automatically under `-race` -- see
// internal/workspace/wsrpc/grpcws/racecheck_off_test.go for the same
// pattern. This package's tests exercise real goroutine hand-offs
// (AgentRunStream fan-in, dispatcher joins), so their hang-guard budgets
// need real wall-clock headroom under -race's instrumentation overhead
// and CI's cross-package CPU contention (AGENTS.md's "wall-clock budgets
// under -race") -- not to assert performance, only to turn an actual
// hang into a failure.
const raceDetectorEnabled = false
