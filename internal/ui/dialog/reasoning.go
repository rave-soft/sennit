package dialog

import (
	"errors"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

const (
	// ReasoningID is the identifier for the reasoning effort dialog.
	ReasoningID              = "reasoning"
	reasoningDialogMaxWidth  = 50
	reasoningDialogMinHeight = 8
	reasoningDialogMaxHeight = 16
)

// Reasoning is a dialog for selecting reasoning effort. It is a thin wrapper
// around [selectDialog]: the dynamic height strategy (sized to content,
// clamped between reasoningDialogMinHeight and reasoningDialogMaxHeight, with
// a scrollbar) is what distinguishes it from [Notifications].
type Reasoning struct {
	*selectDialog
}

// ReasoningItem represents a reasoning effort list item.
type ReasoningItem struct {
	list.BaseItem
	effort    string
	title     string
	isCurrent bool
	t         *styles.Styles
}

var (
	_ Dialog   = (*Reasoning)(nil)
	_ ListItem = (*ReasoningItem)(nil)
)

// NewReasoning creates a new reasoning effort dialog.
func NewReasoning(com *common.Common) (*Reasoning, error) {
	sd, err := newSelectDialog(com, selectDialogConfig{
		id:            ReasoningID,
		title:         "Select Reasoning Effort",
		maxWidth:      reasoningDialogMaxWidth,
		dynamicHeight: true,
		minHeight:     reasoningDialogMinHeight,
		maxHeight:     reasoningDialogMaxHeight,
		buildItems:    func() ([]list.FilterableItem, int, error) { return reasoningItems(com) },
		onSelect: func(id string) Action {
			return ActionSelectReasoningEffort{Effort: id}
		},
	})
	if err != nil {
		return nil, err
	}
	return &Reasoning{selectDialog: sd}, nil
}

// reasoningItems builds the reasoning effort list items, returning the index
// of the currently active effort.
func reasoningItems(com *common.Common) ([]list.FilterableItem, int, error) {
	cfg := com.Config()
	if _, ok := cfg.Agents[config.AgentCoder]; !ok {
		return nil, 0, errors.New("agent configuration not found")
	}

	// The coder agent leaves Model unset (it inherits the app's configured
	// model), so the model it actually runs on is always cfg.Model.
	selectedModel := cfg.Model
	model := cfg.SelectedCatalogModel()
	if model == nil {
		return nil, 0, errors.New("model configuration not found")
	}

	if len(model.ReasoningLevels) == 0 {
		return nil, 0, errors.New("no reasoning levels available")
	}

	currentEffort := selectedModel.ReasoningEffort
	if currentEffort == "" {
		currentEffort = model.DefaultReasoningEffort
	}

	items := make([]list.FilterableItem, 0, len(model.ReasoningLevels))
	selectedIndex := 0
	for i, effort := range model.ReasoningLevels {
		item := &ReasoningItem{
			BaseItem:  list.NewBaseItem(),
			effort:    effort,
			title:     common.FormatReasoningEffort(effort),
			isCurrent: effort == currentEffort,
			t:         com.Styles,
		}
		items = append(items, item)
		if effort == currentEffort {
			selectedIndex = i
		}
	}

	return items, selectedIndex, nil
}

// Filter returns the filter value for the reasoning item.
func (r *ReasoningItem) Filter() string {
	return r.title
}

// ID returns the unique identifier for the reasoning effort.
func (r *ReasoningItem) ID() string {
	return r.effort
}

// Render returns the string representation of the reasoning item.
func (r *ReasoningItem) Render(width int) string {
	info := ""
	if r.isCurrent {
		info = "current"
	}
	styles := defaultListItemStyles(r.t)
	return renderItem(styles, r.title, info, r.Focused(), width, r.Cache(), r.Match())
}
