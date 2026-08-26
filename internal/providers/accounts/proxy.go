package accounts

import "github.com/rave-soft/sennit/internal/proxyhttp"

// ResolveProxy returns the proxy a request for this account should take,
// given the account's own proxy override and its provider's.
//
// Priority is account, then provider, then "": a non-empty accountProxy
// always wins, an empty one falls back to providerProxy, and both empty
// means no override at all — the caller inherits HTTP_PROXY/HTTPS_PROXY
// from the environment exactly as it does today.
//
// The easy mistake here is treating [proxyhttp.Direct] ("none") as if it
// were the same as "": it is not. "" means "nothing configured, fall
// through"; "none" means "a direct connection was deliberately chosen at
// this level, in preference to whatever the environment or a lower-
// priority level would otherwise pick." So "none" on the account beats a
// provider proxy URL, and "none" on the provider beats the environment,
// exactly like any other explicit value — it just never falls through.
func ResolveProxy(accountProxy, providerProxy string) string {
	if accountProxy != "" {
		return accountProxy
	}
	return providerProxy
}

// IsDirect reports whether proxy is the explicit "connect directly, ignore
// the environment" sentinel rather than "" ("no override"). Callers that
// need to branch on that distinction should use this instead of comparing
// against a hand-copied "none", so accounts stays in sync with
// [proxyhttp.Direct] if that value ever changes.
func IsDirect(proxy string) bool {
	return proxy == proxyhttp.Direct
}
