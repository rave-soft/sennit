package dialog

import (
	"cmp"
	"slices"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

const (
	// ProvidersID is the identifier for the providers configuration dialog.
	ProvidersID              = "providers"
	providersDialogMaxWidth  = 60
	providersDialogMinHeight = 8
	providersDialogMaxHeight = 20
)

// customProviderItemID is the sentinel ID for the "Custom provider…" entry.
// It can never collide with a real catwalk.InferenceProvider ID.
const customProviderItemID = "__custom__"

// Providers is a dialog for choosing a provider to configure — a catalog
// provider (opens the API-key or OAuth flow) or a custom provider (opens
// [ProviderForm]). It is a thin wrapper around [selectDialog].
type Providers struct {
	*selectDialog
}

var _ Dialog = (*Providers)(nil)

// NewProviders creates a new providers configuration dialog. isOnboarding
// switches the title to onboarding copy; no caller passes true in this
// portion (the dialog is reachable only from the command palette), but it
// follows the same forward-compatible shape as [NewModels] for when
// onboarding wires it up.
func NewProviders(com *common.Common, isOnboarding bool) (*Providers, error) {
	title := "Set up a provider"
	if isOnboarding {
		title = "To start, connect a model provider."
	}
	sd, err := newSelectDialog(com, selectDialogConfig{
		id:            ProvidersID,
		title:         title,
		maxWidth:      providersDialogMaxWidth,
		dynamicHeight: true,
		minHeight:     providersDialogMinHeight,
		maxHeight:     providersDialogMaxHeight,
		buildItems:    func() ([]list.FilterableItem, int, error) { return providerItems(com) },
		onSelect: func(id string) Action {
			if id == customProviderItemID {
				return ActionOpenCustomProviderForm{}
			}
			return ActionConfigureProvider{ProviderID: id}
		},
	})
	if err != nil {
		return nil, err
	}
	return &Providers{selectDialog: sd}, nil
}

// providerItems builds the provider list items: the catalog providers
// (sorted by name for stable output), each flagged "Configured" when
// already present in cfg.Providers, prefixed with a "Custom provider…"
// entry.
func providerItems(com *common.Common) ([]list.FilterableItem, int, error) {
	t := com.Styles
	cfg := com.Config()

	providers := config.Providers(cfg)

	sorted := slices.Clone(providers)
	slices.SortFunc(sorted, func(a, b catwalk.Provider) int {
		return cmp.Compare(a.Name, b.Name)
	})

	items := make([]list.FilterableItem, 0, len(sorted)+1)
	items = append(items, &ProviderItem{
		BaseItem: list.NewBaseItem(),
		id:       customProviderItemID,
		name:     "Custom provider…",
		t:        t,
	})

	for _, p := range sorted {
		_, configured := cfg.Providers.Get(string(p.ID))
		items = append(items, &ProviderItem{
			BaseItem:   list.NewBaseItem(),
			id:         string(p.ID),
			name:       p.Name,
			configured: configured,
			t:          t,
		})
	}

	return items, 0, nil
}

// ProviderItem represents a provider list item.
type ProviderItem struct {
	list.BaseItem
	id         string
	name       string
	configured bool
	t          *styles.Styles
}

var _ ListItem = (*ProviderItem)(nil)

// Filter implements ListItem.
func (p *ProviderItem) Filter() string {
	return p.name
}

// ID implements ListItem.
func (p *ProviderItem) ID() string {
	return p.id
}

// Render implements ListItem.
func (p *ProviderItem) Render(width int) string {
	info := ""
	if p.configured {
		info = "Configured"
	}
	st := defaultListItemStyles(p.t)
	return renderItem(st, p.name, info, p.Focused(), width, p.Cache(), p.Match())
}
