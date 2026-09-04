Edit a file by exact find-and-replace; can also create or delete content. If old_string differs from the file only in whitespace, the matching lines are still edited and new_string is re-indented to the file's style; the response says when this happened, so verify the result. For whole-function/method/type replacements prefer `lsp_replace_symbol` (no whitespace matching needed; also prefer it for a unique symbol name — see its own doc for how it picks among duplicates). For renames prefer `lsp_rename` (semantic, cross-file). For large edits use write.

Refuses, rather than just advising, in these cases:
- The file hasn't been read in this session, or was read but has changed on disk since (a stale read).
- The specific lines the edit would touch fall outside what was actually read — a prior read can cover only part of a large file, and old_string being right about text outside that window is not enough.
- `old_string` matches more than once in the file and `replace_all` isn't set — the edit refuses rather than guess which occurrence you meant.
- The path resolves outside the confined working directory, when one is configured.
- The permission system denies the edit.

Each refusal names what to do next (usually: read the relevant part of the file, then retry).
