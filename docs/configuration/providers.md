# Providers and models

A **provider** is an endpoint that serves models. A **model** is one entry on a
provider, referred to everywhere as `provider/model-id`. Sennit ships a set of
built-in providers with their model catalogues; anything else you add yourself.

## Seeing what you have

```sh
sennit models                  # everything Sennit can see, by provider
sennit models gpt5             # filter
sennit models refresh          # re-discover models on custom providers (and Codex)
sennit models refresh ollama   # …just one
sennit models refresh codex    # re-read the per-account Codex list
```

Providers you have no credentials for are listed as `(not configured)`.

## Built-in providers

Built-in providers only need a key:

```bash
provider add anthropic --api-key "$ANTHROPIC_API_KEY"
provider add openai    --api-key "$(op read op://work/openai/key)"
```

GitHub Copilot uses a device-code login rather than a key:

```sh
sennit login copilot     # sennit logout copilot to revoke
```

OpenAI Codex runs on a ChatGPT subscription, so it signs in through the
browser rather than taking a key:

```sh
sennit login codex       # sennit logout codex to revoke
```

An existing Codex CLI login is picked up automatically when there is one, so
the browser step is skipped. Sennit reuses the access token that login
already holds rather than refreshing it: OpenAI's refresh tokens are
single-use, so spending one would log the Codex CLI out. It refreshes only
when that token is within a day of expiring, and prefers a newer one the CLI
has since produced over spending the refresh token itself. Which models the account may use is read from
the Codex backend at sign-in and written to `providers.codex.models`; re-run
`sennit login codex -f` to refresh that list after a plan change.

Signing in again adds another Codex account rather than replacing the
current one; see [Accounts](accounts.md) for managing several, switching
between them, and per-account proxies.

Because the plan is flat-rate, the sidebar shows what it costs you instead
of a running total: the tier and how much of the current allowance is gone,
e.g. `Plus · 6% of weekly limit, resets in 3d`. The figures ride along on
the model's own replies, so the line appears once the session has sent its
first message and refreshes with every one after that.

If OpenAI is only reachable through a proxy, give it one — the sign-in asks
for it before anything goes out, and the model picker's Codex dialog opens
on the same question:

```sh
sennit login codex --proxy socks5://127.0.0.1:1080
```

It is optional: leave it empty to use `HTTP_PROXY`/`HTTPS_PROXY`, or pass
`none` to force a direct connection despite them. If the Codex CLI on this
machine already has one configured (`proxy_url` or `socks_url` in
`~/.codex/config.toml`), it is offered as the default, so there is nothing
to retype. The value is saved as
`providers.codex.proxy_url`, so the token refreshes and the model requests
all take the same route as the sign-in.

To ignore the built-in catalogue entirely and declare everything yourself:

```bash
option default-providers false
```

## Any OpenAI-compatible endpoint

This is the escape hatch that covers local servers, gateways and providers
Sennit has never heard of.

```bash
# llama.cpp / LM Studio / anything speaking the OpenAI API
provider add local \
  --type openai-compat \
  --base-url "http://127.0.0.1:8080/v1" \
  --api-key "not-needed"

# Ollama has its own type
provider add ollama --type ollama --base-url "http://localhost:11434/v1"

# A hosted provider by its OpenAI-compatible API
provider add deepseek \
  --type openai-compat \
  --base-url "https://api.deepseek.com/v1" \
  --api-key "${DEEPSEEK_API_KEY:?set DEEPSEEK_API_KEY}"
```

Ask a provider to enumerate its own models instead of registering each one:

```bash
provider add local --type openai-compat \
  --base-url "http://127.0.0.1:8080/v1" \
  --api-key "not-needed" \
  --discover-models true
```

Discovery results are cached; `sennit models refresh` forces a re-scan.

### Headers, bodies and provider quirks

```bash
provider add openai \
  --api-key "$OPENAI_API_KEY" \
  --extra-header OpenAI-Organization "$OPENAI_ORG_ID" \
  --extra-body '{"service_tier":"flex"}'
```

A header whose value resolves to the empty string — an unset `$VAR`, a
`$(...)` that prints nothing, a literal `""` — is dropped from the outgoing
request rather than sent empty. That makes env-gated headers safe to leave in a
shared config.

`--system-prompt-prefix` prepends text to the system prompt for every model on
that provider, which some endpoints require.

## Registering a model

A provider that can't enumerate its models needs each one declared, along with
whatever metadata Sennit should know about it:

```bash
model add local/qwen2.5-coder-32b \
  --name "Qwen 2.5 Coder 32B" \
  --context-window 128000 \
  --default-max-tokens 8192 \
  --supports-images false
```

Pricing metadata is optional and only affects reporting — it is what `sennit
stat` uses to turn tokens into money:

```bash
model add deepseek/deepseek-chat \
  --context-window 64000 \
  --price-input 0.27 --price-output 1.10 \
  --price-cache-hit 0.07
```

For reasoning models, declare the capability and optionally a default effort:

```bash
model add local/qwq-32b --can-reason true --reasoning-effort medium
```

## Choosing the model

```bash
model anthropic/claude-sonnet-4-20250514 --think
model openai/gpt-4o --reasoning-effort high --temperature 0.2
```

With no argument, `model` prints the current selection, which makes it usable
inside the config script itself:

```bash
echo "coding with: $(model)" >&2
```

From the shell, `sennit run -m` overrides the model for one prompt. In the TUI,
`ctrl+l` opens the model switcher.

> [!NOTE]
> Session titles and summarization run on the same model as the turn that
> triggers them — Sennit does not switch to a smaller or cheaper model for
> this internal work.

### Per-agent models

An agent defined in `.sennit/agents/` can pin its own model and effort,
independently of the main selection — a cheap model for a mechanical reviewer,
an expensive one for architecture work. See [Agents](../extending/agents.md).

## Reasoning and thinking

Two different mechanisms, depending on the model:

- Models with discrete reasoning levels (`low`/`medium`/`high`) take
  `--reasoning-effort`. In the TUI this is the **effort** command.
- Models with a single thinking toggle take `--think`. In the TUI this is the
  **thinking** command.

Setting an effort a model doesn't offer is ignored at call time in favour of
the model's own default; `sennit doctor` reports it as a config problem.

## Web search

The `web_search` tool defaults to a keyless DuckDuckGo scraper. Tavily is
available for better results:

```jsonc
// sennit.json — see Legacy JSON; there is no `option` for this yet
{
  "options": {
    "web_search": { "provider": "tavily", "api_key": "$TAVILY_API_KEY" }
  }
}
```

## Diagnosing

`sennit doctor` inspects the loaded config and reports providers dropped for a
missing API key, agents pinned to a model that doesn't resolve, an effort set
on a model that can't reason, and a main model that silently fell back to a
default. It exits non-zero when any problem is an error.

It does not make network calls. To verify a key and endpoint actually work, use
`sennit models refresh` or the TUI's **Test Connection** in the providers
dialog.
