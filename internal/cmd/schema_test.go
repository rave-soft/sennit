package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/invopop/jsonschema"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/stretchr/testify/require"
)

func TestSchemaNoBrokenRefs(t *testing.T) {
	t.Parallel()

	reflector := new(jsonschema.Reflector)
	bts, err := json.Marshal(reflector.Reflect(&config.Config{}))
	require.NoError(t, err)

	var schema struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(bts, &schema))
	require.NotEmpty(t, schema.Defs, "schema should have definitions")

	for name := range schema.Defs {
		require.NotContains(t, name, "/", "schema $def key %q contains '/' which breaks JSON Pointer $ref resolution", name)
	}
}

func TestSchemaProvidersHasAdditionalProperties(t *testing.T) {
	t.Parallel()

	reflector := new(jsonschema.Reflector)
	bts, err := json.Marshal(reflector.Reflect(&config.Config{}))
	require.NoError(t, err)

	var schema struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(bts, &schema))

	var cfg struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(schema.Defs["Config"], &cfg))

	providersRaw, ok := cfg.Properties["providers"]
	require.True(t, ok, "Config should have a providers property")

	var providers struct {
		Type                 string          `json:"type"`
		AdditionalProperties json.RawMessage `json:"additionalProperties"`
	}
	require.NoError(t, json.Unmarshal(providersRaw, &providers))
	require.Equal(t, "object", providers.Type)
	require.True(t, strings.Contains(string(providers.AdditionalProperties), "ProviderConfig"),
		"providers should use additionalProperties with a ProviderConfig ref, got: %s", string(providers.AdditionalProperties))
}

// repoRoot locates the repository from this source file's own path
// rather than from the working directory. Other tests in this package
// chdir into temp directories, and with t.Parallel() a relative
// "../../schema.json" resolves against whichever cwd happens to be
// installed at the moment the read runs.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "could not locate this test file")
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// The committed schema.json is what the docs site publishes (via the
// docs/schema.json symlink), and its $id claims that address. If the
// generator's $id and the file on disk disagree, the published schema
// identifies itself as something other than where it lives — so
// regenerate rather than let the two drift.
func TestSchemaCommittedFileCarriesThePublishedID(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "schema.json"))
	require.NoError(t, err, "schema.json must be committed at the repo root")

	var schema struct {
		ID string `json:"$id"`
	}
	require.NoError(t, json.Unmarshal(raw, &schema))
	require.Equal(t, SchemaID, schema.ID,
		"schema.json is stale — run `task schema` to regenerate it")
}

// A "$schema" example that points at a URL nothing serves is worse than
// none: an editor silently fetches nothing and the user gets no
// completion, with no error to explain why. The fork inherited exactly
// that — Charm's domain with Sennit's filename — so pin the examples to
// the address the schema is actually published at.
func TestSchemaExamplesPointAtThePublishedURL(t *testing.T) {
	t.Parallel()

	// Every file a user might copy a "$schema" line out of.
	root := repoRoot(t)
	examples := []string{
		filepath.Join(root, "docs", "configuration", "sennitrc.md"),
		filepath.Join(root, "internal", "skills", "builtin", "sennit-config", "SKILL.md"),
	}

	for _, path := range examples {
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		text := string(body)
		require.NotContains(t, text, "charm.land/sennit.json",
			"%s still points at the upstream domain with this fork's filename", path)
		require.Contains(t, text, `"$schema": "`+SchemaID+`"`,
			"%s must show the address the schema is published at", path)
	}
}
