package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/stretchr/testify/require"
	"gopkg.in/dnaeon/go-vcr.v4/pkg/cassette"
	"gopkg.in/dnaeon/go-vcr.v4/pkg/recorder"
)

const (
	vcrModeRecord       = "record"
	vcrModeFixture      = "fixture"
	cassetteAuthority   = "http://vcr.test"
	defaultFixtureModel = "glm-5.1"
)

type testVCRConfig struct {
	Mode         recorder.Mode
	CassetteRoot string
	BaseURL      string
	Model        string
}

func resolveTestVCRConfig(mode, cassetteRoot, baseURL, model string) (testVCRConfig, error) {
	cfg := testVCRConfig{Mode: recorder.ModeReplayOnly, CassetteRoot: cassetteRoot, BaseURL: cassetteAuthority + "/v1", Model: defaultFixtureModel}
	switch mode {
	case "":
		return cfg, nil
	case vcrModeFixture:
		if cassetteRoot == "" {
			return testVCRConfig{}, fmt.Errorf("fixture mode requires an absolute SENNIT_TEST_CASSETTE_ROOT")
		}
		if !filepath.IsAbs(cassetteRoot) {
			return testVCRConfig{}, fmt.Errorf("fixture mode requires absolute SENNIT_TEST_CASSETTE_ROOT, got %q", cassetteRoot)
		}
		cfg.Mode = recorder.ModeRecordOnly
		return cfg, nil
	case vcrModeRecord:
		if baseURL == "" || model == "" {
			return testVCRConfig{}, fmt.Errorf("record mode requires SENNIT_TEST_OPENAI_BASE_URL and SENNIT_TEST_OPENAI_MODEL")
		}
		cfg.Mode, cfg.BaseURL, cfg.Model = recorder.ModeRecordOnly, baseURL, model
		return cfg, nil
	default:
		return testVCRConfig{}, fmt.Errorf("invalid SENNIT_TEST_VCR_MODE %q (want %q, %q, or unset)", mode, vcrModeRecord, vcrModeFixture)
	}
}

func cassetteName(t *testing.T, model string) string {
	t.Helper()
	parts := strings.Split(t.Name(), "/")
	if len(parts) < 3 {
		t.Fatalf("unexpected TestCoderAgent name %q", t.Name())
	}
	return filepath.Join(parts[0], model, strings.ReplaceAll(parts[len(parts)-1], " ", "_"))
}

func newTestRecorder(t *testing.T, cfg testVCRConfig, name string) *recorder.Recorder {
	t.Helper()
	root := cfg.CassetteRoot
	if root == "" {
		var err error
		root, err = filepath.Abs("testdata")
		require.NoError(t, err)
	} else {
		require.True(t, filepath.IsAbs(root), "cassette root must be absolute")
	}
	r, err := recorder.New(filepath.Join(root, name), recorder.WithMode(cfg.Mode), recorder.WithMatcher(strictMatcher), recorder.WithSkipRequestLatency(true), recorder.WithHook(beforeSaveCanonicalAuthority, recorder.BeforeSaveHook), recorder.WithReplayableInteractions(true))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Stop() })
	return r
}

func beforeSaveCanonicalAuthority(i *cassette.Interaction) error {
	u, err := url.Parse(i.Request.URL)
	if err != nil {
		return fmt.Errorf("canonicalize cassette URL: %w", err)
	}
	authority := u.Scheme + "://" + u.Host
	i.Request.URL = cassetteAuthority + u.RequestURI()
	i.Request.Host = ""
	i.Request.Headers.Del("X-Session-ID")
	i.Request.Headers.Del("X-Session-Affinity")
	i.Request.Headers.Del("Authorization")
	i.Request.Body = strings.ReplaceAll(i.Request.Body, authority, cassetteAuthority)
	i.Response.Body = strings.ReplaceAll(i.Response.Body, authority, cassetteAuthority)
	i.Response.Headers.Del("Date")
	i.Response.Duration = 0
	return nil
}

func strictMatcher(r *http.Request, i cassette.Request) bool {
	cassetteURL, err := url.Parse(i.URL)
	if err != nil || cassetteURL.Scheme+"://"+cassetteURL.Host != cassetteAuthority || r.Method != i.Method || r.URL.Path != cassetteURL.Path || r.URL.RawQuery != cassetteURL.RawQuery {
		return false
	}
	return jsonBodyEqual([]byte(i.Body), r)
}

func jsonBodyEqual(want []byte, req *http.Request) bool {
	got, err := io.ReadAll(req.Body)
	if err != nil {
		return false
	}
	req.Body = io.NopCloser(bytes.NewReader(got))
	if len(want) == 0 || len(got) == 0 {
		return bytes.Equal(want, got)
	}
	var wantJSON, gotJSON any
	if json.Unmarshal(want, &wantJSON) != nil || json.Unmarshal(got, &gotJSON) != nil {
		return false
	}
	return reflect.DeepEqual(normalizeForMatch(wantJSON), normalizeForMatch(gotJSON))
}

// normalizeForMatch strips the parts of a request that are prose rather than
// behavior: the system prompt's text and every tool/parameter description.
//
// What these cassettes are for is the agent's loop — which tools it is
// offered, which it calls, with what arguments, and how it folds the results
// back into the conversation. None of that is what the prose decides, but
// under a byte-exact match the prose decides whether the test runs at all: a
// reworded sentence in a skill's front matter or a tool description (both of
// which reach the request through the system prompt and the tools array)
// stops every interaction from matching, and the fix would be re-recording
// the whole suite against a paid endpoint for a copy edit. That trade is
// backwards, and in practice it left the suite red rather than re-recorded.
//
// Everything that is behavior stays strict: the model, sampling parameters,
// the set of tool names and their JSON schemas, and every non-system message
// including the assistant's tool calls and their results.
func normalizeForMatch(body any) any {
	root, ok := body.(map[string]any)
	if !ok {
		return body
	}
	// Shallow copy: the callers' decoded bodies are not ours to mutate.
	out := make(map[string]any, len(root))
	for k, v := range root {
		out[k] = v
	}

	if messages, ok := out["messages"].([]any); ok {
		normalized := make([]any, 0, len(messages))
		for _, raw := range messages {
			message, ok := raw.(map[string]any)
			if !ok {
				normalized = append(normalized, raw)
				continue
			}
			if message["role"] != "system" {
				normalized = append(normalized, message)
				continue
			}
			// Kept as a message, so a prompt that disappears entirely (or
			// gains a second system message) still fails to match.
			normalized = append(normalized, map[string]any{"role": "system", "content": "<system prompt>"})
		}
		out["messages"] = normalized
	}
	if messages, ok := out["messages"].([]any); ok {
		out["messages"] = normalizeToolBatchOrder(messages)
	}
	if tools, ok := out["tools"]; ok {
		out["tools"] = stripDescriptions(tools)
	}
	return out
}

// normalizeToolBatchOrder canonicalizes every complete, valid assistant
// tool-call batch independently. Fantasy can append concurrent results in
// completion order, but their identity is the call ID. Results are reordered
// only when they exactly match the unique IDs declared by their own batch; all
// non-tool messages retain their slots. Any non-empty tool_calls declaration,
// even malformed, closes the previous batch and prevents results from crossing
// into it.
func normalizeToolBatchOrder(messages []any) []any {
	out := slices.Clone(messages)
	for start := 0; start < len(messages); {
		callIDs, valid := toolCallBatch(messages[start])
		if !valid {
			start++
			continue
		}

		end := len(messages)
		for i := start + 1; i < len(messages); i++ {
			if hasToolCallBoundary(messages[i]) {
				end = i
				break
			}
		}
		normalizeToolBatch(out, messages, start+1, end, callIDs)
		start = end
	}
	return out
}

// hasToolCallBoundary identifies any assistant message with present, non-empty
// tool_calls. It intentionally accepts malformed declarations as boundaries:
// they must close the preceding valid batch, but are not normalized themselves.
func hasToolCallBoundary(raw any) bool {
	message, ok := raw.(map[string]any)
	if !ok || message["role"] != "assistant" {
		return false
	}
	calls, present := message["tool_calls"]
	if !present || calls == nil {
		return false
	}
	switch calls := calls.(type) {
	case []any:
		return len(calls) > 0
	case map[string]any:
		return len(calls) > 0
	case string:
		return calls != ""
	default:
		return true
	}
}

// toolCallBatch returns unique, valid call IDs declared by one assistant
// message. A malformed declaration is not normalized, while still acting as a
// boundary through hasToolCallBoundary.
func toolCallBatch(raw any) ([]string, bool) {
	if !hasToolCallBoundary(raw) {
		return nil, false
	}
	message := raw.(map[string]any)
	calls, ok := message["tool_calls"].([]any)
	if !ok {
		return nil, false
	}
	ids := make([]string, 0, len(calls))
	seen := make(map[string]struct{}, len(calls))
	for _, rawCall := range calls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			return nil, false
		}
		id, ok := call["id"].(string)
		if !ok || id == "" {
			return nil, false
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, false
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, true
}

func normalizeToolBatch(out, messages []any, start, end int, callIDs []string) {
	resultIdx := make([]int, 0, len(callIDs))
	resultIDs := make([]string, 0, len(callIDs))
	for i := start; i < end; i++ {
		message, ok := messages[i].(map[string]any)
		if !ok || message["role"] != "tool" {
			continue
		}
		id, ok := message["tool_call_id"].(string)
		if !ok || id == "" {
			return
		}
		resultIdx = append(resultIdx, i)
		resultIDs = append(resultIDs, id)
	}
	if len(resultIdx) < 2 || !sameIDSet(callIDs, resultIDs) {
		return
	}

	sortedIDs := slices.Clone(resultIDs)
	slices.Sort(sortedIDs)
	byID := make(map[string]any, len(resultIDs))
	for i, id := range resultIDs {
		byID[id] = messages[resultIdx[i]]
	}
	for i, id := range sortedIDs {
		out[resultIdx[i]] = byID[id]
	}
}

// sameIDSet reports whether a and b hold the same unique, non-empty call IDs.
// Duplicates make a batch malformed: canonicalizing through an ID-keyed map
// would otherwise discard one result's content.
func sameIDSet(a, b []string) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, id := range a {
		if id == "" {
			return false
		}
		if _, duplicate := seen[id]; duplicate {
			return false
		}
		seen[id] = struct{}{}
	}
	for _, id := range b {
		if id == "" {
			return false
		}
		if _, found := seen[id]; !found {
			return false
		}
		delete(seen, id)
	}
	return len(seen) == 0
}

// stripDescriptions removes every "description" key, at any depth. Tool
// descriptions and their per-parameter documentation are written for the
// model to read, and are edited constantly.
func stripDescriptions(node any) any {
	switch typed := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, v := range typed {
			if k == "description" {
				continue
			}
			out[k] = stripDescriptions(v)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, v := range typed {
			out[i] = stripDescriptions(v)
		}
		return out
	default:
		return node
	}
}

type modelPair struct{ name string }

var modelPairs = []modelPair{{name: defaultFixtureModel}}

func getModel(t *testing.T, transport http.RoundTripper, baseURL, model string) fantasy.LanguageModel {
	t.Helper()
	provider, err := openaicompat.New(openaicompat.WithBaseURL(baseURL), openaicompat.WithHTTPClient(&http.Client{Transport: transport}))
	require.NoError(t, err)
	lm, err := provider.LanguageModel(t.Context(), model)
	require.NoError(t, err)
	return lm
}

func TestTestVCRConfig(t *testing.T) {
	cfg, err := resolveTestVCRConfig("", "", "ignored", "ignored")
	require.NoError(t, err)
	require.Equal(t, recorder.ModeReplayOnly, cfg.Mode)
	_, err = resolveTestVCRConfig(vcrModeRecord, "", "", "")
	require.Error(t, err)
	_, err = resolveTestVCRConfig("bad", "", "", "")
	require.Error(t, err)
	_, err = resolveTestVCRConfig(vcrModeFixture, "relative/testdata", "", "")
	require.ErrorContains(t, err, "absolute SENNIT_TEST_CASSETTE_ROOT")
	root, err := filepath.Abs(t.TempDir())
	require.NoError(t, err)
	cfg, err = resolveTestVCRConfig(vcrModeFixture, root, "", "")
	require.NoError(t, err)
	require.Equal(t, root, cfg.CassetteRoot)
	cfg, err = resolveTestVCRConfig(vcrModeRecord, root, "http://example.test/v1", "model")
	require.NoError(t, err)
	require.Equal(t, recorder.ModeRecordOnly, cfg.Mode)
}

func TestVCRRecordThenReplayIsStrictAndOffline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) }))
	root := t.TempDir()
	recordConfig := testVCRConfig{Mode: recorder.ModeRecordOnly, CassetteRoot: root, BaseURL: server.URL, Model: "model"}
	record := newTestRecorder(t, recordConfig, "strict")
	request := func(method, target, body string) *http.Request {
		req, err := http.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
		require.NoError(t, err)
		return req
	}
	response, err := record.GetDefaultClient().Do(request(http.MethodPost, server.URL+"/v1/chat?x=1", `{"model":"model","prompt":"p","tools":[{"name":"t","args":{"x":1}}]}`))
	require.NoError(t, err)
	response.Body.Close()
	require.NoError(t, record.Stop())
	cassetteBytes, err := os.ReadFile(filepath.Join(root, "strict.yaml"))
	require.NoError(t, err)
	require.NotContains(t, string(cassetteBytes), server.URL)
	require.Contains(t, string(cassetteBytes), "duration: 0s")
	server.Close()

	for range 2 {
		replay := newTestRecorder(t, testVCRConfig{Mode: recorder.ModeReplayOnly, CassetteRoot: root}, "strict")
		response, err = replay.GetDefaultClient().Do(request(http.MethodPost, cassetteAuthority+"/v1/chat?x=1", `{"tools":[{"args":{"x":1},"name":"t"}],"prompt":"p","model":"model"}`))
		require.NoError(t, err)
		response.Body.Close()
		require.NoError(t, replay.Stop())
	}
	for _, target := range []string{cassetteAuthority + "/v1/chat?x=2", cassetteAuthority + "/v1/other?x=1"} {
		replay := newTestRecorder(t, testVCRConfig{Mode: recorder.ModeReplayOnly, CassetteRoot: root}, "strict")
		response, requestErr := replay.GetDefaultClient().Do(request(http.MethodPost, target, `{"model":"model","prompt":"p","tools":[{"name":"t","args":{"x":1}}]}`))
		if response != nil {
			response.Body.Close()
		}
		require.Error(t, requestErr)
		_ = replay.Stop()
	}
	replay := newTestRecorder(t, testVCRConfig{Mode: recorder.ModeReplayOnly, CassetteRoot: root}, "strict")
	response, err = replay.GetDefaultClient().Do(request(http.MethodGet, cassetteAuthority+"/v1/chat?x=1", ``))
	if response != nil {
		response.Body.Close()
	}
	require.Error(t, err)
	response, err = replay.GetDefaultClient().Do(request(http.MethodPost, cassetteAuthority+"/v1/chat?x=1", `{"model":"other","prompt":"p","tools":[{"name":"t","args":{"x":1}}]}`))
	if response != nil {
		response.Body.Close()
	}
	require.Error(t, err)
}

// The normalizeToolBatchOrder cases below pin the three boundaries the
// normalizer must not cross: results of the same parallel batch may be
// reordered among themselves, but nothing across a turn boundary, nothing
// that the preceding assistant batch did not declare, and no message may be
// reordered on the strength of a missing tool_call_id.
func TestNormalizeToolBatchOrder(t *testing.T) {
	user := map[string]any{"role": "user", "content": "find the files and list the directory"}
	assistantBatch := func(ids ...string) map[string]any {
		calls := make([]any, 0, len(ids))
		for i, id := range ids {
			calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "tool" + strconv.Itoa(i), "arguments": "{}"}})
		}
		return map[string]any{"role": "assistant", "content": nil, "tool_calls": calls}
	}
	toolMsg := func(id, content string) map[string]any {
		return map[string]any{"role": "tool", "tool_call_id": id, "content": content}
	}

	t.Run("same batch, different arrival order, equivalent", func(t *testing.T) {
		a := normalizeToolBatchOrder([]any{
			user,
			assistantBatch("call_b", "call_a"),
			toolMsg("call_a", "result a"),
			toolMsg("call_b", "result b"),
		})
		b := normalizeToolBatchOrder([]any{
			user,
			assistantBatch("call_b", "call_a"),
			toolMsg("call_b", "result b"),
			toolMsg("call_a", "result a"),
		})
		require.True(t, reflect.DeepEqual(a, b), "reordering within one batch must be order-independent")
		// And the canonical order really is sorted by call ID, not the
		// recorded order.
		wantOrder := []string{"call_a", "call_b"}
		for i, id := range wantOrder {
			require.Equal(t, id, a[len(a)-len(wantOrder)+i].(map[string]any)["tool_call_id"])
		}
	})

	t.Run("two batches normalize independently", func(t *testing.T) {
		// Both batches can complete in either order. Each is canonicalized
		// independently, rather than only canonicalizing the final batch.
		recA := normalizeToolBatchOrder([]any{
			user,
			assistantBatch("call_a", "call_b"),
			toolMsg("call_a", "a"), toolMsg("call_b", "b"),
			assistantBatch("call_c", "call_d"),
			toolMsg("call_c", "c"), toolMsg("call_d", "d"),
		})
		recB := normalizeToolBatchOrder([]any{
			user,
			assistantBatch("call_a", "call_b"),
			toolMsg("call_b", "b"), toolMsg("call_a", "a"),
			assistantBatch("call_c", "call_d"),
			toolMsg("call_d", "d"), toolMsg("call_c", "c"),
		})
		require.Equal(t, recA, recB)
	})

	t.Run("results do not cross batch boundaries", func(t *testing.T) {
		// The first batch is incomplete and must not borrow call_c from the
		// later batch merely because it appears before its own companion.
		in := []any{
			user,
			assistantBatch("call_a", "call_b"),
			toolMsg("call_b", "b"),
			assistantBatch("call_c", "call_d"),
			toolMsg("call_c", "c"), toolMsg("call_d", "d"),
		}
		out := normalizeToolBatchOrder(in)
		require.Equal(t, "call_b", out[2].(map[string]any)["tool_call_id"])
		require.Equal(t, "call_c", out[4].(map[string]any)["tool_call_id"])
		require.Equal(t, "call_d", out[5].(map[string]any)["tool_call_id"])
	})

	t.Run("malformed assistant tool calls close the preceding batch", func(t *testing.T) {
		malformedBoundary := map[string]any{"role": "assistant", "tool_calls": "invalid"}
		a := normalizeToolBatchOrder([]any{
			user,
			assistantBatch("call_a", "call_b"),
			toolMsg("call_a", "result a"),
			malformedBoundary,
			toolMsg("call_b", "result b"),
		})
		b := normalizeToolBatchOrder([]any{
			user,
			assistantBatch("call_a", "call_b"),
			toolMsg("call_b", "result b"),
			malformedBoundary,
			toolMsg("call_a", "result a"),
		})

		require.False(t, reflect.DeepEqual(a, b), "a malformed declaration must close, not join, the preceding batch")
		require.Equal(t, "call_a", a[2].(map[string]any)["tool_call_id"])
		require.Equal(t, "call_b", b[2].(map[string]any)["tool_call_id"])
	})

	t.Run("results split by an intervening message are still one batch", func(t *testing.T) {
		// fantasy may emit an assistant text alongside the tool results of
		// the same batch; the batch boundary is the assistant message, not
		// adjacency. In the first recording the earlier slot holds the
		// earlier call ID (already canonical); in the second the earlier
		// slot holds the later call ID — only a batch-aware sort makes the
		// two recordings equivalent.
		a := normalizeToolBatchOrder([]any{
			user,
			assistantBatch("call_b", "call_a"),
			toolMsg("call_a", "result a"),
			map[string]any{"role": "assistant", "content": "found both"},
			toolMsg("call_b", "result b"),
		})
		b := normalizeToolBatchOrder([]any{
			user,
			assistantBatch("call_b", "call_a"),
			toolMsg("call_b", "result b"),
			map[string]any{"role": "assistant", "content": "found both"},
			toolMsg("call_a", "result a"),
		})
		require.True(t, reflect.DeepEqual(a, b), "a split batch must normalize like an unsplit one")
	})

	t.Run("three way arrival orders all equivalent", func(t *testing.T) {
		ids := []string{"call_1", "call_2", "call_3"}
		contents := []string{"one", "two", "three"}
		run := func(order []int) any {
			msgs := []any{user, assistantBatch(ids...)}
			for _, i := range order {
				msgs = append(msgs, toolMsg(ids[i], contents[i]))
			}
			return normalizeToolBatchOrder(msgs)
		}
		ref := run([]int{0, 1, 2})
		for _, order := range [][]int{{1, 0, 2}, {2, 1, 0}, {2, 0, 1}} {
			require.True(t, reflect.DeepEqual(ref, run(order)), "order %v must normalize like %v", order, []int{0, 1, 2})
		}
	})

	t.Run("result not in the batch is not normalized away", func(t *testing.T) {
		// The run's IDs do not set-match the batch: "call_x" was never
		// declared and "call_b" is missing. The run must be left as-is, so
		// it does not match any recording that carried the real batch.
		rogue := normalizeToolBatchOrder([]any{
			user,
			assistantBatch("call_a", "call_b"),
			toolMsg("call_a", "result a"),
			toolMsg("call_x", "result x"),
		})
		legit := normalizeToolBatchOrder([]any{
			user,
			assistantBatch("call_a", "call_b"),
			toolMsg("call_a", "result a"),
			toolMsg("call_b", "result b"),
		})
		require.False(t, reflect.DeepEqual(rogue, legit), "a result for an undeclared call must not match the real batch")
	})

	t.Run("duplicate declared call IDs remain strict", func(t *testing.T) {
		a := normalizeToolBatchOrder([]any{
			user,
			assistantBatch("call_a", "call_a"),
			toolMsg("call_a", "first result"),
			toolMsg("call_a", "second result"),
		})
		b := normalizeToolBatchOrder([]any{
			user,
			assistantBatch("call_a", "call_a"),
			toolMsg("call_a", "second result"),
			toolMsg("call_a", "first result"),
		})

		require.False(t, reflect.DeepEqual(a, b), "duplicate declared IDs must not create a lossy equivalence")
		require.Equal(t, "first result", a[2].(map[string]any)["content"])
		require.Equal(t, "second result", a[3].(map[string]any)["content"])
	})

	t.Run("duplicate result IDs remain strict", func(t *testing.T) {
		a := normalizeToolBatchOrder([]any{
			user,
			assistantBatch("call_a", "call_b"),
			toolMsg("call_a", "first result"),
			toolMsg("call_a", "second result"),
		})
		b := normalizeToolBatchOrder([]any{
			user,
			assistantBatch("call_a", "call_b"),
			toolMsg("call_a", "second result"),
			toolMsg("call_a", "first result"),
		})

		require.False(t, reflect.DeepEqual(a, b), "duplicate result IDs must not be collapsed through a map")
		require.Equal(t, "first result", a[2].(map[string]any)["content"])
		require.Equal(t, "second result", a[3].(map[string]any)["content"])
	})

	t.Run("missing tool_call_id: no panic, no widened equivalence", func(t *testing.T) {
		// A tool message without an ID (malformed transcript) must not be
		// reordered with its neighbours — that would let a broken
		// transcript match a healthy recording.
		idless := normalizeToolBatchOrder([]any{
			user,
			assistantBatch("call_a", "call_b"),
			map[string]any{"role": "tool", "content": "no id"},
			toolMsg("call_a", "result a"),
		})
		require.Len(t, idless, 4)
		// Position unchanged: the ID-less message is still second-to-last.
		require.Nil(t, idless[2].(map[string]any)["tool_call_id"])
		require.Equal(t, "call_a", idless[3].(map[string]any)["tool_call_id"])

		// A run with an ID-less message is not equivalent to a run of two
		// real results with the same call IDs — different transcripts.
		twoResults := normalizeToolBatchOrder([]any{
			user,
			assistantBatch("call_a", "call_b"),
			toolMsg("call_a", "result a"),
			toolMsg("call_b", "result b"),
		})
		require.False(t, reflect.DeepEqual(idless, twoResults), "an ID-less tool message must not normalize into an equivalent of a real result")
	})

	t.Run("single result untouched", func(t *testing.T) {
		in := []any{user, assistantBatch("call_a"), toolMsg("call_a", "only")}
		out := normalizeToolBatchOrder(in)
		require.Equal(t, in, out, "a single result carries no order to normalize")
	})
}

// failFastOnCassetteMiss converts a replay miss into a synthetic 400
// response instead of letting it surface as a transport error.
//
// A cassette miss reaches the caller as a *url.Error, which implements
// net.Error, so fantasy's retry middleware classifies it as a transient
// network failure and re-runs the step three times with 5s/10s/20s backoff:
// every stale-cassette test burned 35 seconds before reporting a mismatch
// that could never fix itself (the whole -race suite ran 8m50s instead of
// ~1m30s that way). A 4xx is non-retryable, so the same failure is reported
// on the first attempt, with the cassette path in the message.
type failFastOnCassetteMiss struct{ inner http.RoundTripper }

func (f failFastOnCassetteMiss) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := f.inner.RoundTrip(req)
	if err == nil || !errors.Is(err, cassette.ErrInteractionNotFound) {
		return resp, err
	}
	body, marshalErr := json.Marshal(map[string]any{"error": map[string]any{
		"type": "vcr_cassette_miss",
		"message": fmt.Sprintf(
			"no recorded interaction matches %s %s: the cassette is out of date, re-record it (see SENNIT_TEST_VCR_MODE=fixture)",
			req.Method, req.URL,
		),
	}})
	if marshalErr != nil {
		return resp, err
	}
	return &http.Response{
		Status:        http.StatusText(http.StatusBadRequest),
		StatusCode:    http.StatusBadRequest,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}
