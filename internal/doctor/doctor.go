// Package doctor collects the problems that cannot be decided from a
// config file alone: what the machine is missing, and what discovery
// found broken on disk. They are reported next to the config problems
// [config.Doctor] finds — `sennit doctor`, the TUI dialog and sennit_info
// all merge the two — but they are not the same question, and keeping
// them apart is what lets internal/config stay a package about
// configuration rather than one that reaches into the clipboard and the
// skills loader to answer it.
package doctor

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rave-soft/sennit/internal/clipboard"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/skills"
)

// EnvironmentProblems reports what is missing from the machine rather than
// from the config: right now, the clipboard helper that lets a paste carry
// the images of a rich selection instead of only its text.
//
// It is deliberately not part of [config.Doctor], which answers "is this config
// right?" from the config alone and stays reproducible anywhere. Callers
// merge this in the same way they merge MCP state and SkillProblems.
func EnvironmentProblems() []config.Problem {
	return environmentProblems(clipboard.MissingHTMLHelpers())
}

func environmentProblems(missing []string) []config.Problem {
	if len(missing) == 0 {
		return nil
	}
	return []config.Problem{{
		Severity: config.SeverityWarn,
		Area:     config.AreaEnvironment,
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
func SkillProblems(states []*skills.SkillState) []config.Problem {
	var problems []config.Problem
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
		problems = append(problems, config.Problem{
			Severity: config.SeverityError,
			Area:     config.AreaSkill,
			Subject:  subject,
			Message:  msg,
			Hint:     "fix " + st.Path + " — a description containing \": \" must be quoted, and name/description are both required",
		})
	}
	return problems
}
