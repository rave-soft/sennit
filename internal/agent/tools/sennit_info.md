Get Sennit's current runtime state: active model, provider, LSP/MCP status, skills, hooks, permissions, and disabled tools. No parameters needed for the full dump; pass `models_for` to list one provider's available model IDs instead.

<usage>
- Shows active model and provider, LSP/MCP server status, skills,
  hooks, permissions mode, disabled tools, and key options
- Use when diagnosing why something isn't working (missing diagnostics,
  provider errors, MCP disconnections)
- No parameters needed — always returns the full current state
- `{"models_for": "<provider-id>"}` returns just that provider's model IDs
  (one per line, capped at 50 with "...and N more" for large router
  catalogs) instead of the full dump — use this to verify a model ID
  actually exists before writing `provider/model-id` into an agent file,
  `model add`, or `sennit.json`
- Add `model_filter` (a case-insensitive substring) alongside `models_for`
  to search a large catalog for a specific ID instead of relying on the
  50-entry cap — a router provider can carry thousands of models, and an
  ID past the cap is invisible without a filter
</usage>

<tips>
- [problems]: config issues found by the built-in doctor (e.g. an agent
  pinned to a model that doesn't exist, silently falling back to the
  main model)
- [lsp]/[mcp]: service health
- [providers]: which providers are enabled and available, then call again
  with `models_for` to see that provider's actual model IDs before
  referencing one
- [skills]: which skills are available and whether loaded this session
- [hooks]: which hook events are configured and whether the hook runner
  is active
- Pair with the sennit-config skill to fix configuration issues
</tips>
