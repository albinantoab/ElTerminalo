//go:build darwin

//  notify_darwin.h
//
//  Declarations for the UserNotifications and Dock-tile half of package macos.
//
//  The implementations are in notify_darwin.m rather than in a cgo preamble
//  because notify_darwin.go uses //export, and cgo copies such a file's
//  preamble into two generated C files. A preamble carrying the delegate's
//  @implementation would therefore define the class twice, and the ObjC runtime
//  resolves a duplicate class definition by warning at load and picking one.

#ifndef ELTERMINALO_NOTIFY_DARWIN_H
#define ELTERMINALO_NOTIFY_DARWIN_H

#include <stdbool.h>

// True when UNUserNotificationCenter can be used at all in this process. See
// notify_darwin.m for what is actually being asked; the short version is that
// an unbundled binary — a `go test` binary, or `wails dev`'s raw executable —
// aborts the process if it so much as asks for the notification center.
bool elterminalo_notifications_available(void);

// Sets the delegate and requests authorization. Safe to call when notifications
// are unavailable (it does nothing) and safe to call more than once.
void elterminalo_notifications_init(void);

// Queues one notification for immediate delivery and returns whether it was
// queued. Never blocks; the actual delivery happens on the main queue.
bool elterminalo_notify(const char *title, const char *body, const char *paneKey);

// Sets the Dock tile's badge. NULL or "" clears it. Never blocks.
void elterminalo_set_dock_badge(const char *label);

#endif
