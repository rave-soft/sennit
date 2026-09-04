Get LSP errors, warnings, and hints for a file or the whole project.

An empty-looking result is not always "clean": if no LSP client is running
at all, the tool says so explicitly instead of returning nothing, and a
genuinely clean check says "No diagnostics found." rather than staying
silent — the two cases used to be indistinguishable.

Each of `<file_diagnostics>` and `<project_diagnostics>` lists at most 10
entries, followed by "... and N more diagnostics" when there are more;
there is no way to page through the rest — narrow with `file_path`, or
fix the listed issues and re-run, to see further diagnostics.
