package dialog

import (
	"cmp"
	"fmt"
	"log/slog"
	"slices"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/list"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/sahilm/fuzzy"
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

	// A stale catalog must not keep this dialog from opening.
	providers, err := config.Providers(cfg)
	if err != nil && len(providers) == 0 {
		return nil, 0, fmt.Errorf("failed to get providers: %w", err)
	} else if err != nil {
		slog.Warn("Listing the previously known providers", "error", err)
	}

	sorted := slices.Clone(providers)
	slices.SortFunc(sorted, func(a, b catwalk.Provider) int {
		return cmp.Compare(a.Name, b.Name)
	})

	items := make([]list.FilterableItem, 0, len(sorted)+1)
	items = append(items, &ProviderItem{
		Versioned: list.NewVersioned(),
		id:        customProviderItemID,
		name:      "Custom provider…",
		t:         t,
	})

	for _, p := range sorted {
		_, configured := cfg.Providers.Get(string(p.ID))
		items = append(items, &ProviderItem{
			Versioned:  list.NewVersioned(),
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
	*list.Versioned
	id         string
	name       string
	configured bool
	t          *styles.Styles
	m          fuzzy.Match
	cache      map[int]string
	focused    bool
}

var _ ListItem = (*ProviderItem)(nil)

// Finished implements list.Item. Provider items are render-stable outside
// of explicit SetFocused / SetMatch.
func (p *ProviderItem) Finished() bool {
	return true
}

// Filter implements ListItem.
func (p *ProviderItem) Filter() string {
	return p.name
}

// ID implements ListItem.
func (p *ProviderItem) ID() string {
	return p.id
}

// SetFocused implements ListItem.
func (p *ProviderItem) SetFocused(focused bool) {
	if p.focused == focused {
		return
	}
	p.cache = nil
	p.focused = focused
	if p.Versioned != nil {
		p.Bump()
	}
}

// SetMatch implements ListItem.
func (p *ProviderItem) SetMatch(m fuzzy.Match) {
	if sameFuzzyMatch(p.m, m) {
		return
	}
	p.cache = nil
	p.m = m
	if p.Versioned != nil {
		p.Bump()
	}
}

// Render implements ListItem.
func (p *ProviderItem) Render(width int) string {
	info := ""
	if p.configured {
		info = "Configured"
	}
	st := ListItemStyles{
		ItemBlurred:     p.t.Dialog.NormalItem,
		ItemFocused:     p.t.Dialog.SelectedItem,
		InfoTextBlurred: p.t.Dialog.ListItem.InfoBlurred,
		InfoTextFocused: p.t.Dialog.ListItem.InfoFocused,
	}
	return renderItem(st, p.name, info, p.focused, width, p.cache, &p.m)
}
