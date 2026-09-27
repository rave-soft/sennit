//go:build !race

package grpcws

// raceDetectorEnabledInternal mirrors raceDetectorEnabled in
// racecheck_{on,off}_test.go (package grpcws_test) for this package's own
// internal (grpcws) tests, which cannot see that package's unexported
// const.
const raceDetectorEnabledInternal = false
