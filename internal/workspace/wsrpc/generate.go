package wsrpc

// Regenerate zz_generated_types.go and zz_generated_loopback.go from
// workspace.Workspace's current method set. CI runs this and fails on any
// diff (see classes_gen_test.go's TestGeneratedFilesAreFresh for the same
// check as a plain `go test`).
//go:generate go run ./gen
