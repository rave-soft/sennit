//go:build !race

package e2e

// raceDetectorEnabled mirrors the `race` build constraint the go command
// sets automatically under `-race` (see internal/daemon/racecheck_off_test.go
// for the same pattern this package copies).
//
// This whole package spawns the real sennit binary as a subprocess and
// builds it fresh in TestMain -- that subprocess is never itself built
// with -race, since GOFLAGS/-race apply only to the test binary that
// `go test -race` produces, not to a `go build` this package's own
// TestMain shells out to separately. Running these scenarios under the
// race job would therefore add real wall time (a fresh build plus six-ish
// subprocess scenarios) while exercising the race detector on none of the
// daemon or agent code the subprocess actually runs -- only on this
// package's own harness goroutines, which is not where PR 2.1-2.4's
// concurrency risk lives (that's internal/daemon, internal/workspace/
// wsrpc/grpcws and internal/app, all covered directly by their own
// -race-run unit and integration suites). So every test here checks this
// constant first and skips under -race; TestMain skips the binary build
// entirely for the same reason.
const raceDetectorEnabled = false
