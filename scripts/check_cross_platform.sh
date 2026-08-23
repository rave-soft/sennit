#!/bin/bash
# Type-check the whole tree, tests included, for the platforms CI builds.
#
# `go build` alone does not compile _test.go files, and `go vet` on the host
# only ever sees the host's build tags — so a test that calls syscall.Kill or
# hardcodes a POSIX path compiles fine here and fails on the Windows runner.
# That happened twice in one review: internal/permission asserted on "/tmp"
# (not an absolute path on Windows, so path canonicalisation joined it onto
# the workspace), and two tests used syscall.Kill/syscall.Getrusage with no
# build constraint. Both would have been caught here in seconds.
set -uo pipefail

status=0
for goos in windows darwin linux; do
  echo "==> GOOS=$goos go vet ./..."
  if ! GOOS="$goos" go vet ./... ; then
    status=1
  fi
done

if [ "$status" -ne 0 ]; then
  echo
  echo "❌ Cross-platform vet failed. A test that only compiles on one OS needs"
  echo "   a //go:build constraint (and a _unix_test.go / _windows_test.go name),"
  echo "   and a path assertion needs t.TempDir() rather than a POSIX literal."
fi
exit "$status"
