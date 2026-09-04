Read a redacted structured diagnostic trace for a session or run. Never returns prompts, arguments, outputs, or error messages.

<usage>
- One of session_id or run_id is required; events are returned most recent
  first
- Each event has a kind: attempt_started, attempt_finished, retry, tool_call,
  tool_result, trim, or repair
- page_summary tallies the events actually scanned for this response; it is
  exact only when summary_exact is true (the scan reached the start of the
  log), otherwise it is a lower bound over a partial scan
</usage>

<pagination>
- limit is the per-call cap, default {{ .DefaultLimit }}, max {{ .MaxLimit }};
  a limit outside 1-{{ .MaxLimit }} is an error (0 uses the default)
- truncated is true when an older matching event exists beyond this page; when
  it is, next_cursor carries a token to fetch it
- cursor continues from a previous next_cursor. It is bound to the session_id/
  run_id/since filter it was issued under — pass the same filter values on the
  follow-up call, or the cursor is rejected as not matching
</pagination>
