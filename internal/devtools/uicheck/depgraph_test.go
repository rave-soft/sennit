package uicheck_test

import (
	"testing"

	"golang.org/x/tools/go/packages"
)

// forbiddenImport is a production import edge the codebase used to have
// and must not regain: the packages under Pattern must not import the
// package Forbidden. The edges were removed by the C8 leaf dependency
// cleanups; these guards are what keep them removed, because nothing
// else in the build would notice an import reappearing.
//
// Matching is exact on the full import path in both directions:
// Pattern and Forbidden are compared against packages.Package.PkgPath
// with package-prefix semantics (the import path equal to the prefix or
// nested under it), never with substring search. A quoted or shortened
// pattern like "testing" or `internal/event` therefore cannot silently
// stop matching — that is precisely the regression
// TestForbiddenMatchersAreExactAndEffective pins.
//
// The check loads only production files (Tests: false), so _test.go
// files may keep importing a forbidden package — gc's own tests import
// internal/thread precisely to pin the local persisted status set against
// the domain set — and the default build constraints
// apply, matching what `go build ./...` compiles on this platform.
//
// Three sanctioned exceptions remain in the production graph: configtest,
// testenv, and rendercachetest are dedicated test-support packages whose
// APIs wrap testing.TB. They are exempted by exact path (Allow) and imported
// only by _test.go files.
//
// forbiddenImportRule bans one production import edge: the packages
// under Pattern must not import the package Forbidden. Allow lists the
// exact packages (by PkgPath) exempt from the ban: sanctioned
// test-support packages that may import the forbidden package but are
// themselves imported only by _test.go files (configtest for testing).
type forbiddenImportRule struct {
	Pattern   string
	Forbidden string
	Allow     []string
	Why       string
}

var forbiddenImports = []forbiddenImportRule{
	{
		Pattern:   "github.com/rave-soft/sennit/internal/session",
		Forbidden: "github.com/rave-soft/sennit/internal/event",
		Why:       "session reports its lifecycle through the TelemetrySink seam; the wiring to the telemetry package belongs in app/cmd composition, not in the repository service",
	},
	{
		Pattern:   "github.com/rave-soft/sennit/internal/gc",
		Forbidden: "github.com/rave-soft/sennit/internal/thread",
		Why:       "gc classifies persisted terminal statuses locally; importing thread drags the delegation runtime into the collector",
	},
	{
		Pattern:   "github.com/rave-soft/sennit/internal/gc",
		Forbidden: "github.com/rave-soft/sennit/internal/proto",
		Why:       "proto contains workspace and UI boundary DTOs, not the persisted-domain classification gc needs",
	},
	{
		Pattern:   "github.com/rave-soft/sennit/internal/format",
		Forbidden: "github.com/rave-soft/sennit/internal/ui",
		Why:       "the animated spinner primitive moved to internal/spin precisely so format (and cmd) stay presentation-neutral; the spinner is driven through plain tea messages, no TUI import is needed",
	},
	{
		Pattern:   "github.com/rave-soft/sennit/internal",
		Forbidden: "testing",
		Allow: []string{
			"github.com/rave-soft/sennit/internal/config/configtest",
			"github.com/rave-soft/sennit/internal/testenv",
			"github.com/rave-soft/sennit/internal/ui/rendercachetest",
		},
		Why: "production packages must not import testing; only dedicated support packages imported exclusively by _test.go files are exempt",
	},
}

func TestForbiddenProductionImports(t *testing.T) {
	t.Parallel()

	cfg := &packages.Config{
		Mode:  packages.NeedName | packages.NeedImports,
		Dir:   "../../..",
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, "github.com/rave-soft/sennit/internal/...")
	if err != nil {
		t.Fatalf("loading packages: %v", err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		t.Fatal("packages failed to load; see errors above")
	}

	for _, rule := range forbiddenImports {
		r := rule
		t.Run(r.Pattern+"!-"+r.Forbidden, func(t *testing.T) {
			t.Parallel()

			violations := 0
			for _, pkg := range pkgs {
				if !pkgInPattern(pkg.PkgPath, r.Pattern) {
					continue
				}
				for _, imp := range pkg.Imports {
					if pkgInPattern(imp.PkgPath, r.Forbidden) && !r.allowed(pkg.PkgPath) {
						violations++
						t.Errorf("%s imports %s: %s", pkg.PkgPath, imp.PkgPath, r.Why)
					}
				}
			}
			if violations == 0 && pkgCount(pkgs, r.Pattern) == 0 {
				t.Errorf("no packages matched %q; the pattern above is stale", r.Pattern)
			}
		})
	}
}

// pkgInPattern reports whether import path p is the package prefix or a
// package nested under it: p == prefix, or p == prefix + "/" + more.
// Full import paths only — a bare "internal" never matches
// "github.com/rave-soft/sennit/internal/session", and
// "github.com/rave-soft/sennit/internal/eventx" never matches
// "github.com/rave-soft/sennit/internal/event".
func pkgInPattern(p, prefix string) bool {
	if p == prefix {
		return true
	}
	return len(p) > len(prefix) && p[:len(prefix)] == prefix && p[len(prefix)] == '/'
}

// pkgCount reports how many loaded packages are under pattern, so a
// stale pattern fails loudly instead of vacuously passing.
func pkgCount(pkgs []*packages.Package, pattern string) int {
	n := 0
	for _, pkg := range pkgs {
		if pkgInPattern(pkg.PkgPath, pattern) {
			n++
		}
	}
	return n
}

// allowed reports whether the rule explicitly exempts pkg, by exact
// import path — a misspelled or quoted exemption matches nothing.
func (r forbiddenImportRule) allowed(pkg string) bool {
	for _, a := range r.Allow {
		if a == pkg {
			return true
		}
	}
	return false
}

// TestForbiddenMatchersAreExactAndEffective is the unit-level proof that
// the matcher cannot regress into a vacuous pass: it feeds synthetic
// import paths straight through the same predicate the production guard
// uses, so a future edit that loosens the match (substring search, a
// quoted path, a bare last element) fails here without needing a real
// production edge to exist.
func TestForbiddenMatchersAreExactAndEffective(t *testing.T) {
	t.Parallel()

	const (
		sessionPkg  = "github.com/rave-soft/sennit/internal/session"
		eventPkg    = "github.com/rave-soft/sennit/internal/event"
		internalPkg = "github.com/rave-soft/sennit/internal"
		configPkg   = internalPkg + "/config"
	)

	// A real violation must be caught: the matcher sees both ends of the
	// edge by their exact full paths.
	for _, p := range []string{sessionPkg, eventPkg, "testing", eventPkg + "/sub"} {
		if !pkgInPattern(p, p) {
			t.Errorf("pkgInPattern(%q, %q) = false; a guard that cannot see a path it is given is vacuous", p, p)
		}
	}
	if !pkgInPattern(eventPkg+"/sub", eventPkg) {
		t.Error("packages nested under the forbidden package must be caught")
	}

	// Lookalike paths that merely contain or extend the forbidden path
	// as a string are different packages and must not match.
	for _, p := range []string{
		"github.com/rave-soft/sennit/internal/eventx",
		"github.com/rave-soft/sennit/internal/eventless",
		"example.com/testing",
	} {
		if pkgInPattern(p, eventPkg) || pkgInPattern(p, "testing") {
			t.Errorf("pkgInPattern(%q) matched %q or %q; substring and partial matches are not allowed", p, eventPkg, "testing")
		}
	}

	// Quoted or shortened patterns must not match: the regression the
	// guard used to have was a Forbidden value of `"testing"` (with
	// quotes) compared through strings.Contains, which never matched the
	// import path testing and let the real edge through.
	for _, pattern := range []string{`"testing"`, `"github.com/rave-soft/sennit/internal/event"`, "internal/event"} {
		if pkgInPattern("testing", pattern) || pkgInPattern(eventPkg, pattern) {
			t.Errorf("pkgInPattern matched pattern %q; patterns must be exact import paths", pattern)
		}
	}

	// A bare last-element pattern must not match the full import path,
	// and the config pattern must still count its nested support package.
	if pkgInPattern(configPkg, "config") {
		t.Error(`a bare last-element pattern "config" must not match the full import path`)
	}
	if !pkgInPattern(configPkg+"/configtest", configPkg) {
		t.Error("configtest is nested under the config pattern and must be counted")
	}

	// Test-support exemptions must be exact: they whitelist the sanctioned
	// support packages and nothing lookalike, misspelled, or adjacent.
	rule := forbiddenImports[len(forbiddenImports)-1]
	wantAllow := []string{
		configPkg + "/configtest",
		internalPkg + "/testenv",
		internalPkg + "/ui/rendercachetest",
	}
	if rule.Pattern != internalPkg || rule.Forbidden != "testing" {
		t.Fatalf("testing rule drifted from the expected shape (pattern=%q, forbidden=%q); update the test if that is intentional", rule.Pattern, rule.Forbidden)
	}
	for _, p := range wantAllow {
		if !rule.allowed(p) {
			t.Errorf("the sanctioned test-support exemption must allow %q", p)
		}
	}
	for _, p := range []string{`"` + configPkg + `/configtest"`, configPkg + "/configtestx", "configtest", internalPkg, internalPkg + "/app/threadspawn"} {
		if rule.allowed(p) {
			t.Errorf("rule.allowed matched %q; exemptions must be exact import paths", p)
		}
	}
}
