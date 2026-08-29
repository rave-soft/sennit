#!/bin/bash
# Refuse a new unbounded output accumulator in internal/shell.
#
# The bug this exists to stop coming back: syncBuffer used to be a plain
# bytes.Buffer behind a mutex, so a background job's stdout grew for as long
# as the command ran — `tail -f` or a chatty build ate the process's memory,
# and nothing capped it. The fix (6c7504ebd) was a 256 KiB head plus a
# 256 KiB tail ring: the caller still sees the start and the most recent
# output, and one stream can never cost more than 512 KiB.
#
# The shape that reintroduces it is specific, so the check is too: a struct
# in this package that holds a bytes.Buffer/strings.Builder *field* and
# exposes it through String()/Bytes()/Len(). A field lives as long as its
# struct — that is what makes it an accumulator — while the local
# bytes.Buffers all over this package are scoped to one command and are
# fine, so they are deliberately not matched.
#
# Not expressible through golangci-lint's forbidigo: forbidigo matches
# identifiers at a use site, so the only rule it could state is "no
# bytes.Buffer in internal/shell", which the correct local buffers here
# would trip immediately.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

# Candidates: "file:line:type:field" for every accumulator-shaped field.
candidates=$(
  for f in internal/shell/*.go; do
    case "$f" in *_test.go) continue ;; esac
    awk -v file="$f" '
      # An opt-out is a marker with a reason after it, either trailing the
      # field or anywhere in the comment block directly above it:
      # "// bounded-buffer: <why this one cannot grow>". Tracked by keeping
      # it alive across a comment block and the one declaration that
      # follows, then dropping it.
      {
        iscomment = ($0 ~ /^[[:space:]]*\/\//)
        if (!iscomment && !prevcomment) marker = 0
        if (iscomment && !prevcomment) marker = 0
        if ($0 ~ /bounded-buffer:[[:space:]]*[^[:space:]]/) marker = 1
        prevcomment = iscomment
      }
      /^type[[:space:]]+[A-Za-z_][A-Za-z0-9_]*[[:space:]]+struct[[:space:]]*\{/ {
        split($0, t, /[[:space:]]+/); typ = t[2]; next
      }
      typ != "" && /^\}/ { typ = ""; next }
      typ != "" && marker == 0 && $0 ~ /^[[:space:]]+[A-Za-z_][A-Za-z0-9_]*[[:space:]]+\*?(bytes\.Buffer|strings\.Builder)([[:space:]]|$)/ {
        split($0, f2, /[[:space:]]+/); field = f2[2]
        print file ":" NR ":" typ ":" field
      }
    ' "$f"
  done
)

status=0
while IFS=: read -r file line typ field; do
  [ -n "${typ:-}" ] || continue
  # Only an accumulator if the value is read back out whole; a write-only
  # field (a scratch encoder, say) is not the pattern we are guarding.
  if ! grep -qE "^func \([a-zA-Z_][a-zA-Z0-9_]* \*?${typ}\) (String|Bytes|Len)\(" internal/shell/*.go; then
    continue
  fi
  if [ "$status" -eq 0 ]; then
    echo "❌ Unbounded output accumulator in internal/shell:"
    echo
  fi
  echo "   $file:$line: $typ.$field is a $(sed -n "${line}p" "$file" | grep -oE '(bytes\.Buffer|strings\.Builder)') read back via String()/Bytes()/Len()"
  status=1
done <<<"$candidates"

if [ "$status" -ne 0 ]; then
  echo
  echo "   A buffer that lives as long as its struct grows as long as the"
  echo "   command writes to it: a background job streaming output takes the"
  echo "   process's memory with it. Bound it the way syncBuffer does — keep"
  echo "   the first MaxSyncBufferHead bytes and the last MaxSyncBufferTail"
  echo "   in a ring, and mark what was dropped (internal/shell/background.go)."
  echo "   Reuse syncBuffer itself where the shape fits."
  echo
  echo "   If this particular buffer genuinely cannot grow, say why on or"
  echo "   above the field: // bounded-buffer: <reason>."
fi
exit "$status"
