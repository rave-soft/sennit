package dialog

import (
	"cmp"
	"fmt"
	"slices"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/util"
)

// modelInputPlaceholder is shown in the filter input.
const modelInputPlaceholder = "Choose a model"

// ModelsID is the identifier for the model selection dialog.
const ModelsID = "models"

const defaultModelsDialogMaxWidth = 73

// Models represents a model selection dialog.
type Models struct {
	Base
	com *common.Common

	providers []catwalk.Provider

	keyMap struct {
		UpDown   key.Binding
		Select   key.Binding
		Edit     key.Binding
		Next     key.Binding
		Previous key.Binding
		Close    key.Binding
	}
	list  *ModelsList
	input textinput.Model
	help  help.Model
}

var _ Dialog = (*Models)(nil)

// NewModels creates a new Models dialog. The returned [tea.Cmd] is non-nil
// when opening the dialog also needs to prune stale entries from the
// "recently used" list — see setProviderItems.
func NewModels(com *common.Common) (*Models, tea.Cmd, error) {
	t := com.Styles
	m := &Models{Base: NewBase(com, defaultModelsDialogMaxWidth)}
	m.com = com

	help := help.New()
	help.Styles = t.DialogHelpStyles()

	m.help = help
	m.list = NewModelsList(t)
	m.list.Focus()
	m.list.SetSelected(0)

	m.input = textinput.New()
	m.input.SetVirtualCursor(false)
	m.input.Placeholder = modelInputPlaceholder
	m.input.SetStyles(com.Styles.TextInput)
	m.input.Focus()

	m.keyMap.Select = key.NewBinding(
		key.WithKeys("enter", "ctrl+y"),
		key.WithHelp("enter", "confirm"),
	)
	m.keyMap.Edit = key.NewBinding(
		key.WithKeys("ctrl+e"),
		key.WithHelp("ctrl+e", "edit"),
	)
	m.keyMap.UpDown = key.NewBinding(
		key.WithKeys("up", "down"),
		key.WithHelp("↑/↓", "choose"),
	)
	m.keyMap.Next = key.NewBinding(
		key.WithKeys("down", "ctrl+n"),
		key.WithHelp("↓", "next item"),
	)
	m.keyMap.Previous = key.NewBinding(
		key.WithKeys("up", "ctrl+p"),
		key.WithHelp("↑", "previous item"),
	)
	m.keyMap.Close = CloseKey

	m.providers = config.Providers(m.com.Config())

	return m, m.setProviderItems(), nil
}

// ID implements Dialog.
func (m *Models) ID() string {
	return ModelsID
}

// HandleMsg implements Dialog.
func (m *Models) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, m.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, m.keyMap.Previous):
			m.list.Focus()
			if m.list.IsSelectedFirst() {
				m.list.SelectLast()
			} else {
				m.list.SelectPrev()
			}
			m.list.ScrollToSelected()
		case key.Matches(msg, m.keyMap.Next):
			m.list.Focus()
			if m.list.IsSelectedLast() {
				m.list.SelectFirst()
			} else {
				m.list.SelectNext()
			}
			m.list.ScrollToSelected()
		case key.Matches(msg, m.keyMap.Select, m.keyMap.Edit):
			selectedItem := m.list.SelectedItem()
			if selectedItem == nil {
				break
			}

			modelItem, ok := selectedItem.(*ModelItem)
			if !ok {
				break
			}

			isEdit := key.Matches(msg, m.keyMap.Edit)

			return ActionSelectModel{
				Provider:       modelItem.prov,
				Model:          m.rememberEffort(modelItem),
				ReAuthenticate: isEdit,
			}
		default:
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			value := m.input.Value()
			m.list.Focus()
			m.list.SetFilter(value)
			m.list.SelectFirst()
			m.list.ScrollToTop()
			return ActionCmd{cmd}
		}
	}
	return nil
}

// rememberEffort is the picked model with the reasoning effort it was last
// used at, falling back to the catalog default the item carries.
//
// Picking a model is not a request to reset how hard it thinks: a user who
// moves to another model and back would otherwise find their effort silently
// back at the default, with only the model name in the status bar to say
// otherwise. A remembered level the model no longer offers (its catalog
// entry changed since) is dropped rather than sent.
func (m *Models) rememberEffort(item *ModelItem) config.SelectedModel {
	selected := item.SelectedModel()
	cfg := m.com.Config()
	if cfg == nil {
		return selected
	}
	effort := cfg.RememberedReasoningEffort(selected.Provider, selected.Model)
	if effort != "" && slices.Contains(item.model.ReasoningLevels, effort) {
		selected.ReasoningEffort = effort
	}
	return selected
}

// Cursor returns the cursor for the dialog.
func (m *Models) Cursor() *tea.Cursor {
	return InputCursor(m.com.Styles, m.input.Cursor())
}

// Draw implements [Dialog].
func (m *Models) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := m.com.Styles
	m.Resize(area)
	width := m.Width()
	height := max(0, min(defaultDialogHeight, area.Dy()-t.Dialog.View.GetVerticalBorderSize()))
	innerWidth := m.InnerWidth()
	m.input.SetWidth(dialogInputTextWidth(t, m.input, innerWidth))

	listHeight, listTotalHeight, _ := sizeDialogList(t, m.list, innerWidth, height)

	rc := NewRenderContext(t, width)
	rc.Title = "Switch Model"

	inputView := t.Dialog.InputPrompt.Render(m.input.View())
	rc.AddPart(inputView)

	listView := t.Dialog.List.Height(m.list.Height()).Render(m.list.Render())
	listView = joinScrollbar(t, listView, listHeight, listTotalHeight, listHeight, m.list.Offset())
	rc.AddPart(listView)

	rc.Help = renderDialogHelp(t, &m.help, m, innerWidth)

	cur := m.Cursor()
	view := rc.Render()
	DrawCenterCursor(scr, area, view, cur)
	return cur
}

// ShortHelp returns the short help view.
func (m *Models) ShortHelp() []key.Binding {
	h := []key.Binding{
		m.keyMap.UpDown,
		m.keyMap.Select,
	}
	if m.isSelectedConfigured() {
		h = append(h, m.keyMap.Edit)
	}
	h = append(h, m.keyMap.Close)
	return h
}

// FullHelp returns the full help view.
func (m *Models) FullHelp() [][]key.Binding {
	return [][]key.Binding{m.ShortHelp()}
}

func (m *Models) isSelectedConfigured() bool {
	selectedItem := m.list.SelectedItem()
	if selectedItem == nil {
		return false
	}
	modelItem, ok := selectedItem.(*ModelItem)
	if !ok {
		return false
	}
	providerID := string(modelItem.prov.ID)
	_, isConfigured := m.com.Config().Providers.Get(providerID)
	return isConfigured
}

// setProviderItems sets the provider items in the list. Providers that were
// removed or disabled since a "recently used" entry was recorded leave that
// entry stale; setProviderItems filters those out of the rendered list and
// returns a [tea.Cmd] that persists the pruned list, so the write happens
// off the Update goroutine instead of inline here (this used to save
// synchronously during dialog construction — see the FIXME this replaced).
func (m *Models) setProviderItems() tea.Cmd {
	t := m.com.Styles
	cfg := m.com.Config()

	var selectedItemID string
	currentModel := cfg.Model
	recentItems := cfg.RecentModels

	// Track providers already added to avoid duplicates
	addedProviders := make(map[string]bool)

	// Get a list of known providers to compare against
	knownProviders := config.Providers(cfg)

	containsProviderFunc := func(id string) func(p catwalk.Provider) bool {
		return func(p catwalk.Provider) bool {
			return p.ID == catwalk.InferenceProvider(id)
		}
	}

	// itemsMap contains the keys of added model items.
	itemsMap := make(map[string]*ModelItem)
	groups := []ModelGroup{}
	for id, p := range cfg.Providers.Seq2() {
		if p.Disable {
			continue
		}

		// Check if this provider is not in the known providers list
		if !slices.ContainsFunc(knownProviders, containsProviderFunc(id)) ||
			!slices.ContainsFunc(m.providers, containsProviderFunc(id)) {
			provider := p.ToProvider()

			// Add this unknown provider to the list
			name := cmp.Or(p.Name, id)

			addedProviders[id] = true

			group := NewModelGroup(t, name)
			for _, model := range p.Models {
				item := NewModelItem(t, provider, model, false)
				group.AppendItems(item)
				itemsMap[item.ID()] = item
				if model.ID == currentModel.Model && string(provider.ID) == currentModel.Provider {
					selectedItemID = item.ID()
				}
			}
			if len(group.Items) > 0 {
				groups = append(groups, group)
			}
		}
	}

	// Now add known providers from the predefined list.
	for _, provider := range m.providers {
		providerID := string(provider.ID)
		if addedProviders[providerID] {
			continue
		}

		// The "Configure Providers" dialog now owns provider setup; this
		// dialog only ever lists models for providers the user already
		// configured, so an unconfigured or disabled catalog provider is
		// skipped entirely rather than shown as a dead end into auth.
		providerConfig, providerConfigured := cfg.Providers.Get(providerID)
		if !providerConfigured || providerConfig.Disable {
			continue
		}

		displayProvider := provider
		// provider.Models is a slice header copied by value; its backing
		// array is still shared with m.providers. Clone it before writing
		// through displayProvider.Models below, or these per-dialog name
		// overrides and appended custom models corrupt the shared catalog.
		displayProvider.Models = slices.Clone(provider.Models)
		displayProvider.Name = cmp.Or(providerConfig.Name, displayProvider.Name)
		modelIndex := make(map[string]int, len(displayProvider.Models))
		for i, model := range displayProvider.Models {
			modelIndex[model.ID] = i
		}
		for _, model := range providerConfig.Models {
			if model.ID == "" {
				continue
			}
			if idx, ok := modelIndex[model.ID]; ok {
				if model.Name != "" {
					displayProvider.Models[idx].Name = model.Name
				}
				continue
			}
			model.Name = cmp.Or(model.Name, model.ID)
			displayProvider.Models = append(displayProvider.Models, model)
			modelIndex[model.ID] = len(displayProvider.Models) - 1
		}

		name := cmp.Or(displayProvider.Name, providerID)

		group := NewModelGroup(t, name)
		for _, model := range displayProvider.Models {
			item := NewModelItem(t, provider, model, false)
			group.AppendItems(item)
			itemsMap[item.ID()] = item
			if model.ID == currentModel.Model && string(provider.ID) == currentModel.Provider {
				selectedItemID = item.ID()
			}
		}

		groups = append(groups, group)
	}

	if len(groups) == 0 {
		// No configured providers at all (e.g. a bare config): show a
		// header-only, non-selectable group pointing the user at the
		// dialog that now owns provider setup. It has no *ModelItem
		// children, so the list's SelectFirst/SelectNext helpers (which
		// only ever land on *ModelItem) simply skip over it.
		groups = append(groups, NewModelGroup(t, `No providers configured — open "Configure Providers" to add one`))
	}

	// pruneCmd persists validRecentItems if the list below finds any recent
	// entries whose provider/model no longer exists. Providers that were
	// removed or disabled after a model was last picked simply never make
	// it into itemsMap above, so the lookup below already drops their
	// recent entries from the rendered list — no extra filtering needed
	// there.
	var pruneCmd tea.Cmd
	if len(recentItems) > 0 {
		recentGroup := NewModelGroup(t, "Recently used")

		var validRecentItems []config.SelectedModel
		for _, recent := range recentItems {
			key := modelKey(recent.Provider, recent.Model)
			item, ok := itemsMap[key]
			if !ok {
				continue
			}

			// Show provider for recent items
			item = NewModelItem(t, item.prov, item.model, true)
			item.showProvider = true

			validRecentItems = append(validRecentItems, recent)
			recentGroup.AppendItems(item)
			if recent.Model == currentModel.Model && recent.Provider == currentModel.Provider {
				selectedItemID = item.ID()
			}
		}

		if len(validRecentItems) != len(recentItems) {
			ws := m.com.Workspace
			pruneCmd = func() tea.Msg {
				if err := ws.SetConfigField(config.ScopeGlobal, "recent_models", validRecentItems); err != nil {
					return util.NewErrorMsg(fmt.Errorf("failed to update recent models: %w", err))
				}
				return nil
			}
		}

		if len(recentGroup.Items) > 0 {
			groups = append([]ModelGroup{recentGroup}, groups...)
		}
	}

	// Set model groups in the list.
	m.list.SetGroups(groups...)
	m.list.SetSelectedItem(selectedItemID)
	if selectedItemID != "" {
		m.list.ScrollToSelected()
	} else {
		m.list.ScrollToTop()
	}

	return pruneCmd
}

func modelKey(providerID, modelID string) string {
	if providerID == "" || modelID == "" {
		return ""
	}
	return providerID + ":" + modelID
}
