package cmd

import (
	"encoding/json"
	"fmt"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/invopop/jsonschema"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/discover"
	"github.com/spf13/cobra"
)

// SchemaID is where the generated schema is published, and so what a
// sennit.json's "$schema" should point an editor at. It is also the
// schema's own identity: left to itself the reflector derives an $id
// from the Go package path (".../internal/config/config"), which is a
// URL nothing serves — a resolver following it to fetch a $ref gets a
// 404. The file is published by the docs site, via the docs/schema.json
// symlink to the generated file at the repo root.
const SchemaID = "https://rave-soft.github.io/sennit/schema.json"

var schemaCmd = &cobra.Command{
	Use:    "schema",
	Short:  "Generate JSON schema for configuration",
	Long:   "Generate JSON schema for the sennit configuration file",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		reflector := new(jsonschema.Reflector)
		schema := reflector.Reflect(&config.Config{})
		schema.ID = SchemaID
		setProviderTypeEnum(schema)
		bts, err := json.MarshalIndent(schema, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal schema: %w", err)
		}
		fmt.Println(string(bts))
		return nil
	},
}

// setProviderTypeEnum overwrites the provider `type` enum with the live set
// of accepted values rather than a hand-maintained struct tag. The values
// must match exactly what load.go validates against: the catwalk provider
// types, the Charm Hyper type, and any locally-discovered providers that
// self-register an enricher (e.g. ollama, omlx). Sourcing the enum here keeps
// the published schema from drifting as provider types are added or renamed.
func setProviderTypeEnum(schema *jsonschema.Schema) {
	def, ok := schema.Definitions["ProviderConfig"]
	if !ok || def.Properties == nil {
		return
	}
	typeProp, ok := def.Properties.Get("type")
	if !ok {
		return
	}

	var types []string
	for _, t := range catwalk.KnownProviderTypes() {
		types = append(types, string(t))
	}
	types = append(types, discover.RegisteredProviderTypes()...)

	typeProp.Enum = make([]any, len(types))
	for i, t := range types {
		typeProp.Enum[i] = t
	}
}
