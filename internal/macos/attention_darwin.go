//go:build darwin

// Package macos wraps the few AppKit calls the terminal needs but that Wails
// does not expose: ringing the system alert sound and asking for the user's
// attention when a bell arrives in a window they are not looking at.
//
// Everything here is fire-and-forget and safe from any goroutine — see the
// dispatch note in the cgo preamble.
package macos

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework AppKit

#import <AppKit/AppKit.h>

// Both entry points hop onto the main queue before touching AppKit.
//
// They are called from a Wails binding, and Wails dispatches every binding call
// on its own goroutine — so "the caller's thread" is an arbitrary one, and
// NSBeep and NSApplication are not safe there. dispatch_async rather than
// dispatch_sync: a bell must never be able to block the goroutine that rang it,
// and dispatch_sync from whatever thread the Go runtime happens to have parked
// this goroutine on is also a deadlock waiting to be found.
//
// Both are therefore fire-and-forget, and a burst of bells coalesces into a
// burst of main-queue blocks rather than into a queue of blocked goroutines.

static void elterminalo_beep(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        NSBeep();
    });
}

static void elterminalo_request_attention(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        // Only when we are in the background. requestUserAttention: on the
        // active app is documented as a no-op, but the check also documents the
        // intent: this is for a build that finished while the user was reading
        // something else, not a way to make the front app bounce its own icon.
        //
        // NSInformationalRequest bounces the Dock icon once and then leaves the
        // tile marked; NSCriticalRequest bounces until the app is activated,
        // which is far too much for a terminal bell.
        if (![NSApp isActive]) {
            [NSApp requestUserAttention:NSInformationalRequest];
        }
    });
}
*/
import "C"

// Beep plays the system alert sound.
//
// Safe to call from any goroutine, and it returns immediately: the sound is
// played from a block on the main queue.
func Beep() {
	C.elterminalo_beep()
}

// RequestAttention bounces the Dock icon once, and only while the app is in the
// background. Safe to call from any goroutine, and it returns immediately.
func RequestAttention() {
	C.elterminalo_request_attention()
}
