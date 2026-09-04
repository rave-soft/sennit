Create or overwrite a file with given content; auto-creates parent dirs. Cannot append. For surgical changes use edit or multiedit.

Refuses, rather than just advising, in these cases:
- The target file exists but hasn't been read in this session — write replaces the whole file, so writing blind could silently discard content from the user, a formatter, or another agent.
- The target file was read, but has changed on disk since (a stale read) — same reason, made concrete: the version in the session is no longer the version on disk.
- The path resolves outside the confined working directory, when one is configured.
- The permission system denies the write.

Each refusal names what to do next (usually: read the file, then retry).
