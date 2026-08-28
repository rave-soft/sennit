package tools

import "github.com/rave-soft/sennit/internal/proto"

// These are compile-time assertions, not runtime tests: each conversion
// function only type-checks if tools.X and proto.X are literally the same
// Go type. That identity is load-bearing (see the comment on
// BashPermissionsParams in bash.go and the "Proto boundary" section of
// AGENTS.md) — a future edit that turns one of these aliases into a
// look-alike copy will fail to build here instead of silently breaking
// the permission dialog's type assertions.
var (
	_ = func(p BashPermissionsParams) proto.BashPermissionsParams { return p }
	_ = func(p DownloadPermissionsParams) proto.DownloadPermissionsParams { return p }
	_ = func(p EditPermissionsParams) proto.EditPermissionsParams { return p }
	_ = func(p FetchPermissionsParams) proto.FetchPermissionsParams { return p }
	_ = func(p AgenticFetchPermissionsParams) proto.AgenticFetchPermissionsParams { return p }
	_ = func(p LSPermissionsParams) proto.LSPermissionsParams { return p }
	_ = func(p MultiEditPermissionsParams) proto.MultiEditPermissionsParams { return p }
	_ = func(p ReadPermissionsParams) proto.ReadPermissionsParams { return p }
	_ = func(p WritePermissionsParams) proto.WritePermissionsParams { return p }
	_ = func(p ReplaceSymbolPermissionsParams) proto.ReplaceSymbolPermissionsParams { return p }
)
