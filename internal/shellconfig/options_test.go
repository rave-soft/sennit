package shellconfig

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOption_Bool(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option debug true
option progress false`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, true, opts["debug"])
	require.Equal(t, false, opts["progress"])
}

func TestOption_BoolCaseInsensitive(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option debug TRUE
option progress False
option metrics YES`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, true, opts["debug"])
	require.Equal(t, false, opts["progress"])
	require.Equal(t, false, opts["disable_metrics"])
}

func TestOption_String(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option data-directory .sennit
option notifications osc`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, ".sennit", opts["data_directory"])
	require.Equal(t, "osc", opts["notifications"])
}

func TestOption_UIKeybinding(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sennitrc")
	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(`option ui keybinding commands super+p
option ui keybinding editor.newline shift+enter super+j`))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))
	ui := result["options"].(map[string]any)["tui"].(map[string]any)
	bindings := ui["keybindings"].(map[string]any)
	require.Equal(t, []any{"super+p"}, bindings["commands"])
	require.Equal(t, []any{"shift+enter", "super+j"}, bindings["editor.newline"])
}

func TestOption_UIKeybindingRequiresKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sennitrc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option ui keybinding commands`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "keybinding <action> <key>")
}

func TestOption_List(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option context-path .cursorrules
option context-path SENNIT.md`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	paths := opts["context_paths"].([]any)
	require.Len(t, paths, 2)
	require.Equal(t, ".cursorrules", paths[0])
	require.Equal(t, "SENNIT.md", paths[1])
}

func TestOption_Reset(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option skill-path ./a
option skill-path ./b
option reset skill-path`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Empty(t, opts["skills_paths"].([]any))
}

func TestOption_ResetThenReadd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option skill-path ./inherited-a
option skill-path ./inherited-b
option reset skill-path
option skill-path ./mine`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	paths := opts["skills_paths"].([]any)
	require.Len(t, paths, 1)
	require.Equal(t, "./mine", paths[0])
}

func TestOption_ResetUnknownKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option reset bogus-key`
	path := filepath.Join(dir, "sennitrc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key")
}

func TestOption_ResetNonListKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option reset debug`
	path := filepath.Join(dir, "sennitrc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not one")
}

func TestOption_UIUnknownKey(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sennitrc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option ui bogus true`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key")
}

func TestOption_BoolShorthand(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option debug
option metrics`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, true, opts["debug"])
	require.Equal(t, false, opts["disable_metrics"])
}

func TestOption_InvertedBool(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option metrics false`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, true, opts["disable_metrics"])
}

func TestOption_Int(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option history-retention-days 30`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, float64(30), opts["history_retention_days"])
}

func TestOption_IntZero(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option history-retention-days 0`
	path := filepath.Join(dir, "sennitrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, float64(0), opts["history_retention_days"])
}

func TestOption_IntInvalid(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option history-retention-days abc`
	path := filepath.Join(dir, "sennitrc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects an integer")
}

// TestOption_UIBoolFields pins "option ui compact|transparent <bool>",
// currently handled by handleOption's special-cased switch in optionUI.
func TestOption_UIBoolFields(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `option ui compact true
option ui transparent true`)

	ui := result["options"].(map[string]any)["tui"].(map[string]any)
	require.Equal(t, true, ui["compact_mode"])
	require.Equal(t, true, ui["transparent"])
}

// TestOption_UICompactRequiresValue pins that, unlike top-level bool
// options, "option ui compact" has no bare-flag shorthand: a value is
// mandatory.
func TestOption_UICompactRequiresValue(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sennitrc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option ui compact`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "<value>")
}

// TestOption_UICompactEmptyValueRejected pins that an explicit empty value
// is still validated as a boolean, not treated as the bare-flag shorthand.
func TestOption_UICompactEmptyValueRejected(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sennitrc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option ui compact ""`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects true/false")
}

func TestOption_UIDiff(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `option ui diff split`)
	ui := result["options"].(map[string]any)["tui"].(map[string]any)
	require.Equal(t, "split", ui["diff_mode"])
}

func TestOption_UIDiffInvalid(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sennitrc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option ui diff bogus`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects unified or split")
}

func TestOption_UIScrollbar(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `option ui scrollbar always`)
	ui := result["options"].(map[string]any)["tui"].(map[string]any)
	require.Equal(t, "always", ui["scrollbar"])
}

func TestOption_UIScrollbarInvalid(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sennitrc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option ui scrollbar bogus`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects default, always, or never")
}

func TestOption_UICompletions(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `option ui completions-max-depth 3
option ui completions-max-items 10`)
	completions := result["options"].(map[string]any)["tui"].(map[string]any)["completions"].(map[string]any)
	require.Equal(t, float64(3), completions["max_depth"])
	require.Equal(t, float64(10), completions["max_items"])
}

// TestOption_UICompletionsNegativeRejected pins that ui completions ints
// reject negative values, unlike the generic top-level optInt fields
// (e.g. history-retention-days) which do not.
func TestOption_UICompletionsNegativeRejected(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sennitrc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option ui completions-max-depth -1`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "non-negative integer")
}

// TestOption_AttributionTrailerStyle pins that setting the trailer style
// defaults attribution.generated_with to true when it hasn't already been
// set.
func TestOption_AttributionTrailerStyle(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `option attribution-trailer-style assisted-by`)
	attribution := result["options"].(map[string]any)["attribution"].(map[string]any)
	require.Equal(t, "assisted-by", attribution["trailer_style"])
	require.Equal(t, true, attribution["generated_with"])
}

// TestOption_AttributionTrailerStylePreservesExplicitGeneratedWith pins that
// an earlier explicit "attribution-generated-with false" is not clobbered by
// a later "attribution-trailer-style" default.
func TestOption_AttributionTrailerStylePreservesExplicitGeneratedWith(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `option attribution-generated-with false
option attribution-trailer-style assisted-by`)
	attribution := result["options"].(map[string]any)["attribution"].(map[string]any)
	require.Equal(t, false, attribution["generated_with"])
}

func TestOption_AttributionTrailerStyleInvalid(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sennitrc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option attribution-trailer-style bogus`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects none or assisted-by")
}

// TestOption_AttributionGeneratedWithShorthand pins that, like other
// top-level bool options, omitting the value defaults to true.
func TestOption_AttributionGeneratedWithShorthand(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `option attribution-generated-with`)
	attribution := result["options"].(map[string]any)["attribution"].(map[string]any)
	require.Equal(t, true, attribution["generated_with"])
}

func TestOption_UnknownKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option bogus-key value`
	path := filepath.Join(dir, "sennitrc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key")
}

// TestOption_AutoSummarizeIdle pins the three idle auto-summarize keys,
// which are the first options to nest under a block of their own outside
// attribution and the UI: they must land in options.auto_summarize_idle,
// not at the top of options.
func TestOption_AutoSummarizeIdle(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `option auto-summarize-idle true
option auto-summarize-idle-tokens 40000
option auto-summarize-idle-after 90s`)

	idle := result["options"].(map[string]any)["auto_summarize_idle"].(map[string]any)
	require.Equal(t, true, idle["enabled"])
	require.Equal(t, float64(40000), idle["context_tokens"])
	require.Equal(t, "90s", idle["after"])
}

// TestOption_AutoSummarizeIdleAfterRejectsGarbage: a duration that does not
// parse is an error at the line that wrote it, not a value silently ignored
// later by whatever reads it.
func TestOption_AutoSummarizeIdleAfterRejectsGarbage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sennitrc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option auto-summarize-idle-after 4 minutes`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects a positive duration")
}
