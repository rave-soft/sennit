Get LSP errors, warnings, and hints for a file or the whole project.

An empty-looking result is not always "clean": if no LSP client covers the
requested file (or, for project-wide diagnostics, none is running at all),
or a client that does cover it hasn't finished starting yet, the tool says
so explicitly instead of returning nothing. A genuinely clean check says
"No diagnostics found." — but that reports what was published by the time
the wait for diagnostics ended, not that the server has necessarily
finished background indexing; a server that keeps analyzing after it
reports ready can still publish more after this call returns.

Each of `<file_diagnostics>` and `<project_diagnostics>` lists at most 10
entries, followed by "... and N more diagnostics" when there are more;
there is no way to page through the rest — narrow with `file_path`, or
fix the listed issues and re-run, to see further diagnostics.
