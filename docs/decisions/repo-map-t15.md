# T15 repository map: measurement and decision handoff

## Decision

**Reject implementation for T15 r1.** The first gate is met, but the
conservative estimate of calls replaceable by a single bounded repository map
is only 2.54% of discovery calls. That is well below the 20% rollout threshold
and does not justify a new tool that would overlap with `ls`, `glob`,
`read`/`multi_read`, LSP symbol tools, and `git_status`.

Reconsider only after a repeat measurement meets both gates:

1. at least 30% of user-message opportunities have three or more discovery
   calls in the first three assistant messages; and
2. at least 20% of all discovery calls are conservatively replaceable by one
   bounded map.

## Scope and definitions

The denominator is one **opportunity** for every `messages` row whose `role`
is `user`, in a session belonging to the selected current project, and whose
`created_at` is in the preceding seven days. `created_at` is a Unix timestamp
in seconds. The project filter is applied through `sessions.project_path`.

For each opportunity, messages in its session are ordered by `(created_at,
id)`. `id` is only a deterministic tiebreaker: it is not treated as a
chronological UUID. The next user-message sequence boundary ends the
opportunity. Assistant messages strictly between these boundaries are ranked,
and only the first three are measured. This prevents calls after a subsequent
user request from being attributed to the earlier request.

A call is read from an assistant message's JSON `parts` array only when
`$.type = 'tool_call'`; its name is `$.data.name` and is normalized with
`lower(...)`. Discovery calls are `ls`, `glob`, `read` (including `view`),
`multi_read`, `lsp_symbols` (including `lsp_workspace_symbols` and
`workspace_symbols`), and `git_status`.

Only `ls`, `glob`, normalized `lsp_symbols`, and `git_status` are
map-capable. `read`, `view`, and `multi_read` remain discovery calls but are
not map-capable because a metadata-only map cannot safely replace targeted
content reads. For an opportunity with `n >= 2` map-capable calls, estimated
savings are `n - 1`; otherwise savings are zero. This deliberately credits no
replacement for a single map-capable call.

The query returns aggregates only. It does not select or retain prompts,
paths, message/session IDs, tool arguments, tool results, or source content.

## Reproducible SQLite measurement

Run against Sennit's shared SQLite database in read-only mode. Set
`@project_path` to the absolute path of the project being measured; do not add
that local path to this document.

```sh
sqlite3 -readonly "$HOME/.config/sennit/sennit.db" \
  -cmd ".parameter set @project_path '/absolute/path/to/project'" <<'SQL'
.headers on
.mode column
WITH
  project_messages AS (
    SELECT m.id, m.session_id, m.role, m.parts, m.created_at
    FROM messages AS m
    JOIN sessions AS s ON s.id = m.session_id
    WHERE s.project_path = @project_path
  ),
  ordered_messages AS (
    SELECT *,
           row_number() OVER (
             PARTITION BY session_id
             ORDER BY created_at, id
           ) AS seq
    FROM project_messages
  ),
  user_messages AS (
    SELECT id,
           session_id,
           created_at,
           seq AS user_seq,
           lead(seq) OVER (
             PARTITION BY session_id
             ORDER BY seq
           ) AS next_user_seq
    FROM ordered_messages
    WHERE role = 'user'
  ),
  user_opportunities AS (
    SELECT id AS user_id, session_id, user_seq, next_user_seq
    FROM user_messages
    WHERE created_at >= strftime('%s', 'now', '-7 days')
  ),
  assistant_messages AS (
    SELECT o.user_id,
           m.id AS assistant_id,
           m.seq,
           row_number() OVER (
             PARTITION BY o.user_id
             ORDER BY m.seq
           ) AS assistant_rank
    FROM user_opportunities AS o
    JOIN ordered_messages AS m
      ON m.session_id = o.session_id
     AND m.seq > o.user_seq
     AND (o.next_user_seq IS NULL OR m.seq < o.next_user_seq)
    WHERE m.role = 'assistant'
  ),
  first_three_assistants AS (
    SELECT user_id, assistant_id, assistant_rank
    FROM assistant_messages
    WHERE assistant_rank <= 3
  ),
  tool_calls AS (
    SELECT a.user_id,
           a.assistant_rank,
           CAST(part.key AS INTEGER) AS part_ordinal,
           lower(json_extract(part.value, '$.data.name')) AS tool_name
    FROM first_three_assistants AS a
    JOIN project_messages AS m ON m.id = a.assistant_id
    JOIN json_each(m.parts) AS part
    WHERE json_extract(part.value, '$.type') = 'tool_call'
  ),
  classified_calls AS (
    SELECT user_id,
           assistant_rank,
           part_ordinal,
           CASE tool_name
             WHEN 'ls' THEN 'ls'
             WHEN 'glob' THEN 'glob'
             WHEN 'read' THEN 'read'
             WHEN 'view' THEN 'read'
             WHEN 'multi_read' THEN 'multi_read'
             WHEN 'lsp_symbols' THEN 'lsp_symbols'
             WHEN 'lsp_workspace_symbols' THEN 'lsp_symbols'
             WHEN 'workspace_symbols' THEN 'lsp_symbols'
             WHEN 'git_status' THEN 'git_status'
           END AS discovery_tool,
           CASE tool_name
             WHEN 'ls' THEN 1
             WHEN 'glob' THEN 1
             WHEN 'lsp_symbols' THEN 1
             WHEN 'lsp_workspace_symbols' THEN 1
             WHEN 'workspace_symbols' THEN 1
             WHEN 'git_status' THEN 1
             ELSE 0
           END AS map_capable
    FROM tool_calls
  ),
  per_opportunity AS (
    SELECT o.user_id,
           count(c.discovery_tool) AS discovery_calls,
           coalesce(sum(c.map_capable), 0) AS map_capable_calls
    FROM user_opportunities AS o
    LEFT JOIN classified_calls AS c ON c.user_id = o.user_id
    GROUP BY o.user_id
  )
SELECT
  count(*) AS opportunities,
  coalesce(sum(discovery_calls), 0) AS discovery_calls,
  coalesce(sum(discovery_calls >= 3), 0) AS opportunities_with_three_or_more,
  coalesce(sum(CASE
    WHEN map_capable_calls >= 2 THEN map_capable_calls - 1
    ELSE 0
  END), 0) AS estimated_savings,
  round(
    100.0 * coalesce(sum(CASE
      WHEN map_capable_calls >= 2 THEN map_capable_calls - 1
      ELSE 0
    END), 0) / nullif(sum(discovery_calls), 0),
    2
  ) AS estimated_reduction_percent
FROM per_opportunity;
SQL
```

## Recorded result and interpretation

The independent review snapshot for the current project was:

| Metric | Result |
| --- | ---: |
| Opportunities | 216 |
| Discovery calls | 905 |
| Opportunities with three or more discovery calls | 135 |
| Estimated savings | 23 |
| Estimated reduction | 2.54% |

That snapshot passes the first gate at 62.50% (`135 / 216`) and fails the
second gate at 2.54% (`23 / 905`); therefore the decision is **reject**.

A local reproduction was run at `2026-08-24T19:53:59Z`, with the dynamic
cutoff reported by SQLite as Unix time `1786996439`:

```text
opportunities  discovery_calls  opportunities_with_three_or_more  estimated_savings  estimated_reduction_percent
-------------  ---------------  --------------------------------  -----------------  ---------------------------
217            911              136                               23                 2.52
```

The small difference is expected: the window is intentionally dynamic because
the query evaluates `strftime('%s', 'now', '-7 days')`; moreover, new
in-project activity can add an opportunity while the measurement is being
reviewed. Counts near the cutoff can also change as messages enter or leave the
seven-day window. Both snapshots retain the same decision: 2.52% and 2.54% are
far below 20%. A subsequent decision review must record its own query output,
timestamp, and cutoff rather than treating either aggregate snapshot as
permanent.

## Safety boundary if later approved

A future `repo_map` may return only bounded metadata: detected languages,
module/manifests, key directories, symbol names/signatures, Git status summary,
and large-file metadata. It must not return source contents, prompts, tool
arguments, secrets, or arbitrary command output. It must enforce explicit
directory-depth, file-count, symbol-count, and response-byte limits, and must
not replace tracked content reads invisibly.
