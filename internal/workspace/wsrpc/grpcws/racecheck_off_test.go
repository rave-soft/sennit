//go:build !race

package grpcws_test

// raceDetectorEnabled mirrors internal/ui/model/racecheck_{on,off}_test.go:
// the `race` build constraint the go command sets automatically under
// `-race`. The lease tests use it to widen their grace period instead of
// asserting a fixed one -- the race detector's instrumentation overhead can
// otherwise make a reconnect that would easily beat a short grace arrive
// late, which would be a false failure about scheduling, not about the
// lease logic itself (see AGENTS.md's note on wall-clock budgets under
// -race).
const raceDetectorEnabled = false
