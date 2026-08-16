package model

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"charm.land/lipgloss/v2"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

type skillStatusItem struct {
	icon  string
	name  string
	title string
	// description carries the discovery error for a skill that failed to
	// parse or validate. A broken SKILL.md is not loaded at all, and until
	// this was shown the only trace of that was a WARN in the log — so an
	// agent instructed to follow a skill would carry on without it, and
	// nobody watching had any way to notice. The error icon alone says
	// "something is wrong"; the reason is what makes it fixable.
	description string
}

var builtinSkillsCache struct {
	once   sync.Once
	skills []*skills.Skill
}

func cachedBuiltinSkills() []*skills.Skill {
	builtinSkillsCache.once.Do(func() {
		builtinSkillsCache.skills = skills.DiscoverBuiltin()
	})
	return builtinSkillsCache.skills
}

// skillsInfo renders the skill discovery status section showing loaded and
// invalid skills.
func (m *UI) skillsInfo(width, maxItems int, isSection bool) string {
	t := m.com.Styles

	title := t.Resource.Heading.Render("Skills")
	if isSection {
		title = common.Section(t, title, width)
	}

	items := m.skillStatusItems()
	if len(items) == 0 {
		list := t.Resource.AdditionalText.Render("None")
		return lipgloss.NewStyle().Width(width).Render(fmt.Sprintf("%s\n\n%s", title, list))
	}

	list := skillsList(t, items, width, maxItems)
	return lipgloss.NewStyle().Width(width).Render(fmt.Sprintf("%s\n\n%s", title, list))
}

func (m *UI) skillStatusItems() []skillStatusItem {
	t := m.com.Styles
	var items []skillStatusItem
	stateNames := make(map[string]struct{}, len(m.skillStates))

	disabledSet := make(map[string]bool)
	if m.com != nil && m.com.Workspace != nil {
		if cfg := m.com.Config(); cfg != nil {
			for _, name := range cfg.Options.DisabledSkills {
				disabledSet[name] = true
			}
		}
	}

	states := slices.Clone(m.skillStates)
	slices.SortStableFunc(states, func(a, b *skills.SkillState) int {
		return strings.Compare(a.Path, b.Path)
	})
	for _, state := range states {
		name := state.Name
		if name == "" {
			name = filepath.Base(filepath.Dir(state.Path))
		}
		if disabledSet[name] {
			continue
		}
		if _, exists := stateNames[name]; exists {
			continue
		}
		stateNames[name] = struct{}{}
		icon := t.Resource.EnabledIcon.String()
		var description string
		if state.State == skills.StateError {
			icon = t.Resource.ErrorIcon.String()
			description = skillErrorDescription(state)
		}
		items = append(items, skillStatusItem{
			icon:        icon,
			name:        name,
			title:       t.Resource.Name.Render(name),
			description: description,
		})
	}

	builtin := cachedBuiltinSkills()
	slices.SortStableFunc(builtin, func(a, b *skills.Skill) int {
		return strings.Compare(a.Name, b.Name)
	})
	for _, skill := range builtin {
		if _, ok := stateNames[skill.Name]; ok {
			continue
		}
		if disabledSet[skill.Name] {
			continue
		}
		items = append(items, skillStatusItem{
			icon:  t.Resource.EnabledIcon.String(),
			name:  skill.Name,
			title: t.Resource.Name.Render(skill.Name),
		})
	}

	slices.SortStableFunc(items, func(a, b skillStatusItem) int {
		return strings.Compare(a.name, b.name)
	})

	return items
}

// skillErrorDescription renders why a skill failed discovery, in the one
// line the status row has room for. It leads with the reason rather than
// the path: the row already names the skill, and the reason ("mapping
// values are not allowed" — an unquoted colon in the frontmatter) is what
// tells the reader what to go change.
func skillErrorDescription(state *skills.SkillState) string {
	if state.Err == nil {
		return "failed to load"
	}
	msg := strings.TrimSpace(state.Err.Error())
	if msg == "" {
		return "failed to load"
	}
	return msg
}

func skillsList(t *styles.Styles, items []skillStatusItem, width, maxItems int) string {
	if maxItems <= 0 {
		return ""
	}

	if len(items) > maxItems {
		visibleItems := items[:maxItems-1]
		remaining := len(items) - (maxItems - 1)
		items = append(visibleItems, skillStatusItem{
			name:  "more",
			title: t.Resource.AdditionalText.Render(fmt.Sprintf("…and %d more", remaining)),
		})
	}

	renderedItems := make([]string, 0, len(items))
	for _, item := range items {
		renderedItems = append(renderedItems, common.Status(t, common.StatusOpts{
			Icon:        item.icon,
			Title:       item.title,
			Description: item.description,
		}, width))
	}
	return lipgloss.JoinVertical(lipgloss.Left, renderedItems...)
}
