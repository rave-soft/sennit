# `multi_read` measurement note

`multi_read` combines independent text-file reads in one sequential tool call.
It is intentionally **not** in the T0 parallel-tool allow-list: `read` updates
session file-read coverage, and preserving that state requires ordered execution.

## Aggregate baseline and expected outcome

The original seven-day snapshot for this project contained 5,372 tool calls,
including 1,617 `read` calls. Of those reads, 845 occurred in the 258 assistant
messages that contained at least two reads; 252 such messages referenced more
than one path. Replacing every message's reads with one batch would save 587
calls, or **36.3%** of all reads (`587 / 1,617`). This is a same-message upper
bound, not a measurement of consecutive runs: calls to other tools may occur
between the reads, and not every same-message group is necessarily suitable for
batching.

The original aggregate did not retain each message's read count. Therefore it
cannot be corrected after the fact for `multi_read`'s 20-entry cap: a message
with `n` reads needs `ceil(n / 20)` batches, and saves
`n - ceil(n / 20)`, rather than always `n - 1`. The 36.3% figure remains useful
as prerequisite evidence, but is explicitly an uncapped upper bound.

The measurement and this document retain no prompts, paths, response bodies,
session identifiers, message identifiers, or other user content.

## Reproducible methodology

Sennit stores tool calls as ordered JSON parts in assistant messages rather
than in a `tool_calls` table. The following query measures consecutive runs,
applies the 20-entry cap, and returns aggregates only:

```sql
WITH calls AS (
  SELECT m.id AS message_id,
         CAST(part.key AS INTEGER) AS ordinal,
         json_extract(part.value, '$.data.name') AS tool_name
  FROM messages AS m
  JOIN sessions AS s ON s.id = m.session_id,
       json_each(m.parts) AS part
  WHERE m.role = 'assistant'
    AND s.project_path = :current_project
    AND m.created_at >= strftime('%s', 'now', '-7 days')
    AND json_extract(part.value, '$.type') = 'tool_call'
), ordered AS (
  SELECT *, ordinal - row_number() OVER (
    PARTITION BY message_id, tool_name ORDER BY ordinal
  ) AS run_id
  FROM calls
), read_runs AS (
  SELECT message_id, run_id, count(*) AS read_count
  FROM ordered
  WHERE tool_name IN ('read', 'view')
  GROUP BY message_id, run_id
)
SELECT
  (SELECT count(*) FROM calls WHERE tool_name IN ('read', 'view')) AS reads,
  (SELECT count(*) FROM calls) AS all_tool_calls,
  coalesce(sum(read_count - ((read_count + 19) / 20)), 0) AS capped_savings
FROM read_runs;
```

Grouping by both `message_id` and `run_id` is what makes this a consecutive-run
measurement. Integer expression `(read_count + 19) / 20` is
`ceil(read_count / 20)` in SQLite. Re-run the query with a new seven-day window
after rollout; changes in model behaviour, task mix, or adoption can move the
realized reduction away from the original upper bound.
