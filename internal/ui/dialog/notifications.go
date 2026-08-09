package dialog

import (
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/list"
	"github.com/rave-soft/braid/internal/ui/notification"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/sahilm/fuzzy"
)

const (
	// NotificationsID is the identifier for the notification style picker dialog.
	NotificationsID              = "notifications"
	notificationsDialogMaxWidth  = 50
	notificationsDialogMaxHeight = 12
)

// NotificationStyle represents a notification backend option.
type NotificationStyle struct {
	ID          string
	Title       string
	Description string
}

// AllNotificationStyles lists all available notification styles in order.
var AllNotificationStyles = []NotificationStyle{
	{ID: "auto", Title: "Auto", Description: "Automatically detect the best backend"},
	{ID: "native", Title: "Native", Description: "Use system notifications (macOS/Linux/Windows)"},
	{ID: "osc", Title: "OSC", Description: "Use terminal OSC escape sequences"},
	{ID: "bell", Title: "Bell", Description: "Use terminal bell character"},
	{ID: "disabled", Title: "Disabled", Description: "Turn off notifications"},
}

// Notifications is a dialog for selecting notification style. It is a thin
// wrapper around [selectDialog]: the fixed, non-scrolling height strategy
// (notificationsDialogMaxHeight, no min-height) is what distinguishes it
// from [Reasoning].
type Notifications struct {
	*selectDialog
}

// NotificationItem represents a notification style list item.
type NotificationItem struct {
	*list.Versioned
	style     NotificationStyle
	isCurrent bool
	t         *styles.Styles
	m         fuzzy.Match
	cache     map[int]string
	focused   bool
}

// Finished implements list.Item. Notification items are render-stable
// outside of explicit SetFocused / SetMatch.
func (n *NotificationItem) Finished() bool {
	return true
}

var (
	_ Dialog   = (*Notifications)(nil)
	_ ListItem = (*NotificationItem)(nil)
)

// NewNotifications creates a new notification style picker dialog.
func NewNotifications(com *common.Common) *Notifications {
	// buildItems never fails for notifications, so the error from
	// newSelectDialog is always nil here.
	sd, _ := newSelectDialog(com, selectDialogConfig{
		id:            NotificationsID,
		title:         "Notification Style",
		maxWidth:      notificationsDialogMaxWidth,
		dynamicHeight: false,
		maxHeight:     notificationsDialogMaxHeight,
		buildItems:    func() ([]list.FilterableItem, int, error) { return notificationItems(com) },
		onSelect: func(id string) Action {
			return ActionSelectNotificationStyle{Style: id}
		},
	})
	return &Notifications{selectDialog: sd}
}

// notificationItems builds the notification style list items, returning the
// index of the currently active style.
func notificationItems(com *common.Common) ([]list.FilterableItem, int, error) {
	cfg := com.Config()
	currentStyle := "auto"
	if cfg != nil && cfg.Options != nil && cfg.Options.Notifications != "" {
		currentStyle = cfg.Options.Notifications
	}

	items := make([]list.FilterableItem, 0, len(AllNotificationStyles))
	selectedIndex := 0
	for _, style := range AllNotificationStyles {
		// Native OS notifications don't build on every platform
		// (illumos/solaris); hide the option where it can't work.
		if style.ID == "native" && !notification.NativeSupported {
			continue
		}
		item := &NotificationItem{
			Versioned: list.NewVersioned(),
			style:     style,
			isCurrent: style.ID == currentStyle,
			t:         com.Styles,
		}
		if style.ID == currentStyle {
			selectedIndex = len(items)
		}
		items = append(items, item)
	}

	return items, selectedIndex, nil
}

// Filter returns the filter value for the notification item.
func (n *NotificationItem) Filter() string {
	return n.style.Title
}

// ID returns the unique identifier for the notification style.
func (n *NotificationItem) ID() string {
	return n.style.ID
}

// SetFocused sets the focus state of the notification item.
func (n *NotificationItem) SetFocused(focused bool) {
	if n.focused == focused {
		return
	}
	n.cache = nil
	n.focused = focused
	if n.Versioned != nil {
		n.Bump()
	}
}

// SetMatch sets the fuzzy match for the notification item.
func (n *NotificationItem) SetMatch(m fuzzy.Match) {
	if sameFuzzyMatch(n.m, m) {
		return
	}
	n.cache = nil
	n.m = m
	if n.Versioned != nil {
		n.Bump()
	}
}

// Render returns the string representation of the notification item.
func (n *NotificationItem) Render(width int) string {
	info := ""
	if n.isCurrent {
		info = "current"
	}
	st := ListItemStyles{
		ItemBlurred:     n.t.Dialog.NormalItem,
		ItemFocused:     n.t.Dialog.SelectedItem,
		InfoTextBlurred: n.t.Dialog.ListItem.InfoBlurred,
		InfoTextFocused: n.t.Dialog.ListItem.InfoFocused,
	}
	return renderItem(st, n.style.Title, info, n.focused, width, n.cache, &n.m)
}
