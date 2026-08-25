package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/clipboard"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/toolmeta"
)

// Severity classifies how urgently a Problem needs attention.
type Severity string

const (
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// Area names the configuration subsystem a Problem was found in.
type Area string

const (
	AreaAgent       Area = "agent"
	AreaProvider    Area = "provider"
	AreaModel       Area = "model"
	AreaSkill       Area = "skill"
	AreaMCP         Area = "mcp"
	AreaPermission  Area = "permission"
	AreaEnvironment Area = "environment"
	AreaTUI         Area = "tui"
)

// Problem describes one thing that is wrong, or worth flagging, in the
// loaded configuration. `sennit doctor`, the TUI's /doctor dialog, and the
// sennit_info tool all render the same list, so an agent asked "why is my
// sub-agent on the wrong model?" can answer the question from its own
// tool output instead of a log file the user never sees.
type Problem struct {
	Severity Severity `json:"severity"`
	Area     Area     `json:"area"`
	Subject  string   `json:"subject"`
	Message  string   `json:"message"`
	Hint     string   `json:"hint,omitempty"`
}

// addProblem records a Problem found while loading or setting up the
// config. It is called from the same sites that used to only slog.Warn, so
// the user-facing text and the doctor's list stay in sync by construction
// instead of drifting apart as two separate descriptions of the same check.
func (c *Config) addProblem(p Problem) {
	c.Problems = append(c.Problems, p)
}

// Doctor collects every known configuration problem: the ones recorded
// while the config was loaded (a provider dropped for a missing api key, an
// agent model that did not resolve, a main model that fell back to a
// default, ...) plus a set of stateless checks run against the final,
// already-merged config (unknown tool names, a reasoning_effort a model
// does not support).
//
// It never makes network calls: whether a provider's api key or endpoint
// actually works is `sennit models --refresh` / the TUI's "Test Connection",
// not this. MCP server health is likewise not checked here — the caller
// (sennit_info, the doctor CLI/dialog) merges in MCP registry state
// separately, since internal/config cannot import the MCP client package
// without an import cycle.
func Doctor(cfg *Config) []Problem {
	if cfg == nil {
		return nil
	}
	problems := slices.Clone(cfg.Problems)
	problems = append(problems, doctorAgentReasoning(cfg)...)
	problems = append(problems, doctorToolNames(cfg)...)
	problems = append(problems, doctorPermissionsBypass(cfg)...)
	problems = append(problems, doctorJunkModelIDs(cfg)...)
	problems = append(problems, doctorSpinnerMode(cfg)...)
	return problems
}

// doctorSpinnerMode flags an options.tui.spinner the UI does not know.
// The value is not validated at load time on purpose: an unreadable
// motion setting is no reason to refuse to start, so SpinnerMode falls
// back to the default and the mistake is reported here instead — which is
// the only place it would otherwise be visible, since a spinner that
// silently ignores its setting looks exactly like one that was never
// configured.
func doctorSpinnerMode(cfg *Config) []Problem {
	if _, ok := cfg.SpinnerMode(); ok {
		return nil
	}
	return []Problem{{
		Severity: SeverityWarn,
		Area:     AreaTUI,
		Subject:  "spinner",
		Message: fmt.Sprintf("unknown options.tui.spinner %q; using %q",
			cfg.Options.TUI.Spinner, SpinnerScramble),
		Hint: "one of: " + strings.Join(SpinnerModes, ", "),
	}}
}

// doctorJunkModelIDs flags a provider whose model list contains a
// filesystem path or a .gguf filename as a model ID — a llama.cpp
// /v1/models response echoing its --model flag verbatim (see
// discover.junkModelID), most likely copy-pasted by hand into
// providers.<id>.models before discovery started filtering it out. Purely
// a string check on the already-loaded config: no network, so it is cheap
// enough to run on every Doctor call.
//
// Providers with an explicit discover_models: false are skipped. There the
// model list is hand-written by definition, and a llama-server really does
// serve its single model under the --model path as the only ID that
// endpoint accepts — so the path is the user's deliberate, correct choice,
// and the hint below ("define models explicitly") is already followed with
// no way left to silence the warning.
func doctorJunkModelIDs(cfg *Config) []Problem {
	var problems []Problem
	for id, pc := range cfg.providersOrEmpty() {
		if pc.AutoDiscoverModels != nil && !*pc.AutoDiscoverModels {
			continue
		}
		for _, m := range pc.Models {
			if strings.HasPrefix(m.ID, "/") || strings.HasSuffix(strings.ToLower(m.ID), ".gguf") {
				problems = append(problems, Problem{
					Severity: SeverityWarn,
					Area:     AreaProvider,
					Subject:  id,
					Message:  fmt.Sprintf("provider %s has a model ID that looks like a file path (%q), not a model name", id, m.ID),
					Hint:     "likely a llama.cpp /v1/models response echoing --model; define models explicitly or run `sennit models refresh`",
				})
				break
			}
		}
	}
	return problems
}

// doctorAgentReasoning flags agents whose reasoning_effort is set on a
// model that does not support reasoning at all. A per-level mismatch (e.g.
// asking for "high" on a model that only offers "low"/"medium") is silently
// corrected at call time (see internal/agent/providers.go) and is not worth
// surfacing; a model that cannot reason at all ignoring the setting
// entirely is worth a warning.
func doctorAgentReasoning(cfg *Config) []Problem {
	var problems []Problem
	providers := cfg.providersOrEmpty()
	for id, agent := range cfg.Agents {
		if agent.ReasoningEffort == "" {
			continue
		}

		var model *catwalk.Model
		switch {
		case agent.Model != "":
			if match, err := ResolveModelString(providers, agent.Model); err == nil {
				model = cfg.GetModel(match.Provider, match.ModelID)
			}
		default:
			model = cfg.GetModel(cfg.Model.Provider, cfg.Model.Model)
		}
		if model == nil || model.CanReason {
			continue
		}

		problems = append(problems, Problem{
			Severity: SeverityWarn,
			Area:     AreaAgent,
			Subject:  id,
			Message: fmt.Sprintf(
				"agent %s: reasoning_effort %q is set but model %s does not support reasoning",
				id, agent.ReasoningEffort, model.ID,
			),
			Hint: "reasoning_effort is ignored for this model; drop it or pin the agent to a reasoning-capable model",
		})
	}
	return problems
}

// doctorToolNames flags disabled_tools / allowed_tools entries that do not
// match any known built-in tool or user-defined agent (agents are callable
// as pseudo-tools from the coder). MCP tool names (mcp_<server>_<tool>) are
// only known once the registry has connected, so they are not validated
// here — a typo'd MCP tool name is a silent no-op the same way it always
// was, not a new false positive.
func doctorToolNames(cfg *Config) []Problem {
	known := make(map[string]bool)
	for _, name := range toolmeta.NamesAll() {
		known[name] = true
	}
	for id := range cfg.Agents {
		known[id] = true
	}
	// Aliases are accepted and folded onto their canonical names when config
	// is read, so doctor must not warn about a name that works.
	for _, alias := range toolmeta.AliasNames() {
		known[alias] = true
	}
	isKnown := func(name string) bool {
		return known[name] || strings.HasPrefix(name, "mcp_")
	}

	var problems []Problem
	if cfg.Options != nil {
		for _, name := range cfg.Options.DisabledTools {
			if isKnown(name) {
				continue
			}
			problems = append(problems, Problem{
				Severity: SeverityWarn,
				Area:     AreaAgent,
				Subject:  "options.disabled_tools",
				Message:  fmt.Sprintf("disabled_tools references unknown tool %q", name),
				Hint:     "check for a typo; unknown names are silently ignored",
			})
		}
	}

	for id, agent := range cfg.Agents {
		for _, name := range agent.AllowedTools {
			if isKnown(name) {
				continue
			}
			problems = append(problems, Problem{
				Severity: SeverityWarn,
				Area:     AreaAgent,
				Subject:  id,
				Message:  fmt.Sprintf("agent %s: allowed_tools references unknown tool %q", id, name),
				Hint:     "check for a typo; unknown names are silently ignored",
			})
		}
	}
	return problems
}

// doctorPermissionsBypass flags a persistently enabled permissions.bypass,
// since it silently disables every permission prompt for the life of the
// process — the same effect as always running with --yolo, but easy to
// forget about once it is checked into sennit.json.
func doctorPermissionsBypass(cfg *Config) []Problem {
	if cfg.Permissions == nil || !cfg.Permissions.Bypass {
		return nil
	}
	return []Problem{{
		Severity: SeverityWarn,
		Area:     AreaPermission,
		Subject:  "permissions.bypass",
		Message:  "permissions bypass is enabled — the agent never asks for permission before running a tool",
		Hint:     "disable permissions.bypass in sennit.json (or `permissions bypass off` in sennitrc) to restore prompts",
	}}
}

// EnvironmentProblems reports what is missing from the machine rather than
// from the config: right now, the clipboard helper that lets a paste carry
// the images of a rich selection instead of only its text.
//
// It is deliberately not part of [Doctor], which answers "is this config
// right?" from the config alone and stays reproducible anywhere. Callers
// merge this in the same way they merge MCP state and SkillProblems.
//
// It answers empty under test, because the machine is the input here: every
// test that asserts a clean problem list through a caller (`sennit doctor`,
// the TUI dialog, sennit_info) would otherwise pass or fail on whether the
// machine running it happens to have a clipboard helper — CI images do not.
// The check itself is exercised directly via environmentProblems.
func EnvironmentProblems() []Problem {
	if testing.Testing() {
		return nil
	}
	return environmentProblems()
}

// environmentProblems is the check itself, split out so the package's own
// tests reach it without the testing.Testing guard above.
func environmentProblems() []Problem {
	missing := clipboard.MissingHTMLHelpers()
	if len(missing) == 0 {
		return nil
	}
	return []Problem{{
		Severity: SeverityWarn,
		Area:     AreaEnvironment,
		Subject:  "clipboard",
		Message: "no clipboard helper installed — pasting a mixed selection " +
			"(text plus images) keeps only the text",
		Hint: "install " + strings.Join(missing, " or ") + " to paste images from a browser or document",
	}}
}

// SkillProblems reports every SKILL.md that failed to parse or validate
// during discovery, so a broken skill shows up wherever the other config
// problems do (`sennit doctor`, the TUI dialog, sennit_info) instead of
// only as a WARN line in the log.
//
// It matters more than a config typo usually does: a skill that fails to
// parse is not loaded at all, yet nothing else changes. Agents told to
// follow it simply proceed without it, doing whatever they would have done
// with no skill present, and the work looks like it followed the process
// right up until someone reads the output closely. The most common cause is
// an unquoted colon in the frontmatter's description, which YAML reads as a
// nested mapping.
//
// States are passed in rather than discovered here because discovery is
// per-workspace and already done by the time anything asks for problems;
// this only translates its outcome. A nil or empty snapshot yields no
// problems.
func SkillProblems(states []*skills.SkillState) []Problem {
	var problems []Problem
	for _, st := range states {
		if st == nil || st.State != skills.StateError {
			continue
		}
		subject := st.Name
		if subject == "" {
			// A skill that failed to parse has no name yet — its own
			// frontmatter is what did not load — so fall back to the
			// directory, which is what the person sees on disk.
			subject = filepath.Base(filepath.Dir(st.Path))
		}
		msg := fmt.Sprintf("skill %s failed to load", subject)
		if st.Err != nil {
			msg += ": " + st.Err.Error()
		}
		problems = append(problems, Problem{
			Severity: SeverityError,
			Area:     AreaSkill,
			Subject:  subject,
			Message:  msg,
			Hint:     "fix " + st.Path + " — a description containing \": \" must be quoted, and name/description are both required",
		})
	}
	return problems
}
