package dialog

import (
	"errors"

	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/list"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/sahilm/fuzzy"
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
	*list.Versioned
	effort    string
	title     string
	isCurrent bool
	t         *styles.Styles
	m         fuzzy.Match
	cache     map[int]string
	focused   bool
}

// Finished implements list.Item. Reasoning items are render-stable
// outside of explicit SetFocused / SetMatch.
func (r *ReasoningItem) Finished() bool {
	return true
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
	agentCfg, ok := cfg.Agents[config.AgentCoder]
	if !ok {
		return nil, 0, errors.New("agent configuration not found")
	}

	selectedModel := cfg.Models[agentCfg.Model]
	model := cfg.GetModelByType(agentCfg.Model)
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
			Versioned: list.NewVersioned(),
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

// SetFocused sets the focus state of the reasoning item.
func (r *ReasoningItem) SetFocused(focused bool) {
	if r.focused == focused {
		return
	}
	r.cache = nil
	r.focused = focused
	if r.Versioned != nil {
		r.Bump()
	}
}

// SetMatch sets the fuzzy match for the reasoning item.
func (r *ReasoningItem) SetMatch(m fuzzy.Match) {
	if sameFuzzyMatch(r.m, m) {
		return
	}
	r.cache = nil
	r.m = m
	if r.Versioned != nil {
		r.Bump()
	}
}

// Render returns the string representation of the reasoning item.
func (r *ReasoningItem) Render(width int) string {
	info := ""
	if r.isCurrent {
		info = "current"
	}
	styles := ListItemStyles{
		ItemBlurred:     r.t.Dialog.NormalItem,
		ItemFocused:     r.t.Dialog.SelectedItem,
		InfoTextBlurred: r.t.Dialog.ListItem.InfoBlurred,
		InfoTextFocused: r.t.Dialog.ListItem.InfoFocused,
	}
	return renderItem(styles, r.title, info, r.focused, width, r.cache, &r.m)
}
