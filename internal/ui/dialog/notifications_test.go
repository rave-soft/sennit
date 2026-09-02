package dialog

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/config"
	providerruntime "github.com/rave-soft/sennit/internal/providers/runtime"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/notification"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// notificationsTestWorkspace is a minimal [workspace.Workspace] stub, only
// implementing what notificationItems reads (Config()).
type notificationsTestWorkspace struct {
	workspace.Workspace
	cfg *config.Config
}

// KnownProviders mirrors what the UI used to compute for itself:
// the embedded catalog for this fake's config.
func (w notificationsTestWorkspace) KnownProviders() []catwalk.Provider {
	return providerruntime.Providers(w.cfg)
}

// SkillStates, BuiltinSkills: the skills panel reads these; no test
// here has a catalog beyond what the binary ships.
func (w notificationsTestWorkspace) SkillStates() []*skills.SkillState { return nil }
func (w notificationsTestWorkspace) ConfigProblems() []config.Problem  { return nil }
func (w notificationsTestWorkspace) BuiltinSkills() []*skills.Skill    { return skills.DiscoverBuiltin() }

func (w *notificationsTestWorkspace) Config() *config.Config { return w.cfg }

// newNotificationsTestCommon builds a [common.Common] whose configured
// notification style is currentStyle. An empty currentStyle produces a
// workspace whose Config() returns nil, exercising the nil-config fallback
// in notificationItems.
func newNotificationsTestCommon(t *testing.T, currentStyle string) *common.Common {
	t.Helper()
	s := styles.SennitDark()
	var cfg *config.Config
	if currentStyle != "" {
		cfg = &config.Config{Options: &config.Options{Notifications: currentStyle}}
	}
	return &common.Common{Styles: &s, Workspace: &notificationsTestWorkspace{cfg: cfg}}
}

// TestNewNotifications verifies the constructor builds a working dialog
// wired up with the NotificationsID and the default ("auto") selection when
// no style is configured.
func TestNewNotifications(t *testing.T) {
	t.Parallel()

	com := newNotificationsTestCommon(t, "")
	n := NewNotifications(com)

	require.NotNil(t, n)
	require.Equal(t, NotificationsID, n.ID())
	require.Equal(t, "auto", n.selectedID())
}

// TestNotificationItems_CurrentStyleSelected verifies the returned index
// and isCurrent flag track the configured style, not just the first item.
func TestNotificationItems_CurrentStyleSelected(t *testing.T) {
	t.Parallel()

	com := newNotificationsTestCommon(t, "osc")
	items, selectedIndex, err := notificationItems(com)

	require.NoError(t, err)
	oscIndex := -1
	for i, item := range items {
		ni := item.(*NotificationItem)
		isOSC := ni.ID() == "osc"
		require.Equal(t, isOSC, ni.isCurrent)
		if isOSC {
			oscIndex = i
		}
	}
	require.GreaterOrEqual(t, oscIndex, 0, "osc must be present in the item list")
	require.Equal(t, oscIndex, selectedIndex)
}

// TestNotificationItems_NilConfigDefaultsToAuto verifies a workspace whose
// Config() returns nil (or whose Options is nil) falls back to "auto"
// rather than panicking or leaving nothing selected.
func TestNotificationItems_NilConfigDefaultsToAuto(t *testing.T) {
	t.Parallel()

	com := newNotificationsTestCommon(t, "")
	items, selectedIndex, err := notificationItems(com)

	require.NoError(t, err)
	require.Equal(t, "auto", items[selectedIndex].(*NotificationItem).style.ID)
}

// TestNotificationItems_NativeHiddenWhenUnsupported verifies the "native"
// option is present exactly when the platform supports it.
func TestNotificationItems_NativeHiddenWhenUnsupported(t *testing.T) {
	t.Parallel()

	com := newNotificationsTestCommon(t, "auto")
	items, _, err := notificationItems(com)
	require.NoError(t, err)

	hasNative := false
	for _, item := range items {
		if item.(*NotificationItem).ID() == "native" {
			hasNative = true
		}
	}
	require.Equal(t, notification.NativeSupported, hasNative)
}

// TestNotificationItem_FilterAndID verify the item exposes its style's
// title for fuzzy filtering and its style's ID as the stable identifier.
func TestNotificationItem_FilterAndID(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()
	item := &NotificationItem{
		BaseItem: list.NewBaseItem(),
		style:    NotificationStyle{ID: "bell", Title: "Bell", Description: "d"},
		t:        &s,
	}

	require.Equal(t, "Bell", item.Filter())
	require.Equal(t, "bell", item.ID())
}

// TestNotificationItem_Render verifies the rendered row contains the
// style's title, and the "current" marker appears only when isCurrent.
func TestNotificationItem_Render(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()
	current := &NotificationItem{
		BaseItem:  list.NewBaseItem(),
		style:     NotificationStyle{ID: "bell", Title: "Bell", Description: "d"},
		isCurrent: true,
		t:         &s,
	}
	other := &NotificationItem{
		BaseItem: list.NewBaseItem(),
		style:    NotificationStyle{ID: "osc", Title: "OSC", Description: "d"},
		t:        &s,
	}

	currentRendered := ansi.Strip(current.Render(40))
	otherRendered := ansi.Strip(other.Render(40))

	require.Contains(t, currentRendered, "Bell")
	require.Contains(t, currentRendered, "current")
	require.Contains(t, otherRendered, "OSC")
	require.NotContains(t, otherRendered, "current")
}
