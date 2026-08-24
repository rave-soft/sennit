Read Sennit's internal application logs (default {{ .DefaultLines }} entries, max {{ .MaxLines }} per call); useful for diagnosing provider errors, tool failures, LSP/MCP issues.

<usage>
- Returns recent log entries from Sennit's internal log file, most recent last
- Use to diagnose issues with Sennit itself (provider errors, tool failures,
  LSP problems, MCP connection issues)
- Entries shown in compact format: TIME LEVEL SOURCE MESSAGE key=value...
- A trailing "-- N matched, M shown[, truncated][, next_cursor=...]" line is
  metadata; ignore it when reading the entries
- match_count is the number of matches for the current filter; match_count_exact
  is true only when the whole file was scanned (otherwise match_count is a lower
  bound and scanned_truncated is set); truncated is true when an older matching
  entry exists beyond this page
</usage>

<filters>
- level: DEBUG | INFO | WARN | ERROR (case-insensitive; an unknown level is an error)
- component: only entries whose component field equals this value (sparse)
- contains: case-insensitive substring of the message or a non-secret field
- session_id / run_id: correlation ids (set on provider/repair/trim lines)
- since: RFC3339 timestamp (e.g. 2024-01-15T10:30:00Z) or a duration like 5m /
  1h relative to now; only entries at or after it (an unparseable value is an error)
</filters>

<pagination>
- limit (alias: lines) is the per-call cap, default {{ .DefaultLines }}, max {{ .MaxLines }};
  a limit above {{ .MaxLines }} is an error
- cursor continues from a previous next_cursor to fetch older matches; it is
  stable under appended lines. A cursor bound to a rotated or replaced file is
  stale and returns an empty page - start over without it rather than paging
</pagination>

<tips>
- Default returns last {{ .DefaultLines }} entries; raise limit for more (max {{ .MaxLines }})
- Look for ERROR and WARN entries first when diagnosing problems
- For a one-run trace (provider request/retry, carried-history trim, orphan
  repair for a session_id/run_id), set chain=true
</tips>
