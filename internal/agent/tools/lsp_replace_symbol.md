Replace, insert, or delete an entire symbol (function, method, class, struct) by name using LSP document symbols to find exact boundaries. Prefer this over `edit` for whole-symbol changes: it eliminates whitespace-matching failures by resolving symbol ranges through the language server instead of exact text matching.

The name must be unique in the file. When it isn't (e.g. a method implemented on two different types), the first match found in a depth-first walk of the document symbol tree is used — silently, not necessarily the one you meant. Run `lsp_symbols` first and check the line range if there is any chance the name repeats.

Refuses to run against a file whose current on-disk content hasn't been read into this session (see edit's read-before-write rule).

Actions:
- `replace` (default): replace the entire symbol including signature and body
- `add_before`: insert text before the symbol
- `add_after`: insert text after the symbol
- `delete`: remove the symbol entirely

Returns diagnostics after the edit.
