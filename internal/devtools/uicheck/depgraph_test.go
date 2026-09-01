package uicheck_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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

// moduleRoot is this repository's own import prefix: the walk in
// findPath steps through these packages and stops at anyone else's.
const moduleRoot = "github.com/rave-soft/sennit"

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
	// The architectural boundaries. Each of these is a seam the tree
	// paid for once and would lose silently: nothing in the build
	// notices a dependency arriving through three hops, and the reason
	// it must not arrive lives in a comment the next edit need not read.
	{
		Pattern:   "github.com/rave-soft/sennit/internal/ui",
		Forbidden: "github.com/rave-soft/sennit/internal/db",
		Why:       "the TUI reads its data through the Workspace seam and internal/proto DTOs; the last link from ui to the database was cut by turning the tools.*PermissionsParams aliases around (AGENTS.md, 2026-08-28), and it arrived transitively the first time",
	},
	{
		Pattern:   "github.com/rave-soft/sennit/internal/ui",
		Forbidden: "github.com/rave-soft/sennit/internal/thread",
		Why:       "the delegation runtime reaches the TUI as proto.Thread data, never as its own types; internal/ui/threads is the view, internal/thread is the machine",
	},
	{
		Pattern:   "github.com/rave-soft/sennit/internal/ui",
		Forbidden: "github.com/rave-soft/sennit/internal/app",
		Why:       "the app composes the workspace and hands the TUI a Workspace; a UI package reaching back into the composition root inverts that",
	},
	{
		Pattern:   "github.com/rave-soft/sennit/internal/thread",
		Forbidden: "github.com/rave-soft/sennit/internal/app",
		Why:       "thread declares what it needs as ports (internal/thread/ports.go) and the app satisfies them; importing the app back is the cycle those ports exist to avoid",
	},
	{
		Pattern:   "github.com/rave-soft/sennit/internal/thread",
		Forbidden: "github.com/rave-soft/sennit/internal/agent",
		Why:       "same seam from the other side: a delegation runs an agent through a spawner port, and thread must not know what an agent is",
	},
	{
		Pattern:   "github.com/rave-soft/sennit/internal/thread",
		Forbidden: "github.com/rave-soft/sennit/internal/db",
		Why:       "thread persists through its Store interface; the SQL layer belongs to whoever implements it",
	},
	{
		Pattern:   "github.com/rave-soft/sennit/internal/agent/tools",
		Forbidden: "github.com/rave-soft/sennit/internal/thread",
		Why:       "tools mirror thread's types (ThreadCreateArgs, TaskInfo, SendOutcome) rather than importing them, and internal/app/threadspawn converts at the seam; the mirrors are pointless the moment this edge exists",
	},
	{
		Pattern:   "github.com/rave-soft/sennit/internal/agent",
		Forbidden: "github.com/rave-soft/sennit/internal/ui",
		Why:       "the agent renders nothing; what a tool call looks like is decided in internal/ui from the call and its result",
	},
	{
		Pattern:   "github.com/rave-soft/sennit/internal/toolmeta",
		Forbidden: "github.com/rave-soft/sennit/internal",
		Allow: []string{
			"github.com/rave-soft/sennit/internal/toolmeta",
		},
		Why: "toolmeta is the one table both the agent and the UI read, and it stays a leaf on purpose: a dependency here would be imported by everything that classifies a tool",
	},
	{
		Pattern:   "github.com/rave-soft/sennit/internal/proto",
		Forbidden: "github.com/rave-soft/sennit/internal/db",
		Why:       "proto is the workspace/UI data boundary and must stay light enough for the TUI to import; internal/proto -> internal/agent/tools -> internal/db is exactly how the TUI used to reach the database",
	},
	{
		Pattern:   "github.com/rave-soft/sennit/internal/proto",
		Forbidden: "github.com/rave-soft/sennit/internal/agent",
		Why:       "the alias direction is proto -> tools, not tools -> proto (AGENTS.md); reversing it puts the agent's dependency graph behind every proto import",
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
		// NeedDeps as well as NeedImports: without it the Imports map
		// holds stubs whose own Imports are empty, and the walk below
		// would silently see the graph one level deep - which is how a
		// forbidden dependency arriving through three hops passed
		// unnoticed in the first place.
		Mode:  packages.NeedName | packages.NeedImports | packages.NeedDeps,
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
				if !pkgInPattern(pkg.PkgPath, r.Pattern) || r.allowed(pkg.PkgPath) {
					continue
				}
				if path := r.findPath(pkg); path != nil {
					violations++
					t.Errorf("%s depends on %s: %s\n  %s",
						pkg.PkgPath, path[len(path)-1], r.Why, strings.Join(path, "\n    -> "))
				}
			}
			if violations == 0 && pkgCount(pkgs, r.Pattern) == 0 {
				t.Errorf("no packages matched %q; the pattern above is stale", r.Pattern)
			}
		})
	}
}

// findPath returns the shortest import path from pkg to a package the
// rule forbids, starting at pkg itself, or nil when there is none.
//
// It walks the whole graph rather than pkg's direct imports because that
// is how the edges this guards actually arrive. The one the tree
// removed by hand - the TUI reaching the database - was
// internal/ui -> internal/proto -> internal/agent/tools -> internal/db:
// four packages, no single import anyone would have questioned. A
// direct-import check reports nothing about that, and reporting only the
// endpoints would leave the reader to rediscover the path, so the whole
// chain goes into the failure.
//
// Breadth-first, so the reported chain is the shortest one and stays
// stable as unrelated edges come and go. Allowed packages are not
// traversed at all: a sanctioned exception (a test-support package that
// may import testing) must not make every package that reaches it a
// violation.
//
// The walk only steps through this repository's own packages, though it
// tests every import it sees. These rules are about this tree's
// architecture, and a dependency that arrives through somebody else's
// module is not one this tree can move: a provider SDK in charm.land/
// fantasy imports google.golang.org/genai, which imports testing, so
// walking third-party edges made "no production package imports testing"
// fail for internal/cmd with a four-hop chain it has no say in. A
// forbidden package reached *directly* from one of our packages is still
// caught, whoever owns it.
func (r forbiddenImportRule) findPath(pkg *packages.Package) []string {
	type node struct {
		pkg  *packages.Package
		path []string
	}
	seen := map[string]bool{pkg.PkgPath: true}
	queue := []node{{pkg: pkg, path: []string{pkg.PkgPath}}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		// Imports is a map, so iterate it in a fixed order or the
		// reported path among several equally short ones changes from
		// run to run.
		for _, name := range sortedImports(cur.pkg) {
			imp := cur.pkg.Imports[name]
			if seen[imp.PkgPath] || r.allowed(imp.PkgPath) {
				continue
			}
			seen[imp.PkgPath] = true
			path := append(append([]string(nil), cur.path...), imp.PkgPath)
			if pkgInPattern(imp.PkgPath, r.Forbidden) {
				return path
			}
			if pkgInPattern(imp.PkgPath, moduleRoot) {
				queue = append(queue, node{pkg: imp, path: path})
			}
		}
	}
	return nil
}

// sortedImports returns pkg's import paths in a stable order.
func sortedImports(pkg *packages.Package) []string {
	names := make([]string, 0, len(pkg.Imports))
	for name := range pkg.Imports {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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

// TestFindPathWalksTheWholeGraph is the unit-level proof that the guard
// cannot regress into the direct-import check it used to be.
//
// The edge this exists for is not one anybody writes on purpose: the TUI
// reached the database through internal/proto and internal/agent/tools,
// three hops of imports each of which looked reasonable on its own. A
// direct-import guard reports nothing at all about that, and it passes
// just as loudly as a real one - which is the failure mode a synthetic
// graph here catches without waiting for a real edge to reappear.
func TestFindPathWalksTheWholeGraph(t *testing.T) {
	t.Parallel()

	pkg := func(path string, imports ...*packages.Package) *packages.Package {
		p := &packages.Package{PkgPath: path, Imports: map[string]*packages.Package{}}
		for _, imp := range imports {
			p.Imports[imp.PkgPath] = imp
		}
		return p
	}
	const (
		ui    = moduleRoot + "/internal/ui"
		proto = moduleRoot + "/internal/proto"
		tools = moduleRoot + "/internal/agent/tools"
		db    = moduleRoot + "/internal/db"
	)

	dbPkg := pkg(db)
	uiPkg := pkg(ui, pkg(proto, pkg(tools, dbPkg)))
	rule := forbiddenImportRule{Pattern: ui, Forbidden: db}

	path := rule.findPath(uiPkg)
	require.Equal(t, []string{ui, proto, tools, db}, path,
		"the whole chain is the finding: reporting only the endpoints leaves the reader to rediscover it")

	// A graph with no route to the forbidden package is clean, not
	// merely unreported.
	require.Nil(t, rule.findPath(pkg(ui, pkg(proto))))

	// An allowed package is not traversed: a sanctioned exception must
	// not make every package that reaches it a violation.
	allowRule := forbiddenImportRule{Pattern: ui, Forbidden: db, Allow: []string{proto}}
	require.Nil(t, allowRule.findPath(uiPkg))

	// Somebody else's module is tested but not walked through: a
	// dependency that arrives inside a third-party package is not one
	// this tree can move (see findPath).
	thirdParty := pkg(ui, pkg("example.com/sdk", dbPkg))
	require.Nil(t, rule.findPath(thirdParty))
	require.Equal(t, []string{ui, db}, rule.findPath(pkg(ui, dbPkg)),
		"a forbidden package imported directly is still caught, whoever owns it")

	// The shortest chain wins, so the report stays stable as unrelated
	// edges come and go.
	long := pkg(proto, pkg(tools, dbPkg))
	require.Equal(t, []string{ui, db}, rule.findPath(pkg(ui, dbPkg, long)))
}
