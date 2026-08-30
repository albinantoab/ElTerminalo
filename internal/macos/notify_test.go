package macos

import (
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
)

// The whole point of these tests is the process they run in.
//
// A `go test` binary has no bundle identifier, and +[UNUserNotificationCenter
// currentNotificationCenter] responds to that by throwing an uncaught
// NSInternalInconsistencyException from inside its dispatch_once — which is
// SIGABRT, not an error return. So every entry point here has to answer without
// asking the notification center anything, and "this test file passes" is the
// assertion that it does.
//
// They are also what stands between a future edit and a `go test ./...` that
// takes the whole suite down with it.

func silenceLog(t *testing.T) {
	t.Helper()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
}

func TestNotificationsAreNotReadyWithoutABundle(t *testing.T) {
	if NotificationsReady() {
		t.Fatal("NotificationsReady is true in an unbundled test process")
	}
}

func TestInitNotificationsIsSafeWithoutABundle(t *testing.T) {
	silenceLog(t)
	// Twice: it is called once per launch, and a second call must not allocate
	// a second delegate or ask a second time.
	InitNotifications()
	InitNotifications()

	if NotificationsReady() {
		t.Fatal("NotificationsReady is true after InitNotifications in an unbundled process")
	}
}

func TestNotifyRefusesWhenUnavailable(t *testing.T) {
	silenceLog(t)
	InitNotifications()

	cases := []struct{ title, body, paneKey string }{
		{"Build finished", "npm run build exited 0", "tab-1/pane-2"},
		{"", "", ""},
		{strings.Repeat("x", 4096), strings.Repeat("y", 4096), strings.Repeat("z", 4096)},
		// Not valid UTF-8, which is exactly what a title scraped out of a
		// terminal escape sequence can be.
		{"\xff\xfe", "body\x00cut", "key\xc3"},
	}
	for _, c := range cases {
		if Notify(c.title, c.body, c.paneKey) {
			t.Fatalf("Notify(%q, ...) reported success with no notification center", c.title)
		}
	}
}

func TestSetDockBadgeIsSafeWithoutAnApplication(t *testing.T) {
	// NSApp is nil here, so this reaches [[nil dockTile] setBadgeLabel:] on the
	// main queue — which is never drained in a test binary either. Neither is
	// allowed to be a crash.
	for _, label := range []string{"", "3", "12", "…", "\xff", strings.Repeat("9", 64)} {
		SetDockBadge(label)
	}
}

// Notify and SetDockBadge are reached from Wails bindings, and Wails dispatches
// every binding call on its own goroutine.
func TestNotifyAndBadgeAreSafeForConcurrentUse(t *testing.T) {
	silenceLog(t)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				Notify("t", "b", "pane")
				SetDockBadge("9")
				_ = NotificationsReady()
				OnNotificationActivated(nil)
			}
		}()
	}
	wg.Wait()
}

func TestOnNotificationActivatedIsRegisterable(t *testing.T) {
	restoreActivationHandler(t)

	got := make(chan string, 1)
	OnNotificationActivated(func(paneKey string) { got <- paneKey })

	activation.mu.RLock()
	fn := activation.fn
	activation.mu.RUnlock()
	if fn == nil {
		t.Fatal("OnNotificationActivated did not register the handler")
	}

	OnNotificationActivated(nil)
	activation.mu.RLock()
	fn = activation.fn
	activation.mu.RUnlock()
	if fn != nil {
		t.Fatal("OnNotificationActivated(nil) did not unregister the handler")
	}
}

// The click path, minus the notification. The exported cgo callback in
// notify_darwin.go is a two-line shim over this — an internal test file may not
// import "C", so this is as close to the AppKit delegate as a test can get.
func TestNotificationActivatedReachesTheHandlerOffTheCallingGoroutine(t *testing.T) {
	silenceLog(t)
	restoreActivationHandler(t)

	OnNotificationActivated(nil)
	notificationActivated("nobody-is-listening") // must not panic

	got := make(chan string, 1)
	OnNotificationActivated(func(paneKey string) { got <- paneKey })

	notificationActivated("tab-3/pane-1")
	if key := <-got; key != "tab-3/pane-1" {
		t.Fatalf("the handler got %q, want %q", key, "tab-3/pane-1")
	}
}

// restoreActivationHandler puts back whatever was registered before the test,
// so -count=2 and every later test in this package start from the same place.
func restoreActivationHandler(t *testing.T) {
	t.Helper()
	activation.mu.RLock()
	prev := activation.fn
	activation.mu.RUnlock()
	t.Cleanup(func() { OnNotificationActivated(prev) })
}

// Beep and RequestAttention are the older half of this package and are reached
// from the same binding goroutines. Neither may need a window to be safe.
func TestBeepAndRequestAttentionAreSafeInATestProcess(t *testing.T) {
	Beep()
	RequestAttention()
}
