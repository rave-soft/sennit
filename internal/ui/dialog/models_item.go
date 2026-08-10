package dialog

import (
	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/list"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/sahilm/fuzzy"
)

// ModelGroup represents a group of model items.
type ModelGroup struct {
	*list.Versioned
	Title string
	Items []*ModelItem
	t     *styles.Styles
}

// NewModelGroup creates a new ModelGroup.
func NewModelGroup(t *styles.Styles, title string, items ...*ModelItem) ModelGroup {
	return ModelGroup{
		Versioned: list.NewVersioned(),
		Title:     title,
		Items:     items,
		t:         t,
	}
}

// Finished implements list.Item. Model groups are immutable headers.
func (m *ModelGroup) Finished() bool {
	return true
}

// AppendItems appends [ModelItem]s to the group.
func (m *ModelGroup) AppendItems(items ...*ModelItem) {
	m.Items = append(m.Items, items...)
}

// Render implements [list.Item].
func (m *ModelGroup) Render(width int) string {
	// This dialog now only ever lists configured providers, so there is no
	// "Configured" badge to render here anymore (see providers.go's
	// ProviderItem for that concern in the provider-setup dialog).
	title := " " + m.Title + " "
	title = ansi.Truncate(title, max(0, width-1), "…")

	return common.Section(m.t, title, width, "")
}

// ModelItem represents a list item for a model type.
type ModelItem struct {
	*list.Versioned

	prov  catwalk.Provider
	model catwalk.Model

	cache        map[int]string
	t            *styles.Styles
	m            fuzzy.Match
	focused      bool
	showProvider bool
}

// Finished implements list.Item. Model items are render-stable
// outside of explicit SetFocused / SetMatch.
func (m *ModelItem) Finished() bool {
	return true
}

// SelectedModel returns this model item as a [config.SelectedModel] instance.
func (m *ModelItem) SelectedModel() config.SelectedModel {
	return config.SelectedModel{
		Model:           m.model.ID,
		Provider:        string(m.prov.ID),
		ReasoningEffort: m.model.DefaultReasoningEffort,
		MaxTokens:       m.model.DefaultMaxTokens,
	}
}

// SelectedModelType returns the type of model represented by this item.
// The models dialog only ever picks the large model slot now — the small
// slot is config-driven only (see ui.go's handleSelectModel auto-fill) —
// so this is always [config.SelectedModelTypeLarge].
func (m *ModelItem) SelectedModelType() config.SelectedModelType {
	return config.SelectedModelTypeLarge
}

var _ ListItem = &ModelItem{}

// NewModelItem creates a new ModelItem.
func NewModelItem(t *styles.Styles, prov catwalk.Provider, model catwalk.Model, showProvider bool) *ModelItem {
	return &ModelItem{
		Versioned:    list.NewVersioned(),
		prov:         prov,
		model:        model,
		t:            t,
		cache:        make(map[int]string),
		showProvider: showProvider,
	}
}

// Filter implements ListItem.
func (m *ModelItem) Filter() string {
	return m.model.Name
}

// ID implements ListItem.
func (m *ModelItem) ID() string {
	return modelKey(string(m.prov.ID), m.model.ID)
}

// Render implements ListItem.
func (m *ModelItem) Render(width int) string {
	var providerInfo string
	if m.showProvider {
		providerInfo = string(m.prov.Name)
	}
	styles := ListItemStyles{
		ItemBlurred:     m.t.Dialog.NormalItem,
		ItemFocused:     m.t.Dialog.SelectedItem,
		InfoTextBlurred: m.t.Dialog.ListItem.InfoBlurred,
		InfoTextFocused: m.t.Dialog.ListItem.InfoFocused,
	}
	return renderItem(styles, m.model.Name, providerInfo, m.focused, width, m.cache, &m.m)
}

// SetFocused implements ListItem.
func (m *ModelItem) SetFocused(focused bool) {
	if m.focused == focused {
		return
	}
	m.cache = nil
	m.focused = focused
	if m.Versioned != nil {
		m.Bump()
	}
}

// SetMatch implements ListItem.
func (m *ModelItem) SetMatch(fm fuzzy.Match) {
	if sameFuzzyMatch(m.m, fm) {
		return
	}
	m.cache = nil
	m.m = fm
	if m.Versioned != nil {
		m.Bump()
	}
}
