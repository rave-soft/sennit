package dialog

import (
	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/styles"
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
	list.BaseItem

	prov  catwalk.Provider
	model catwalk.Model

	t            *styles.Styles
	showProvider bool
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

var _ ListItem = &ModelItem{}

// NewModelItem creates a new ModelItem.
func NewModelItem(t *styles.Styles, prov catwalk.Provider, model catwalk.Model, showProvider bool) *ModelItem {
	return &ModelItem{
		BaseItem:     list.NewBaseItem(),
		prov:         prov,
		model:        model,
		t:            t,
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
	return renderItem(styles, m.model.Name, providerInfo, m.Focused(), width, m.Cache(), m.Match())
}
