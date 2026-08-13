package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
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
			return testVCRConfig{}, fmt.Errorf("fixture mode requires an absolute BRAID_TEST_CASSETTE_ROOT")
		}
		if !filepath.IsAbs(cassetteRoot) {
			return testVCRConfig{}, fmt.Errorf("fixture mode requires absolute BRAID_TEST_CASSETTE_ROOT, got %q", cassetteRoot)
		}
		cfg.Mode = recorder.ModeRecordOnly
		return cfg, nil
	case vcrModeRecord:
		if baseURL == "" || model == "" {
			return testVCRConfig{}, fmt.Errorf("record mode requires BRAID_TEST_OPENAI_BASE_URL and BRAID_TEST_OPENAI_MODEL")
		}
		cfg.Mode, cfg.BaseURL, cfg.Model = recorder.ModeRecordOnly, baseURL, model
		return cfg, nil
	default:
		return testVCRConfig{}, fmt.Errorf("invalid BRAID_TEST_VCR_MODE %q (want %q, %q, or unset)", mode, vcrModeRecord, vcrModeFixture)
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
	return json.Unmarshal(want, &wantJSON) == nil && json.Unmarshal(got, &gotJSON) == nil && reflect.DeepEqual(wantJSON, gotJSON)
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
	require.ErrorContains(t, err, "absolute BRAID_TEST_CASSETTE_ROOT")
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
