// Package skills implements the Agent Skills open standard.
// See https://agentskills.io for the specification.
package skills

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/charlievieth/fastwalk"
	"github.com/rave-soft/sennit/internal/frontmatter"
	"gopkg.in/yaml.v3"
)

const (
	SkillFileName          = "SKILL.md"
	MaxNameLength          = 64
	MaxDescriptionLength   = 1024
	MaxCompatibilityLength = 500
)

var (
	namePattern    = regexp.MustCompile(`^[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*$`)
	promptReplacer = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
)

// Skill represents a parsed SKILL.md file.
type Skill struct {
	Name                   string            `yaml:"name" json:"name"`
	Description            string            `yaml:"description" json:"description"`
	UserInvocable          bool              `yaml:"user-invocable" json:"user_invocable"`
	DisableModelInvocation bool              `yaml:"disable-model-invocation" json:"disable_model_invocation"`
	License                string            `yaml:"license,omitempty" json:"license,omitempty"`
	Compatibility          string            `yaml:"compatibility,omitempty" json:"compatibility,omitempty"`
	Metadata               map[string]string `yaml:"metadata,omitempty" json:"metadata,omitempty"`
	Instructions           string            `yaml:"-" json:"instructions"`
	Path                   string            `yaml:"-" json:"path"`
	SkillFilePath          string            `yaml:"-" json:"skill_file_path"`
	Builtin                bool              `yaml:"-" json:"builtin"`
	// Source is the SKILL.md text this skill was parsed from. It is what
	// lets a skill be handed to another workspace and still be loadable
	// there: a thread reads its skills by their location, and an
	// inherited skill's file is not one the thread may open (see
	// DiscoveryConfig.InheritedSkills), so the text travels with it.
	Source string `yaml:"-" json:"-"`
}

// DiscoveryState represents the outcome of discovering a single skill file.
type DiscoveryState int

const (
	// StateNormal indicates the skill was parsed and validated successfully.
	StateNormal DiscoveryState = iota
	// StateError indicates discovery encountered a scan/parse/validate error.
	StateError
)

// SkillState represents the latest discovery status of a skill file.
type SkillState struct {
	Name  string
	Path  string
	State DiscoveryState
	Err   error
}

// Event is published when skill discovery completes.
type Event struct {
	States []*SkillState
}

// cloneStates returns a deep copy of the given state slice so callers cannot
// accidentally mutate the source.
func cloneStates(states []*SkillState) []*SkillState {
	if states == nil {
		return nil
	}
	result := make([]*SkillState, len(states))
	for i, s := range states {
		clone := *s
		result[i] = &clone
	}
	return result
}

// Validate checks if the skill meets spec requirements.
func (s *Skill) Validate() error {
	var errs []error

	if s.Name == "" {
		errs = append(errs, errors.New("name is required"))
	} else {
		if len(s.Name) > MaxNameLength {
			errs = append(errs, fmt.Errorf("name exceeds %d characters", MaxNameLength))
		}
		if !namePattern.MatchString(s.Name) {
			errs = append(errs, errors.New("name must be alphanumeric with hyphens, no leading/trailing/consecutive hyphens"))
		}
		if s.Path != "" && !strings.EqualFold(filepath.Base(s.Path), s.Name) {
			errs = append(errs, fmt.Errorf("name %q must match directory %q", s.Name, filepath.Base(s.Path)))
		}
	}

	if s.Description == "" {
		errs = append(errs, errors.New("description is required"))
	} else if len(s.Description) > MaxDescriptionLength {
		errs = append(errs, fmt.Errorf("description exceeds %d characters", MaxDescriptionLength))
	}

	if len(s.Compatibility) > MaxCompatibilityLength {
		errs = append(errs, fmt.Errorf("compatibility exceeds %d characters", MaxCompatibilityLength))
	}

	return errors.Join(errs...)
}

// Parse parses a SKILL.md file from disk.
func Parse(path string) (*Skill, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	skill, err := ParseContent(content)
	if err != nil {
		return nil, err
	}

	skill.Path = filepath.Dir(path)
	skill.SkillFilePath = path

	return skill, nil
}

// ParseContent parses a SKILL.md from raw bytes.
func ParseContent(content []byte) (*Skill, error) {
	header, body, err := frontmatter.Split(string(content))
	if err != nil {
		return nil, err
	}

	var skill Skill
	if err := yaml.Unmarshal([]byte(header), &skill); err != nil {
		return nil, fmt.Errorf("parsing frontmatter: %w", err)
	}

	skill.Instructions = strings.TrimSpace(body)
	skill.Source = string(content)

	return &skill, nil
}

// walkSkillFiles walks each of paths looking for SKILL.md files, the shared
// traversal DiscoverWithStates and scanSkillFiles both need.
//
// We use fastwalk with Follow: true instead of filepath.WalkDir because
// WalkDir doesn't follow symlinked directories at any depth—only entry
// points. This ensures skills in symlinked subdirectories are discovered.
// fastwalk is concurrent, so visit and onWalkErr must guard any shared state
// they touch.
//
// visit is called once per SKILL.md file found (err nil) and once per
// traversal error fastwalk reports for an entry (err non-nil, d may be nil);
// onWalkErr, if non-nil, is called when Walk itself returns a non-"not
// exist" error for a base path.
func walkSkillFiles(paths []string, visit func(base, path string, d os.DirEntry, err error) error, onWalkErr func(base string, err error)) {
	for _, base := range paths {
		conf := fastwalk.Config{
			Follow:  true,
			ToSlash: fastwalk.DefaultToSlash(),
		}
		err := fastwalk.Walk(&conf, base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return visit(base, path, d, err)
			}
			if d.IsDir() || d.Name() != SkillFileName {
				return nil
			}
			return visit(base, path, d, nil)
		})
		if err != nil && !os.IsNotExist(err) && onWalkErr != nil {
			onWalkErr(base, err)
		}
	}
}

// Discover finds all valid skills in the given paths.
func Discover(paths []string) []*Skill {
	skills, _ := DiscoverWithStates(paths)
	return skills
}

// DiscoverWithStates finds all valid skills in the given paths and also
// returns a per-file state slice describing parse/validation outcomes. Useful
// for diagnostics and UI reporting.
func DiscoverWithStates(paths []string) ([]*Skill, []*SkillState) {
	var skills []*Skill
	// bases parallels skills, recording which entry of paths produced each
	// skill, so the sort below can restore determinism within a path
	// without disturbing precedence across paths (see the sort below and
	// Deduplicate, which keeps the last occurrence).
	var bases []string
	var states []*SkillState
	var mu sync.Mutex
	seen := make(map[string]bool)
	addState := func(name, path string, state DiscoveryState, err error) {
		mu.Lock()
		states = append(states, &SkillState{
			Name:  name,
			Path:  path,
			State: state,
			Err:   err,
		})
		mu.Unlock()
	}

	walkSkillFiles(paths, func(base, path string, d os.DirEntry, err error) error {
		if err != nil {
			slog.Warn("Failed to walk skills path entry", "base", base, "path", path, "error", err)
			addState("", path, StateError, err)
			return nil
		}
		mu.Lock()
		if seen[path] {
			mu.Unlock()
			return nil
		}
		seen[path] = true
		mu.Unlock()
		skill, err := Parse(path)
		if err != nil {
			slog.Warn("Failed to parse skill file", "path", path, "error", err)
			addState("", path, StateError, err)
			return nil
		}
		if err := skill.Validate(); err != nil {
			slog.Warn("Skill validation failed", "path", path, "error", err)
			addState(skill.Name, path, StateError, err)
			return nil
		}
		slog.Debug("Successfully loaded skill", "name", skill.Name, "path", path)
		mu.Lock()
		skills = append(skills, skill)
		bases = append(bases, base)
		mu.Unlock()
		addState(skill.Name, path, StateNormal, nil)
		return nil
	}, func(base string, err error) {
		slog.Warn("Failed to walk skills path", "path", base, "error", err)
	})

	// walkSkillFiles walks paths in order, one at a time, so skills is
	// already grouped into contiguous runs by which entry of paths found
	// them. fastwalk's traversal within a single base is concurrent and so
	// non-deterministic, which is what needs sorting — but sorting must stay
	// inside each run: a caller's path order is precedence (Deduplicate
	// keeps the last occurrence), and comparing across runs would erase it.
	// So each run is sorted in place, by path then alphabetically by name,
	// leaving run boundaries untouched.
	start := 0
	for start < len(skills) {
		end := start + 1
		for end < len(skills) && bases[end] == bases[start] {
			end++
		}
		run := skills[start:end]
		slices.SortStableFunc(run, func(a, b *Skill) int {
			if c := strings.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path)); c != 0 {
				return c
			}
			return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		})
		start = end
	}

	return skills, states
}

// ToPromptXML generates XML for injection into the system prompt.
// Skills with DisableModelInvocation set to true are excluded.
func ToPromptXML(skills []*Skill) string {
	if len(skills) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("<available_skills>\n")
	for _, s := range skills {
		// Skip skills that have disable-model-invocation set
		if s.DisableModelInvocation {
			continue
		}
		sb.WriteString("  <skill>\n")
		fmt.Fprintf(&sb, "    <name>%s</name>\n", escape(s.Name))
		fmt.Fprintf(&sb, "    <description>%s</description>\n", escape(s.Description))
		fmt.Fprintf(&sb, "    <location>%s</location>\n", escape(s.SkillFilePath))
		if s.Builtin {
			sb.WriteString("    <type>builtin</type>\n")
		}
		sb.WriteString("  </skill>\n")
	}
	sb.WriteString("</available_skills>")
	return sb.String()
}

func escape(s string) string {
	return promptReplacer.Replace(s)
}

// DeduplicateStates removes duplicate skill states by name. When duplicates exist,
// the last occurrence wins (consistent with Deduplicate for skills).
func DeduplicateStates(all []*SkillState) []*SkillState {
	seen := make(map[string]int, len(all))
	for i, s := range all {
		if s.Name != "" {
			seen[s.Name] = i
		}
	}

	result := make([]*SkillState, 0, len(seen))
	for i, s := range all {
		// If it's the last occurrence of this name, or it has no name (error state), keep it
		if s.Name == "" || seen[s.Name] == i {
			result = append(result, s)
		}
	}
	return result
}

// Deduplicate removes duplicate skills by name. When duplicates exist, the
// last occurrence wins. This means user skills (appended after builtins)
// override builtin skills with the same name.
func Deduplicate(all []*Skill) []*Skill {
	seen := make(map[string]int, len(all))
	for i, s := range all {
		seen[s.Name] = i
	}

	result := make([]*Skill, 0, len(seen))
	for i, s := range all {
		if seen[s.Name] == i {
			result = append(result, s)
		}
	}
	return result
}

// ApproxTokenCount returns a rough estimate of how many tokens a string
// occupies when sent to an LLM. Uses the common ~4-chars-per-token heuristic
// that approximates GPT/Claude tokenizers well enough for diagnostic logging.
func ApproxTokenCount(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// Filter removes skills whose names appear in the disabled list.
func Filter(all []*Skill, disabled []string) []*Skill {
	if len(disabled) == 0 {
		return all
	}

	disabledSet := make(map[string]bool, len(disabled))
	for _, name := range disabled {
		disabledSet[name] = true
	}

	result := make([]*Skill, 0, len(all))
	for _, s := range all {
		if !disabledSet[s.Name] {
			result = append(result, s)
		}
	}
	return result
}
