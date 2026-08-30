//go:build darwin

//  notify_darwin.m
//
//  Native notifications and the Dock tile badge.

#import <AppKit/AppKit.h>
#import <UserNotifications/UserNotifications.h>

#import "notify_darwin.h"

// The declarations of the //export'ed Go functions this file calls back into:
// elterminaloNotificationAuthorized, elterminaloNotificationActivated and
// elterminaloNotificationFailed. cgo generates the header.
#include "_cgo_export.h"

// paneKeyField is the userInfo key the pane identifier travels under. Go picks
// the value and never looks at it; this file only has to put it in and take it
// back out again, so the two ends only share this one string.
static NSString *const paneKeyField = @"paneKey";

// The delegate is stored strongly here because UNUserNotificationCenter's
// delegate property is `weak`. Assigning a freshly allocated object to it and
// returning would leave the center pointing at deallocated memory, and the
// symptom is a crash on the first notification the user clicks — long after the
// code that caused it ran. It is never released: it lives as long as the app.
static id gDelegate = nil;

// available reports whether asking for the notification center is safe.
//
// Two guards, and only the first one is load-bearing. In a process with no
// bundle identifier — a `go test` binary, or the raw executable `wails dev`
// builds — +[UNUserNotificationCenter currentNotificationCenter] throws
// NSInternalInconsistencyException ("bundleProxyForCurrentProcess is nil") from
// inside its dispatch_once, which is an uncaught ObjC exception and therefore
// SIGABRT: the process is gone, not the call. Measured, not assumed.
//
// The class check is the cheaper, second question — whether the framework is
// present at all — and it is *not* a substitute for the first: the class loads
// fine in an unbundled process, it is the center behind it that does not exist.
static bool available(void) {
    if ([[NSBundle mainBundle] bundleIdentifier] == nil) {
        return false;
    }
    if (NSClassFromString(@"UNUserNotificationCenter") == nil) {
        return false;
    }
    return true;
}

// text converts one of Go's C strings into an NSString, never returning nil.
//
// +stringWithUTF8String: returns nil for bytes that are not valid UTF-8, and a
// Go string is only conventionally UTF-8 — the title and body ultimately come
// from a terminal escape sequence, which is to say from whatever the user ran.
// A nil title assigned to UNMutableNotificationContent (whose properties are
// declared nonnull) and a nil value in the userInfo dictionary literal are both
// ways to throw from a background queue, so neither is allowed to happen.
static NSString *text(const char *s) {
    if (s == NULL) {
        return @"";
    }
    NSString *out = [NSString stringWithUTF8String:s];
    return out != nil ? out : @"";
}

@interface ElTerminaloNotificationDelegate : NSObject <UNUserNotificationCenterDelegate>
@end

@implementation ElTerminaloNotificationDelegate

// Without this, macOS suppresses a notification while the app is frontmost.
// That is the wrong default for a terminal: "frontmost" says nothing about
// whether the user is looking at the pane whose build just finished — they are
// usually in another tab, or another pane in the same window. So it is always
// presented as a banner.
//
// Banner and not List: the notification is about a moment, and leaving a row in
// Notification Center for every command that finished would turn it into a log.
- (void)userNotificationCenter:(UNUserNotificationCenter *)center
       willPresentNotification:(UNNotification *)notification
         withCompletionHandler:(void (^)(UNNotificationPresentationOptions options))completionHandler {
    completionHandler(UNNotificationPresentationOptionBanner);
}

// The user clicked the notification. Hand the pane key back to Go, which brings
// the window forward and tells the frontend which pane to focus.
//
// Only the default action counts. A dismissal also arrives here, with
// UNNotificationDismissActionIdentifier, and swapping the user's pane out from
// under them because they swiped a banner away would be the opposite of what
// they asked for.
- (void)userNotificationCenter:(UNUserNotificationCenter *)center
didReceiveNotificationResponse:(UNNotificationResponse *)response
         withCompletionHandler:(void (^)(void))completionHandler {
    if ([response.actionIdentifier isEqualToString:UNNotificationDefaultActionIdentifier]) {
        id key = response.notification.request.content.userInfo[paneKeyField];
        if ([key isKindOfClass:[NSString class]]) {
            // -UTF8String hands back a buffer owned by an autoreleased NSString.
            // The Go side copies it before it returns — see the comment there —
            // so it cannot outlive this call.
            elterminaloNotificationActivated((char *)[(NSString *)key UTF8String]);
        }
    }
    // Always called, and called last: the system holds the response open until
    // it is, and a delegate that forgets it is a hang the user sees as the
    // notification never going away.
    completionHandler();
}

@end

void elterminalo_notifications_init(void) {
    if (!available()) {
        return;
    }
    // Onto the main queue for the same reason everything in attention_darwin.go
    // is: this is called from Wails' startup callback, on whichever thread the
    // Go runtime parked that goroutine on.
    //
    // Apple documents that the delegate "must be set before the application
    // returns from application:didFinishLaunchingWithOptions:". Wails runs
    // OnStartup after that point, so a notification that *launches* the app
    // from a cold start could have its response delivered before this runs. It
    // does not matter here: every notification this app sends is about a pane
    // in a window that is already open, so the app is always already running
    // when the user clicks one.
    dispatch_async(dispatch_get_main_queue(), ^{
        UNUserNotificationCenter *center = [UNUserNotificationCenter currentNotificationCenter];
        if (center == nil) {
            return;
        }
        if (gDelegate == nil) {
            gDelegate = [[ElTerminaloNotificationDelegate alloc] init];
        }
        center.delegate = gDelegate;

        // Alert only. The bell already owns sound — it has its own rate limiter
        // in app.go, and asking for UNAuthorizationOptionSound here would let a
        // notification ring a second time underneath it. Badge is not requested
        // either: the Dock badge below is [NSApp dockTile], which needs no
        // authorization at all.
        //
        // The first call shows the system's permission prompt; every call after
        // that answers from the recorded decision without prompting, which is
        // why this is safe to run on every launch.
        [center requestAuthorizationWithOptions:UNAuthorizationOptionAlert
                              completionHandler:^(BOOL granted, NSError *error) {
            const char *reason = "";
            if (error != nil) {
                reason = [[error localizedDescription] UTF8String];
                if (reason == NULL) {
                    reason = "";
                }
            }
            // An int rather than the BOOL itself: BOOL is _Bool on arm64 but
            // signed char on x86_64, and a universal build would otherwise need
            // the Go side to have guessed which.
            elterminaloNotificationAuthorized(granted ? 1 : 0, (char *)reason);
        }];
    });
}

bool elterminalo_notifications_available(void) {
    return available();
}

bool elterminalo_notify(const char *title, const char *body, const char *paneKey) {
    if (!available()) {
        return false;
    }

    // Converted here, on the calling thread, because the C strings belong to
    // Go: it frees them the moment this function returns, so nothing the block
    // below runs may still be pointing at them.
    NSString *nsTitle = text(title);
    NSString *nsBody = text(body);
    NSString *nsPaneKey = text(paneKey);

    // Retained explicitly rather than leaning on the retain a block copy
    // performs on its captured objects. The three strings are autoreleased into
    // whatever pool belongs to the thread cgo parked this call on, and that pool
    // is not one this file can reason about.
    [nsTitle retain];
    [nsBody retain];
    [nsPaneKey retain];

    dispatch_async(dispatch_get_main_queue(), ^{
        UNMutableNotificationContent *content = [[UNMutableNotificationContent alloc] init];
        content.title = nsTitle;
        content.body = nsBody;
        content.userInfo = @{paneKeyField: nsPaneKey};

        // A nil trigger means "deliver now". A UUID identifier means every
        // notification is its own: reusing an identifier replaces the delivered
        // notification that already carries it, which would silently collapse
        // two panes finishing at once into one banner.
        UNNotificationRequest *request =
            [UNNotificationRequest requestWithIdentifier:[[NSUUID UUID] UUIDString]
                                                 content:content
                                                 trigger:nil];

        [[UNUserNotificationCenter currentNotificationCenter]
            addNotificationRequest:request
             withCompletionHandler:^(NSError *error) {
                 if (error != nil) {
                     const char *reason = [[error localizedDescription] UTF8String];
                     elterminaloNotificationFailed((char *)(reason != NULL ? reason : ""));
                 }
             }];

        // requestWithIdentifier:content:trigger: copies the content, so it is
        // ours to release as soon as the request exists.
        [content release];
        [nsTitle release];
        [nsBody release];
        [nsPaneKey release];
    });

    return true;
}

void elterminalo_set_dock_badge(const char *label) {
    // nil, not @"": setBadgeLabel:@"" leaves an empty red bubble on the tile,
    // and the contract for an empty label is that the badge goes away.
    NSString *badge = (label != NULL && label[0] != '\0') ? [NSString stringWithUTF8String:label] : nil;
    [badge retain];
    dispatch_async(dispatch_get_main_queue(), ^{
        // NSApp is nil in a process that never brought up an NSApplication — a
        // `go test` binary, say. Messaging nil is a no-op in Objective-C, so
        // this whole line is one, which is what makes SetDockBadge safe to call
        // from a test.
        [[NSApp dockTile] setBadgeLabel:badge];
        [badge release];
    });
}
