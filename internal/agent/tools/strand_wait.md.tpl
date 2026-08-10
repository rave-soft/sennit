Block until the given strands settle: none of them are pending, running, or
merging anymore.

Use this after creating or sending work to one or more strands, when you
need their results before continuing (e.g. before merging or reviewing
their output).

Parameters:
- `ids` (optional): strand IDs or names to wait for. Omit to wait for every
  strand.
- `timeout_seconds` (optional): give up and return after this many seconds.
  Omit or 0 for no timeout beyond the tool call's own cancellation.
