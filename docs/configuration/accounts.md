# Accounts

A provider can hold more than one **account** — more than one Codex login,
more than one API key against the same endpoint. An account is one
credentialed identity: its own token or key, its own label, its own proxy
override, and (for providers that report it) its own usage snapshot. A
provider that has never seen more than one credential still has exactly
one account under the hood; this page is about the case where it has more.

## Connecting several

```sh
sennit accounts add codex          # runs the Codex sign-in again, adding an account
sennit accounts add copilot        # same, for GitHub Copilot's device-code flow
sennit accounts add openai --api-key "$OPENAI_API_KEY_WORK"
sennit accounts add openai         # omit --api-key and you're prompted for one
```

For an OAuth provider (Codex, Copilot today) this is the same sign-in flow
as `sennit login`, run again — it always attempts a fresh sign-in rather
than reusing whatever is already active, and the result is added as another
account rather than replacing the current one. For everything else, the key
you type or pass is stored exactly as given: if it's a `$VAR`-style
template, the template is what gets stored, and it's resolved at the moment
the account is actually used, not when the account is added.

## Managing them

```sh
sennit accounts list                    # every provider that has an account
sennit accounts list codex              # just one provider's accounts
sennit accounts use codex acc_work      # switch which account is active
sennit accounts remove codex acc_work   # drop one
```

`sennit accounts list` marks the active account and shows `(disabled)` for any
account that has been turned off — a disabled account is skipped by
automatic rotation and `sennit accounts use` refuses to switch to it
directly; it has to be re-enabled first.

Removing an account refuses to take the last one a provider has — a
provider with credentials configured but nothing backing them isn't a
state this supports. Use `sennit logout` instead, which removes the
provider's credentials outright.

## Proxy

```sh
sennit accounts proxy codex socks5://127.0.0.1:1080   # provider-level base
sennit accounts proxy codex acc_work socks5://10.0.0.1:1080  # one account's own
sennit accounts proxy codex acc_work -                # clear it; falls back to the provider
sennit accounts proxy codex none                       # force a direct connection
```

Two arguments after `proxy` set the provider-level proxy; three set one
account's own override, which wins over the provider's when present. `-`
clears whichever level you targeted, falling back to what it would
otherwise inherit (the account falls back to the provider, and the
provider falls back to `HTTP_PROXY`/`HTTPS_PROXY`). `none` is a distinct
value from clearing: it forces a direct connection even when the
environment names a proxy.

The provider-level value lives in `sennit.json`/`sennitrc` as
`providers.<id>.proxy_url`. An account's own proxy is not — it lives in
`accounts.json` alongside the rest of that account's credentials (see
below).

## Where credentials live

Accounts are stored in `accounts.json`, next to the global config
directory (`~/.config/sennit/accounts.json` unless `SENNIT_GLOBAL_CONFIG`
points elsewhere), permissions `0600`. It holds OAuth tokens, API-key
templates, and cached usage snapshots for every provider's accounts.

> [!NOTE]
> `accounts.json` is not meant for hand-editing. Its format is internal and
> may change between releases; use `sennit accounts` to manage it.

## Limits display

`sennit accounts list` shows stored usage percentages only for providers that
report how much of your allowance is left — Codex today. Providers that
don't report this (API-key endpoints, GitHub Copilot) just show their
accounts with no limit figures, since there's nothing to show.

The figures are whatever the last response reported, so they can be stale
for an account you haven't used in a while. In the TUI's accounts dialog,
`ctrl+l` re-reads them for every account of the provider at once. Once a
provider has more than one account, the sidebar's plan line also names
which account the current numbers belong to.

## Rotation

Once a provider has more than one account, Sennit can switch between them
automatically instead of failing when the active one runs out. It is not a
`sennit accounts` subcommand: set it in the TUI, by opening the provider's
accounts dialog and choosing **Provider settings…**, or write it into
`sennitrc`/`sennit.json` under `providers.<id>.rotation`:

```jsonc
// sennit.json
{
  "providers": {
    "codex": {
      "rotation": {
        "enabled": true,
        "min_remaining_percent": 10,
        "order": ["acc_personal", "acc_work"]
      }
    }
  }
}
```

Which fields apply depends on whether the provider reports remaining
allowance:

- A provider that does (Codex) rotates once the active account's remaining
  allowance drops below `min_remaining_percent` (1-99, default 10). Setting
  `cooldown` on this kind of provider is a config error.
- Everyone else has no number to compare against, so rotation is reactive:
  it switches accounts on an HTTP 429, waits `cooldown` (a Go duration
  string, default `10m`) before trying that account again if the response
  carried no `Retry-After` header, and has no `min_remaining_percent`
  setting at all — setting it is a config error for this kind of provider.

`order` lists account IDs in the order rotation should try them; left
empty, it tries them in the order they were added.
