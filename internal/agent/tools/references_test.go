package tools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/stretchr/testify/require"
)

// referencesSyncHelperProcessEnv gates referencesSyncLSPHelper below: a fake
// LSP server whose textDocument/references answer depends on whatever
// content the client most recently sent via didOpen/didChange, the same
// technique lsp_definition_test.go uses, needed because a fixed-location
// canned response cannot distinguish a stale overlay from a resynced one.
const referencesSyncHelperProcessEnv = "SENNIT_REFERENCES_SYNC_HELPER"

// TestReferencesSyncLSPHelperProcess reports a reference only when the
// requested line, in whatever content it was last told about, actually
// names the target identifier.
func TestReferencesSyncLSPHelperProcess(t *testing.T) {
	if os.Getenv(referencesSyncHelperProcessEnv) != "1" {
		return
	}
	runReferencesSyncLSPHelper()
	os.Exit(0)
}

func runReferencesSyncLSPHelper() {
	r := bufio.NewReader(os.Stdin)
	var docText string
	for {
		body, err := readLSPToolFrame(r)
		if err != nil {
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &request) != nil {
			continue
		}
		if len(request.ID) == 0 {
			switch request.Method {
			case "textDocument/didOpen":
				var params struct {
					TextDocument struct {
						Text string `json:"text"`
					} `json:"textDocument"`
				}
				if json.Unmarshal(request.Params, &params) == nil {
					docText = params.TextDocument.Text
				}
			case "textDocument/didChange":
				var params struct {
					ContentChanges []struct {
						Text string `json:"text"`
					} `json:"contentChanges"`
				}
				if json.Unmarshal(request.Params, &params) == nil && len(params.ContentChanges) > 0 {
					docText = params.ContentChanges[0].Text
				}
			}
			continue
		}

		var result string
		switch request.Method {
		case "initialize":
			result = `{"capabilities":{"referencesProvider":true}}`
		case "textDocument/references":
			var params struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
				Position struct {
					Line uint32 `json:"line"`
				} `json:"position"`
			}
			result = "null"
			if json.Unmarshal(request.Params, &params) == nil {
				lines := strings.Split(docText, "\n")
				if int(params.Position.Line) < len(lines) && strings.Contains(lines[params.Position.Line], "Exact") {
					result = fmt.Sprintf(`[{"uri":%q,"range":{"start":{"line":%d,"character":0},"end":{"line":%d,"character":5}}}]`,
						params.TextDocument.URI, params.Position.Line, params.Position.Line)
				}
			}
		default:
			result = "null"
		}
		response := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, result))
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(response), response)
	}
}

func newReferencesSyncE2EManager(t *testing.T, root string) *lsp.Manager {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	autoLSP := false
	store := configtest.NewStore(t, &config.Config{
		Options: &config.Options{AutoLSP: &autoLSP},
		LSP: config.LSPs{"gopls": {
			Command:     exe,
			Args:        []string{"-test.run=^TestReferencesSyncLSPHelperProcess$"},
			Env:         map[string]string{referencesSyncHelperProcessEnv: "1"},
			FileTypes:   []string{"go"},
			RootMarkers: []string{"go.mod"},
			Timeout:     5,
		}},
	}, configtest.WithWorkingDir(root))
	manager := lsp.NewManager(store)
	t.Cleanup(func() { manager.StopAll(t.Context()) })
	return manager
}

// TestLSPReferencesResyncsOverlayBeforeQuerying is the regression test for
// finding 1.5's references surface: unlike resolveSymbol (used by
// definition/rename/call-hierarchy), lsp_references calls FindReferences
// directly on every resolveSymbolResults candidate rather than going
// through firstSymbolWithDefinition, so it needed its own syncOverlay call
// in references.go's loop. Same fixture and staleness setup as
// TestLSPDefinitionResyncsOverlayBeforeCheckingViability: "Exact" shifts
// from line 2 to line 3 on disk without notifying the already-open client.
func TestLSPReferencesResyncsOverlayBeforeQuerying(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/e2e\n\ngo 1.24\n"), 0o644))
	original := "package e2e\n\nfunc Exact() string { return \"old\" }\n"
	file := filepath.Join(root, "a.go")
	require.NoError(t, os.WriteFile(file, []byte(original), 0o644))

	manager := newReferencesSyncE2EManager(t, root)

	manager.Start(t.Context(), file)
	client := findLSPClient(manager, file)
	require.NotNil(t, client)
	require.NoError(t, client.OpenFileOnDemand(t.Context(), file))

	shifted := "// Package e2e is a fixture.\n" + original
	require.NoError(t, os.WriteFile(file, []byte(shifted), 0o644))

	tool := NewReferencesTool(manager, root)
	resp := runToolWith(t, tool, t.Context(), ReferencesToolName, ReferencesParams{Symbol: "Exact", Path: "."})

	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "Found 1 reference(s)",
		"a resynced overlay must let the fake server confirm the identifier at its post-shift line")
	require.NotContains(t, resp.Content, "No references found",
		"a stale overlay must not make a genuine symbol read back as having no references")
}

// referencesMultiHelperProcessEnv gates referencesMultiLSPHelper below: a
// fake server that answers every textDocument/references request with a
// reference derived from the requested line, so two distinct grep
// candidates for the same symbol name (finding A: a homonymous method on
// two types) are distinguishable in the tool's merged output.
const referencesMultiHelperProcessEnv = "SENNIT_REFERENCES_MULTI_HELPER"

// TestReferencesMultiHelperProcess is the fake LSP server subprocess for
// TestLSPReferencesQueriesEveryCandidate below.
func TestReferencesMultiHelperProcess(t *testing.T) {
	if os.Getenv(referencesMultiHelperProcessEnv) != "1" {
		return
	}
	runReferencesMultiLSPHelper()
	os.Exit(0)
}

func runReferencesMultiLSPHelper() {
	r := bufio.NewReader(os.Stdin)
	for {
		body, err := readLSPToolFrame(r)
		if err != nil {
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &request) != nil || len(request.ID) == 0 {
			continue
		}

		var result string
		switch request.Method {
		case "initialize":
			result = `{"capabilities":{"referencesProvider":true}}`
		case "textDocument/references":
			var params struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
				Position struct {
					Line uint32 `json:"line"`
				} `json:"position"`
			}
			result = "null"
			if json.Unmarshal(request.Params, &params) == nil {
				// Every distinct requested line stands in for a distinct
				// symbol identity: answer with a reference at a fixed
				// offset from that line, so candidates never collide
				// after cleanupLocations dedupes the merged set.
				refLine := params.Position.Line + 1000
				result = fmt.Sprintf(`[{"uri":%q,"range":{"start":{"line":%d,"character":0},"end":{"line":%d,"character":3}}}]`,
					params.TextDocument.URI, refLine, refLine)
			}
		default:
			result = "null"
		}
		response := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, result))
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(response), response)
	}
}

func newReferencesMultiE2EManager(t *testing.T, root string) *lsp.Manager {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	autoLSP := false
	store := configtest.NewStore(t, &config.Config{
		Options: &config.Options{AutoLSP: &autoLSP},
		LSP: config.LSPs{"gopls": {
			Command:     exe,
			Args:        []string{"-test.run=^TestReferencesMultiHelperProcess$"},
			Env:         map[string]string{referencesMultiHelperProcessEnv: "1"},
			FileTypes:   []string{"go"},
			RootMarkers: []string{"go.mod"},
			Timeout:     5,
		}},
	}, configtest.WithWorkingDir(root))
	manager := lsp.NewManager(store)
	t.Cleanup(func() { manager.StopAll(t.Context()) })
	return manager
}

// TestLSPReferencesQueriesEveryCandidate is the regression test for
// finding A: lsp_references used to stop at the first grep candidate
// whose FindReferences call came back non-empty, discarding every other
// candidate — so a name shared by two distinct symbols (a method
// implemented on two different types, here both named "Twin") reported
// only one symbol's references while claiming to have found "all
// references". The fixture has two matches for "Twin"; the fake server
// answers each with a distinct reference, so a complete merge must report
// two, not one.
func TestLSPReferencesQueriesEveryCandidate(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/e2e\n\ngo 1.24\n"), 0o644))
	src := "package e2e\n\nfunc Twin() {}\n\ntype T struct{}\n\nfunc (T) Twin() {}\n"
	file := filepath.Join(root, "a.go")
	require.NoError(t, os.WriteFile(file, []byte(src), 0o644))

	manager := newReferencesMultiE2EManager(t, root)

	tool := NewReferencesTool(manager, root)
	resp := runToolWith(t, tool, t.Context(), ReferencesToolName, ReferencesParams{Symbol: "Twin", Path: "."})

	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "Found 2 reference(s)",
		"both grep candidates for a homonymous symbol must be queried, not just the first that answers")
}

// referencesDedupHelperProcessEnv gates referencesDedupLSPHelper below: a
// fake LSP server that, like a real one, answers textDocument/references
// with every occurrence of the requested word in whatever content it was
// last told about — so grep's several candidates for one common
// identifier all belong to the same identity, and only the first should
// ever need to be asked. SENNIT_REFERENCES_DEDUP_LOG names a file this
// helper appends one byte to per request, letting the test count how
// many round trips actually happened.
const referencesDedupHelperProcessEnv = "SENNIT_REFERENCES_DEDUP_HELPER"

// TestReferencesDedupHelperProcess is the fake LSP server subprocess for
// TestLSPReferencesSkipsCandidatesAlreadyCovered below.
func TestReferencesDedupHelperProcess(t *testing.T) {
	if os.Getenv(referencesDedupHelperProcessEnv) != "1" {
		return
	}
	runReferencesDedupLSPHelper()
	os.Exit(0)
}

func runReferencesDedupLSPHelper() {
	r := bufio.NewReader(os.Stdin)
	var docText string
	logPath := os.Getenv("SENNIT_REFERENCES_DEDUP_LOG")
	wordRe := regexp.MustCompile(`\bcount\b`)
	for {
		body, err := readLSPToolFrame(r)
		if err != nil {
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &request) != nil {
			continue
		}
		if len(request.ID) == 0 {
			switch request.Method {
			case "textDocument/didOpen":
				var params struct {
					TextDocument struct {
						Text string `json:"text"`
					} `json:"textDocument"`
				}
				if json.Unmarshal(request.Params, &params) == nil {
					docText = params.TextDocument.Text
				}
			case "textDocument/didChange":
				var params struct {
					ContentChanges []struct {
						Text string `json:"text"`
					} `json:"contentChanges"`
				}
				if json.Unmarshal(request.Params, &params) == nil && len(params.ContentChanges) > 0 {
					docText = params.ContentChanges[0].Text
				}
			}
			continue
		}

		var result string
		switch request.Method {
		case "initialize":
			result = `{"capabilities":{"referencesProvider":true}}`
		case "textDocument/references":
			var params struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			result = "null"
			if json.Unmarshal(request.Params, &params) == nil {
				if logPath != "" {
					if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
						// Best effort: the log only counts requests, and
						// a fake server that failed to write one is not
						// worth failing the test it is serving.
						_, _ = f.WriteString("x")
						_ = f.Close()
					}
				}
				// A real server resolves the identity at the requested
				// position and returns every occurrence of it, regardless
				// of which occurrence was asked about. Simulate that by
				// returning every textual occurrence of the word in the
				// tracked document, not just one derived from the request.
				var locs []string
				for i, line := range strings.Split(docText, "\n") {
					for _, m := range wordRe.FindAllStringIndex(line, -1) {
						locs = append(locs, fmt.Sprintf(`{"uri":%q,"range":{"start":{"line":%d,"character":%d},"end":{"line":%d,"character":%d}}}`,
							params.TextDocument.URI, i, m[0], i, m[1]))
					}
				}
				result = "[" + strings.Join(locs, ",") + "]"
			}
		default:
			result = "null"
		}
		response := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, result))
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(response), response)
	}
}

func newReferencesDedupE2EManager(t *testing.T, root, logPath string) *lsp.Manager {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	autoLSP := false
	store := configtest.NewStore(t, &config.Config{
		Options: &config.Options{AutoLSP: &autoLSP},
		LSP: config.LSPs{"gopls": {
			Command:     exe,
			Args:        []string{"-test.run=^TestReferencesDedupHelperProcess$"},
			Env:         map[string]string{referencesDedupHelperProcessEnv: "1", "SENNIT_REFERENCES_DEDUP_LOG": logPath},
			FileTypes:   []string{"go"},
			RootMarkers: []string{"go.mod"},
			Timeout:     5,
		}},
	}, configtest.WithWorkingDir(root))
	manager := lsp.NewManager(store)
	t.Cleanup(func() { manager.StopAll(t.Context()) })
	return manager
}

// TestLSPReferencesSkipsCandidatesAlreadyCovered is the regression test
// for finding 1: querying every grep candidate one candidate at a time
// (the fix for finding A, preserved by TestLSPReferencesQueriesEveryCandidate
// above) meant querying every textual occurrence of a common local
// variable name separately, even though they all belong to the same
// identity — measured on this repo's own source, a name like "filePath"
// (100 grep candidates collapsing to far fewer distinct identities) went
// from 656ms to 10ms once a candidate whose position is already covered
// by an already-queried identity's answer is skipped instead of
// re-queried. Here "count" is declared once and used twice in the same
// function — one identity, three grep candidates — so a correct fix
// queries textDocument/references exactly once and still reports all
// three locations.
func TestLSPReferencesSkipsCandidatesAlreadyCovered(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/e2e\n\ngo 1.24\n"), 0o644))
	src := "package e2e\n\nfunc F() {\n\tcount := 0\n\tcount++\n\tcount++\n}\n"
	file := filepath.Join(root, "a.go")
	require.NoError(t, os.WriteFile(file, []byte(src), 0o644))

	logPath := filepath.Join(t.TempDir(), "requests.log")
	manager := newReferencesDedupE2EManager(t, root, logPath)

	tool := NewReferencesTool(manager, root)
	resp := runToolWith(t, tool, t.Context(), ReferencesToolName, ReferencesParams{Symbol: "count", Path: "."})

	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "Found 3 reference(s)",
		"all three occurrences of the same identity must still be merged into the result")

	logged, err := os.ReadFile(logPath)
	require.NoError(t, err, "textDocument/references must have been called at least once")
	require.Len(t, logged, 1,
		"grep found 3 candidates for the same identity; only the first should ever reach textDocument/references — "+
			"the other two are already covered by its answer, and re-querying them is the regression this closes")
}

// referencesAllCommentsHelperProcessEnv gates runReferencesAllCommentsLSPHelper
// below: a fake server that reports every position as "not an identifier",
// simulating grep matches that all landed in comments or strings.
const referencesAllCommentsHelperProcessEnv = "SENNIT_REFERENCES_ALL_COMMENTS_HELPER"

// TestReferencesAllCommentsHelperProcess is the fake LSP server subprocess
// for TestLSPReferencesEmptyResultStillMentionsTruncation below.
func TestReferencesAllCommentsHelperProcess(t *testing.T) {
	if os.Getenv(referencesAllCommentsHelperProcessEnv) != "1" {
		return
	}
	runReferencesAllCommentsLSPHelper()
	os.Exit(0)
}

func runReferencesAllCommentsLSPHelper() {
	r := bufio.NewReader(os.Stdin)
	for {
		body, err := readLSPToolFrame(r)
		if err != nil {
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(body, &request) != nil || len(request.ID) == 0 {
			continue
		}

		var response []byte
		switch request.Method {
		case "initialize":
			response = []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"capabilities":{"referencesProvider":true}}}`, request.ID))
		case "textDocument/references":
			// Every candidate reports the same "not an identifier" failure
			// isNoIdentifierError matches on, standing in for every grep
			// match having landed in a comment or a string.
			response = []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"no identifier found"}}`, request.ID))
		default:
			response = []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":null}`, request.ID))
		}
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(response), response)
	}
}

func newReferencesAllCommentsE2EManager(t *testing.T, root string) *lsp.Manager {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	autoLSP := false
	store := configtest.NewStore(t, &config.Config{
		Options: &config.Options{AutoLSP: &autoLSP},
		LSP: config.LSPs{"gopls": {
			Command:     exe,
			Args:        []string{"-test.run=^TestReferencesAllCommentsHelperProcess$"},
			Env:         map[string]string{referencesAllCommentsHelperProcessEnv: "1"},
			FileTypes:   []string{"go"},
			RootMarkers: []string{"go.mod"},
			Timeout:     5,
		}},
	}, configtest.WithWorkingDir(root))
	manager := lsp.NewManager(store)
	t.Cleanup(func() { manager.StopAll(t.Context()) })
	return manager
}

// TestLSPReferencesEmptyResultStillMentionsTruncation is the regression
// test for finding 2: the truncation/unqueried note used to be appended
// only inside the `len(allLocations) > 0` branch, so a capped grep whose
// every candidate turned out not to be an identifier (a name whose first
// 100+ textual matches are all in comments and strings) fell through to
// "No references found" with no hint that the search never got past its
// match limit — a silently incomplete search read exactly like a
// definitively empty one.
func TestLSPReferencesEmptyResultStillMentionsTruncation(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/e2e\n\ngo 1.24\n"), 0o644))
	writeManySymbolMatches(t, root)
	// writeManySymbolMatches writes data.txt; give the fake server a real
	// .go file to open so gopls-style file handling in this manager has
	// something to route "count" grep matches through.
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package e2e\n"), 0o644))
	require.NoError(t, os.Rename(filepath.Join(root, "data.txt"), filepath.Join(root, "b.go")))

	manager := newReferencesAllCommentsE2EManager(t, root)
	tool := NewReferencesTool(manager, root)
	resp := runToolWith(t, tool, t.Context(), ReferencesToolName, ReferencesParams{Symbol: "count", Path: "."})

	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "No references found for symbol 'count'")
	require.Contains(t, resp.Content, "match limit",
		"a capped grep whose every candidate was 'not an identifier' must still say the search was truncated")
}
