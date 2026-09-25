package workspace

import (
	"go/ast"
	"go/types"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// configPkgPath is internal/config's import path, named once so every
// check below compares against the same string.
const configPkgPath = "github.com/rave-soft/sennit/internal/config"

// uiAllowedConfigIdentifiers lists the internal/config package-level
// identifiers (types and constants) internal/ui is allowed to name - the
// shapes CLIENT-SERVER.md PR 0.5's guard note calls out
// ("SelectedModel, Scope, agent name constants..."). Everything else in
// internal/config, most pointedly the Config type itself and every method
// on it, is off limits: the UI reads configuration through
// workspace.FrontendConfig (see frontend_config.go), never through
// *config.Config, because *config.Config carries secrets (API keys, OAuth
// tokens) and unexported/json:"-" runtime state that a remote frontend
// must never see once Workspace is served over gRPC.
//
// A name only needs to be here if internal/ui legitimately references it
// as a bare `config.Name` identifier (a type in a signature, a constant, a
// package-level var). Extend this list, with a one-line reason, the next
// time a genuinely data-only/constant identifier is needed; do not add
// Config, RuntimeProvider, ProviderConfig, or any other type with
// behavior or secret-shaped fields.
var uiAllowedConfigIdentifiers = map[string]string{
	"SelectedModel": "the one-model selection value FrontendConfig.Model/RecentModels carries verbatim",
	"Scope":         "ScopeGlobal/ScopeProject selectors for the config-writing calls (SetConfigField, RecordAccount, ...)",
	"ScopeGlobal":   "see Scope",
	"ScopeProject":  "see Scope",
	"AgentCoder":    "the built-in coder agent's name, used to key config.Agent overrides and as a map/arg constant",
	"AgentTask":     "the built-in task agent's name, same use as AgentCoder",
	"DockerMCPName": "the Docker MCP server's well-known name, used to recognize its entry in FrontendConfig.MCPNames",
	// RotationConfig is a plain data struct (no methods with behavior
	// beyond pure default-filling helpers) that FrontendProvider.Rotation
	// carries verbatim - see frontend_config.go's doc comment on why it
	// is reused rather than re-declared.
	"RotationConfig": "FrontendProvider.Rotation's type, a plain data struct with no secret-shaped fields",
	// ProviderFieldKey builds the dotted config-path key SetConfigField
	// takes; it is a pure string formatter, not a Config accessor.
	"ProviderFieldKey": "builds the dotted key SetConfigField writes to (provider rotation/proxy submission)",
	// Problem and its Severity/Area enums are their own wire-safe DTO,
	// unrelated to Config: ConfigProblems/DoctorProblems (Workspace,
	// class U) already return []config.Problem directly over the wire
	// (see wire_dto_samples_test.go's Problem sample) - the doctor
	// dialog renders it.
	"Problem":      "ConfigProblems/DoctorProblems's element type, a plain wire-safe DTO",
	"Severity":     "Problem.Severity's type",
	"SeverityWarn": "a Severity value",
	"Area":         "Problem.Area's type",
	"AreaProvider": "an Area value",
	"AreaModel":    "an Area value",
	"AreaAgent":    "an Area value",
	// Scrollbar* are the string values internal/uiprefs.Prefs.Scrollbar
	// and FrontendWorkspace's cached settings compare against - display
	// preferences, not secrets.
	"ScrollbarDefault": "a Scrollbar setting value",
	"ScrollbarAlways":  "a Scrollbar setting value",
	"ScrollbarNever":   "a Scrollbar setting value",
	// GlobalConfigData is a pure path helper (the data directory path),
	// not a Config accessor.
	"GlobalConfigData": "returns the data directory path, used for a help/hint string",
}

// TestUIDoesNotTouchConfigConfig is the PR 0.5 boundary guard: it fails if
// any non-test file under internal/ui references internal/config in a way
// that is not on uiAllowedConfigIdentifiers above, or calls a method on
// (or reads a field of) a value whose type is config.Config/*config.Config.
//
// Two complementary checks, both go/types-based rather than textual
// grep: grep would false-positive on unrelated methods that happen to
// share a name with one of *config.Config's (GetModel, ProviderName, ...
// all exist on workspace.FrontendConfig on purpose, so a name-only search
// cannot tell the allowed call from the forbidden one) and would
// false-negative on a call reached through a type alias or an inferred
// local variable. Checking the resolved type instead is exact in both
// directions:
//
//  1. every *ast.Ident that go/types resolved to a package-level object
//     in internal/config must name something in the allowlist;
//  2. every *ast.SelectorExpr whose base expression's type is
//     config.Config or *config.Config is a violation outright, regardless
//     of whether the selector is a method or a field - this is what would
//     catch a value that reached UI code without going through a
//     textual `config.Config` identifier at all (e.g. a return value
//     assigned with `:=`).
func TestUIDoesNotTouchConfigConfig(t *testing.T) {
	t.Parallel()

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes |
			packages.NeedTypesInfo | packages.NeedImports | packages.NeedDeps | packages.NeedFiles,
	}
	pkgs, err := packages.Load(cfg, "github.com/rave-soft/sennit/internal/ui/...")
	require.NoError(t, err)
	require.NotEmpty(t, pkgs, "expected at least one internal/ui package")

	var violations int
	for _, pkg := range pkgs {
		for _, err := range pkg.Errors {
			t.Errorf("loading %s: %v", pkg.PkgPath, err)
		}
		for _, file := range pkg.Syntax {
			filename := pkg.Fset.Position(file.Package).Filename
			if isTestFile(filename) {
				continue
			}

			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.Ident:
					obj := pkg.TypesInfo.Uses[node]
					if obj == nil || obj.Pkg() == nil || obj.Pkg().Path() != configPkgPath {
						return true
					}
					// A struct field name (composite-literal key, or the
					// Sel half of a field selector already covered by the
					// SelectorExpr case below) is only reachable through
					// its owning type, which is itself checked separately
					// wherever it is named - config.SelectedModel{Model:
					// ...} flags "SelectedModel" if that type isn't
					// allowed, not "Model" a second time for every field.
					if v, ok := obj.(*types.Var); ok && v.IsField() {
						return true
					}
					if _, ok := uiAllowedConfigIdentifiers[obj.Name()]; !ok {
						t.Errorf("%s: references internal/config.%s, which is not on uiAllowedConfigIdentifiers; "+
							"the UI must read configuration through workspace.FrontendConfig, not internal/config directly",
							filename, obj.Name())
						violations++
					}
				case *ast.SelectorExpr:
					xt := pkg.TypesInfo.TypeOf(node.X)
					if xt == nil {
						return true
					}
					if named, ok := underlyingNamed(xt); ok && named.Obj().Pkg() != nil &&
						named.Obj().Pkg().Path() == configPkgPath && named.Obj().Name() == "Config" {
						t.Errorf("%s: selects %s on a config.Config-typed value; "+
							"the UI must read configuration through workspace.FrontendConfig, not internal/config.Config",
							filename, node.Sel.Name)
						violations++
					}
				}
				return true
			})
		}
	}
	require.Zero(t, violations, "see individual t.Errorf calls above for each violation")
}

// underlyingNamed unwraps a pointer to reach the *types.Named underneath,
// the same "Pointer, then Elem" pattern the wire walker in wire_dto_test.go
// uses for reflect.Type - here for go/types.Type instead.
func underlyingNamed(t types.Type) (*types.Named, bool) {
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	return named, ok
}

func isTestFile(path string) bool {
	const suffix = "_test.go"
	return len(path) >= len(suffix) && path[len(path)-len(suffix):] == suffix
}
