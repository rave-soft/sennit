#!/bin/bash
# Report functions that nothing can reach — not from main, not from a test.
#
# Why `-test` and not a plain `deadcode ./...`: without it the analyzer only
# asks "is this reachable from main", which on this tree answers "no" for 85
# symbols that are perfectly alive and covered by tests (test helpers,
# constructors a test drives directly, methods whose only callers are
# _test.go). A gate on that number would be noise. With `-test` the test
# binaries become roots too, so a finding means something stronger: nobody
# at all — no production path, no test — can call this.
#
# A finding is a reason to read the code, never a delete list. The same
# review that asked for this gate sent six delegation_finalizer methods to
# be deleted as "dead" when one of them had 19 callers in tests; deadcode
# had answered a different question than the one being asked. Each new name
# here is one of three things, and only reading tells them apart: a function
# that lost its call site in a refactor (delete it), a whole capability that
# was built and never wired up (connect it), or a helper kept on purpose
# (add it to the allow-list below, with the reason).
set -uo pipefail

# Pinned, not @latest, for the same reason golangci-lint is pinned in
# .github/workflows/build.yml: @latest silently swaps the analyzer, and the
# same commit starts failing with no diff of ours to blame. v0.48.0 is the
# golang.org/x/tools this module already requires (see go.mod), so the
# analyzer and the packages it loads come from one version.
DEADCODE_VERSION="v0.48.0"

cd "$(dirname "$0")/.." || exit 1
allowlist="scripts/deadcode_allowlist.txt"

output=$(go run "golang.org/x/tools/cmd/deadcode@${DEADCODE_VERSION}" -test ./...)
if [ $? -ne 0 ]; then
  echo "❌ deadcode failed to run (see the error above)."
  exit 1
fi

# Key findings by file and symbol, dropping line:col — those shift with
# every edit above them and would make the allow-list rot for no reason.
found=$(echo "$output" |
  sed -nE 's|^([^:]+):[0-9]+:[0-9]+: unreachable func: (.*)$|\1 \2|p' |
  sort -u)
allowed=$(grep -vE '^\s*(#|$)' "$allowlist" | sort -u)

new=$(comm -23 <(echo "$found") <(echo "$allowed"))
if [ -n "$new" ]; then
  echo "❌ New unreachable functions — nothing in the tree, tests included,"
  echo "   can call these:"
  echo
  echo "$new" | sed 's/^/   /'
  echo
  echo "   Read each one before acting. If it lost its call site, delete it."
  echo "   If it is a capability that was never wired up, wire it up. If it"
  echo "   is kept on purpose, add it to $allowlist with the reason."
  exit 1
fi

stale=$(comm -13 <(echo "$found") <(echo "$allowed"))
if [ -n "$stale" ]; then
  echo "ℹ️  Allow-list entries that are no longer unreachable — drop them from"
  echo "   $allowlist so it keeps describing the tree:"
  echo "$stale" | sed 's/^/   /'
fi
