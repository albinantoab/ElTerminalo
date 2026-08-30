//go:build !darwin

package macos

// The macOS half of this file is notify_darwin.go. See attention_other.go for
// why the non-darwin stubs exist at all.

// InitNotifications sets up native notifications. There are none off macOS.
func InitNotifications() {}

// NotificationsReady reports whether a notification would reach the user.
// Never, off macOS.
func NotificationsReady() bool { return false }

// Notify posts a native notification and reports whether it was queued.
// Nothing to post to off macOS.
func Notify(_, _, _ string) bool { return false }

// SetDockBadge sets the Dock tile's badge. There is no Dock off macOS.
func SetDockBadge(_ string) {}
