//go:build darwin

package macos

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework AppKit -framework UserNotifications

#include <stdlib.h>
#include "notify_darwin.h"
*/
import "C"

import (
	"log"
	"sync/atomic"
	"unsafe"
)

// Everything in this file is safe to call from any goroutine and from a process
// where notifications are not available at all — an unbundled binary, which is
// what `go test` and `wails dev` produce. See notify_darwin.m for what
// "available" is actually asking and why getting it wrong aborts the process
// rather than returning an error.

// authorized records what the user answered when macOS asked. The answer
// arrives on a system queue, minutes after InitNotifications returned, and is
// read from binding goroutines, so it is atomic rather than a plain bool.
//
// It is not re-checked after startup. A user who revokes notifications in
// System Settings mid-session leaves this true, and Notify goes on reporting
// success while the system quietly drops the banners — the delivery failure is
// logged, which is the trail that explains it. Re-asking the system on every
// notification would mean an async round trip per call, for a state that
// changes about once in the life of an install.
var authorized atomic.Bool

// InitNotifications sets the notification delegate and asks the user for
// permission, if this process can use notifications at all.
//
// Called once from startup. It returns immediately: the authorization prompt is
// answered by a human, so the result arrives later through
// elterminaloNotificationAuthorized. Until then NotificationsReady reports
// false and Notify refuses, which is the right answer — there is genuinely no
// permission yet.
func InitNotifications() {
	C.elterminalo_notifications_init()
}

// NotificationsReady reports whether a call to Notify would actually reach the
// user: notifications are usable in this process and the user has granted
// permission.
func NotificationsReady() bool {
	return bool(C.elterminalo_notifications_available()) && authorized.Load()
}

// Notify posts a native notification and reports whether it was queued. It
// returns false when notifications are unavailable or unauthorized, and never
// blocks — delivery happens on the main queue after this returns.
//
// paneKey is opaque here. Whatever the caller passes comes back through the
// handler registered with OnNotificationActivated when the user clicks the
// notification.
func Notify(title, body, paneKey string) bool {
	if !NotificationsReady() {
		return false
	}

	cTitle := C.CString(title)
	defer C.free(unsafe.Pointer(cTitle))
	cBody := C.CString(body)
	defer C.free(unsafe.Pointer(cBody))
	cPaneKey := C.CString(paneKey)
	defer C.free(unsafe.Pointer(cPaneKey))

	// The C side copies all three into NSStrings before it returns, so the
	// deferred frees above cannot pull the bytes out from under the delivery.
	return bool(C.elterminalo_notify(cTitle, cBody, cPaneKey))
}

// SetDockBadge sets the badge on the app's Dock tile. An empty label clears it.
//
// The label is used as given — callers are expected to have bounded it; see
// App.SetDockBadge. Unlike a notification this needs no authorization and works
// whether or not the app is bundled.
func SetDockBadge(label string) {
	cLabel := C.CString(label)
	defer C.free(unsafe.Pointer(cLabel))
	C.elterminalo_set_dock_badge(cLabel)
}

//export elterminaloNotificationAuthorized
func elterminaloNotificationAuthorized(granted C.int, reason *C.char) {
	ok := granted != 0
	authorized.Store(ok)
	if ok {
		log.Printf("notifications: authorized")
		return
	}
	// The one line that answers "why did I never get a notification". Denial
	// with no error is the user having said no, either just now or on some
	// earlier launch — the system remembers and stops asking.
	if msg := C.GoString(reason); msg != "" {
		log.Printf("notifications: not authorized: %s", msg)
	} else {
		log.Printf("notifications: not authorized; enable them in System Settings › Notifications › El Terminalo")
	}
}

//export elterminaloNotificationActivated
func elterminaloNotificationActivated(paneKey *C.char) {
	// C.GoString first and nothing else: the buffer belongs to an autoreleased
	// NSString on the AppKit side and is only valid for the length of this call,
	// while the goroutine notificationActivated starts outlives it.
	notificationActivated(C.GoString(paneKey))
}

//export elterminaloNotificationFailed
func elterminaloNotificationFailed(reason *C.char) {
	// Reached when the system accepted the request and then refused to show it
	// — most often because notifications were turned off for this app after
	// authorization was granted. See the note on `authorized`.
	log.Printf("notification: the system did not deliver it: %s", C.GoString(reason))
}
