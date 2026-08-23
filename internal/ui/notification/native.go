package notification

import (
	"log/slog"
	"sync"

	tea "charm.land/bubbletea/v2"
)

// NativeBackend sends desktop notifications using the native OS notification
// system. The actual delivery function is supplied per-platform via
// defaultNotifyFunc; on illumos/solaris (where beeep's dbus dependency does
// not build) it is a no-op. Selection logic avoids this backend there and
// uses a terminal-based backend instead, so this is only a safety net. See
// NativeSupported.
type NativeBackend struct {
	// icon is the notification icon data (PNG bytes).
	icon []byte
	// notifyFunc is the function used to send notifications (swappable for testing).
	notifyFunc func(title, message string, icon any) error

	// iconPathOnce and iconPath cache the on-disk location of icon (see
	// CacheIcon) so repeated sends reuse one file instead of beeep writing a
	// fresh temp file per notification.
	iconPathOnce sync.Once
	iconPath     string
}

// NewNativeBackend creates a new native notification backend.
func NewNativeBackend(icon []byte) *NativeBackend {
	return &NativeBackend{
		icon:       icon,
		notifyFunc: defaultNotifyFunc,
	}
}

// Send returns a command that sends a desktop notification using the native
// OS notification system.
func (b *NativeBackend) Send(n Notification) tea.Cmd {
	return func() tea.Msg {
		slog.Debug("Sending native notification", "title", n.Title, "message", n.Message)

		if err := b.notifyFunc(n.Title, n.Message, b.resolvedIcon()); err != nil {
			slog.Error("Failed to send notification", "error", err)
		} else {
			slog.Debug("Notification sent successfully")
		}

		return nil
	}
}

// resolvedIcon returns the icon argument to pass to notifyFunc: a cached
// on-disk path when icon data is set (falling back to the raw bytes if
// caching fails), or nil when no icon was configured — e.g. in tests that
// construct a bare backend.
func (b *NativeBackend) resolvedIcon() any {
	if len(b.icon) == 0 {
		return nil
	}

	b.iconPathOnce.Do(func() {
		path, err := CacheIcon(b.icon)
		if err != nil {
			slog.Warn("Failed to cache notification icon on disk; sending inline bytes instead", "error", err)
			return
		}
		b.iconPath = path
	})

	if b.iconPath != "" {
		return b.iconPath
	}
	return b.icon
}

// SetNotifyFunc allows replacing the notification function for testing.
func (b *NativeBackend) SetNotifyFunc(fn func(title, message string, icon any) error) {
	b.notifyFunc = fn
}
