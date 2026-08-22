//go:build !race

package model

// raceDetectorEnabled mirrors the `race` build constraint the go command
// sets automatically under `-race` (see racecheck_on_test.go). Wall-clock
// timing assertions use it to skip themselves under the race detector,
// whose 2-20x instrumentation overhead measures the detector, not the
// code — see TestSessionPanelDrawFrameBudget.
const raceDetectorEnabled = false
