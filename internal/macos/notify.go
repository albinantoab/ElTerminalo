package macos

import (
	"log"
	"sync"
)

// The Go-only half of the notification plumbing: the handler a clicked
// notification is routed to.
//
// It is here rather than in notify_darwin.go so that it can be tested. An
// internal _test.go file may not import "C" — `go test` rejects the package
// outright — so the exported callback in notify_darwin.go is a two-line shim
// that copies the C string and calls notificationActivated, and this is the
// part a test can reach.

// activation holds the callback app.go registers for a clicked notification.
// Guarded rather than atomic because it is written once at startup and read
// from an AppKit delegate callback; the lock costs nothing at that rate and
// keeps the zero value ("no handler yet") honest.
var activation struct {
	mu sync.RWMutex
	fn func(paneKey string)
}

// OnNotificationActivated registers the function to call when the user clicks
// one of this app's notifications, with the paneKey that notification carried.
// Passing nil unregisters.
//
// The handler runs on its own goroutine, so it may do anything a Wails binding
// may — including calling back into AppKit through the Wails runtime, which the
// delegate thread it would otherwise have run on could not safely wait for.
func OnNotificationActivated(fn func(paneKey string)) {
	activation.mu.Lock()
	activation.fn = fn
	activation.mu.Unlock()
}

// notificationActivated hands one clicked notification's pane key to whatever
// is registered. paneKey has already been copied out of C memory by the caller.
func notificationActivated(paneKey string) {
	activation.mu.RLock()
	fn := activation.fn
	activation.mu.RUnlock()
	if fn == nil {
		log.Printf("notification: clicked (paneKey=%q) with no handler registered", paneKey)
		return
	}
	go fn(paneKey)
}
