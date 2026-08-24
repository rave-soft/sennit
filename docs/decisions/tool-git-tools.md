# T12 structured Git tools: measurement and safety decision

`git_status`, `git_diff`, and `git_log` provide bounded, structured Git reads.
They are read-only and remain in the parallel-safe tool classification, but each
invocation validates the current Git worktree and path scope before execution.

## Aggregate measurement

The expected saving is an **assumption**, not a measured product result. The
aggregate-only classification snapshot covers **2026-08-10 through 2026-08-24**:

```text
total Git shell calls: 7,328
chained calls:         5,713 (78%)
standalone calls:      1,615
ambiguous calls:           0
raw command totals: status 3,236; diff 3,151; log 941
```

The standalone value is the baseline population for this rollout. Its per-command
breakdown is unavailable, so the 1,615 classification is explicitly an assumption
rather than an attribution to status, diff, or log. The raw command totals reconcile
to 7,328; `chained + standalone + ambiguous` also reconciles to 7,328.

The snapshot records only aggregate normalized command-family counts. It does not
collect prompts, repository paths, arguments, session IDs, source content, command
output, or any other user data. Reproduce it by grouping normalized command-family
telemetry over the stated interval, classifying calls as chained or standalone, and
asserting the two reconciliation equations above. Measure rollout outcome again from
those same aggregate counts; task mix and adoption can change the result.

## Security and pagination

Arbitrary command arguments and options are rejected by typed parameters and
validated path/revision handling. Diff invocations explicitly disable external
diffs and text conversion (`--no-ext-diff --no-textconv`), preventing configured
helper execution. `GIT_CONFIG_COUNT=0` blocks environment-based config injection,
but does not suppress repository configuration; other benign repository config
continues to apply. Path filters are relative to the active working directory,
reject option-like, absolute, and escaping values, and are passed after `--`.
Cursors are signed, bound to normalized request parameters, and reject stale
status/log generations or changed patch bytes. No source data is written by these
tools.
