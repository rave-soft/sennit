Find all references to a symbol by name via LSP; more accurate than grep for code symbols.

Every distinct symbol identity that grep found a candidate location for is
queried, not just the first one that resolves — if the name is not unique
(e.g. a method implemented on two different types), references for every
such symbol are merged into one list. A candidate that turns out to be
another use of an identity already queried is not re-queried. When the
underlying search hit its match limit, or a candidate location could not be
queried, the response says so explicitly instead of silently reporting a
partial list as complete.
