// Command gen generates internal/workspace/wsrpc's zz_generated_types.go
// and zz_generated_loopback.go from workspace.Workspace's method set. Run
// via `go generate ./internal/workspace/wsrpc/...` (see the //go:generate
// directive in ../generate.go); its actual work lives in ./genlib so
// wsrpc's own freshness test (classes_gen_test.go) can call the same code
// without importing a package named "main", which the toolchain refuses.
package main

import (
	"fmt"
	"os"

	"github.com/rave-soft/sennit/internal/workspace/wsrpc/gen/genlib"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run() error {
	typesSrc, loopbackSrc, err := genlib.Generate()
	if err != nil {
		return err
	}
	if err := os.WriteFile(genlib.TypesFileName, typesSrc, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", genlib.TypesFileName, err)
	}
	if err := os.WriteFile(genlib.LoopbackFileName, loopbackSrc, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", genlib.LoopbackFileName, err)
	}
	return nil
}
