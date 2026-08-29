// Package logging gives the app a log file that survives the session.
//
// Everything used to go to stderr. For a binary started from a terminal that is
// fine; for a .app launched from the Dock or Finder — which is how every user
// runs this — stderr goes nowhere retrievable, and console.error inside
// WKWebView is invisible unless Safari's inspector happens to be attached. When
// folder access broke mid-session there was simply nothing to read afterwards.
//
// Init points the standard log package at a file under the config directory (and
// still at stderr, so `wails dev` keeps showing lines live). Every other package
// logs with plain log.Printf and needs to know nothing about this one.
package logging

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// logDirName is created under the config directory. A subdirectory rather
	// than a bare file so rotated generations do not litter the place the user
	// goes to edit themes and commands.
	logDirName  = "logs"
	logFileName = "elterminalo.log"

	// maxLogBytes is the size at which the live file is rotated. 2 MiB is a few
	// tens of thousands of lines — enough to cover a long session, small enough
	// that a user can attach the whole set to a bug report.
	maxLogBytes = 2 << 20

	// keptRotations is how many generations survive beyond the live file:
	// elterminalo.log.1 and elterminalo.log.2. Three files in total.
	keptRotations = 2

	// logPerm keeps the log owner-only. Lines carry working directories, shell
	// names and dropped file paths — the user's project layout.
	logPerm = 0o600

	// maxMessageBytes bounds a single sanitised line. A caught exception can
	// carry a whole stack trace, and a terminal error can carry a screenful of
	// escape-laden output; either would otherwise be able to fill the log alone.
	maxMessageBytes = 4096
	// maxFrontendMessageBytes is the tighter cap on a line that came from the
	// webview. The frontend is the one source that can produce lines faster than
	// a human reads them and is also the least likely to be saying something
	// that needs four kilobytes to say; a quarter of the general budget means a
	// burst that gets past the rate limiter still cannot dominate the bytes the
	// log has to spend on the backend.
	maxFrontendMessageBytes = maxMessageBytes / 4
	// truncatedMarker replaces the tail of an over-long message and is counted
	// against the cap, so the logged text is never larger than it.
	truncatedMarker = "[truncated]"

	// Frontend rate limit. LogMessage is a binding, so the webview can call it
	// from a render loop or an error handler that itself errors: measured, 100k
	// frontend lines push every backend line out through all three rotations,
	// and the log that exists to explain a failure ends up holding nothing but
	// the symptom. A token bucket keeps a genuine burst — an exception storm
	// during startup, say — while capping the sustained rate well below what it
	// takes to evict anything.
	//
	// 50 and 5/s: a burst covers a page of startup errors at once, and the
	// steady rate fills the 2 MiB live file with frontend lines in roughly two
	// hours rather than in seconds.
	frontendBurst          = 50
	frontendRefillPerSec   = 5
	frontendDropSummaryGap = time.Minute
)

var (
	mu sync.Mutex
	// current is the writer log points at, kept so a second Init can close the
	// first one instead of leaking its descriptor.
	current     *rotatingWriter
	currentPath string
)

// Init creates <configDir>/logs, opens the log file for append and makes the
// standard log package write to it as well as to stderr. It returns the path of
// the live log file.
//
// A returned error means only that lines will not reach disk; log.Printf keeps
// working (its default destination is stderr), so callers should report the
// error and carry on rather than refuse to start.
func Init(configDir string) (string, error) {
	dir := filepath.Join(configDir, logDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("cannot create log directory %s: %w", dir, err)
	}
	// MkdirAll only applies its mode to directories it actually creates, so a
	// logs/ left at 0755 by an older build — or by a user's umask — keeps it
	// forever, and a world-readable directory listing is enough to learn the
	// user's project names from the rotated generations. Best-effort: a
	// directory we cannot chmod is still a directory we can log into, and
	// refusing to log is by far the worse outcome.
	if err := os.Chmod(dir, 0o700); err != nil {
		log.Printf("logging: could not tighten the mode of %s: %v", dir, err)
	}

	path := filepath.Join(dir, logFileName)
	w, err := newRotatingWriter(path, maxLogBytes, keptRotations)
	if err != nil {
		return "", fmt.Errorf("cannot open log file %s: %w", path, err)
	}

	mu.Lock()
	prev := current
	current, currentPath = w, path
	mu.Unlock()
	if prev != nil {
		_ = prev.Close()
	}

	// Microseconds because the interesting questions this log answers are about
	// ordering — did the frontend save before shutdown ran, did the PTY exit
	// before or after the pane subscribed — and second resolution loses them.
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(io.MultiWriter(w, os.Stderr))
	return path, nil
}

// Path returns the live log file's path, or "" when Init never succeeded.
func Path() string {
	mu.Lock()
	defer mu.Unlock()
	return currentPath
}

// Frontend records a line the webview asked us to log. The frontend has no
// other way to leave a trace: console output inside WKWebView is not written
// anywhere a user can retrieve after the fact.
//
// level is normalised to info/warn/error; anything else is info, because a
// mislabelled line is still worth keeping.
//
// Rate limited — see frontendLimiter. A dropped line is not recovered; what the
// log gets instead is a periodic count, so a reader can tell "the frontend was
// quiet" apart from "the frontend was screaming and we stopped listening".
func Frontend(level, message string) {
	allowed, dropped := frontendLimiter.take(frontendClock())
	if dropped > 0 {
		log.Printf("[frontend] warn: dropped %d messages (rate limited)", dropped)
	}
	if !allowed {
		return
	}
	log.Printf("[frontend] %s: %s", normalizeLevel(level), sanitize(message, maxFrontendMessageBytes))
}

var (
	// frontendClock is the clock the limiter reads. A variable so tests can step
	// time instead of sleeping through a refill window.
	frontendClock = time.Now

	frontendLimiter = newTokenBucket(frontendBurst, frontendRefillPerSec)
)

// tokenBucket is the rate limiter behind Frontend: burst tokens available at
// once, refill per second, one token per message.
type tokenBucket struct {
	mu     sync.Mutex
	burst  float64
	refill float64

	tokens float64
	// last is when tokens was last brought up to date. Zero until the first
	// take, so the bucket starts full rather than crediting the refill for every
	// second since the zero time.
	last time.Time
	// dropped counts messages refused since the last summary line, and summaryAt
	// is the earliest the next one may be written.
	dropped   int
	summaryAt time.Time
}

func newTokenBucket(burst, refillPerSecond float64) *tokenBucket {
	return &tokenBucket{burst: burst, refill: refillPerSecond, tokens: burst}
}

// take spends a token and reports whether the caller may log.
//
// The second return value is non-zero at most once per frontendDropSummaryGap
// and carries how many messages have been dropped since the previous summary;
// the caller logs that as a single line. The very first drop reports 1
// immediately — that line is the marker that says the log is incomplete from
// here on, which is worth more than waiting a minute to say it precisely — and
// every summary after it covers a whole window.
//
// A pending count is flushed on the first call at or after the window closes
// whether or not *that* call was itself dropped. Flushing only on a drop would
// mean a flood that stops never gets its final count into the log at all, which
// is the case a reader most needs told: the messages after the gap are the ones
// they will be trying to line up against the ones that are missing.
func (b *tokenBucket) take(now time.Time) (allowed bool, dropped int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.last.IsZero() {
		if elapsed := now.Sub(b.last); elapsed > 0 {
			b.tokens += elapsed.Seconds() * b.refill
			if b.tokens > b.burst {
				b.tokens = b.burst
			}
		}
	}
	b.last = now

	allowed = b.tokens >= 1
	if allowed {
		b.tokens--
	} else {
		b.dropped++
	}

	if b.dropped > 0 && !now.Before(b.summaryAt) {
		dropped = b.dropped
		b.dropped = 0
		b.summaryAt = now.Add(frontendDropSummaryGap)
	}
	return allowed, dropped
}

// normalizeLevel maps the frontend's level onto the three we record.
func normalizeLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "warn":
		return "warn"
	case "error":
		return "error"
	default:
		return "info"
	}
}

// sanitize makes an arbitrary frontend string safe to put in a log file: one
// line, bounded length.
//
// Control characters are dropped rather than escaped, newlines included. A
// newline in the middle of a message would look exactly like the start of a new
// entry, so anything that can reach the frontend logger — a filename, a shell's
// output, a caught exception's text — could otherwise forge log lines and make
// the log lie about what happened. Escape sequences in a message would also be
// re-interpreted by whatever terminal the user later cats the file in.
//
// Cc alone is not enough for that, which is why Cf, Zl and Zp go too:
//
//   - Zl is U+2028 LINE SEPARATOR and Zp is U+2029 PARAGRAPH SEPARATOR. Neither
//     is a control character, both are line breaks to a great many viewers, and
//     JavaScript emits them verbatim — so a directory or command name carrying
//     one splits the log entry in two in whatever the user reads it with.
//   - Cf is the format characters, and it is the interesting one. U+202E
//     RIGHT-TO-LEFT OVERRIDE and U+2066 LEFT-TO-RIGHT ISOLATE reverse the
//     rendered order of everything after them, so a repository that names a file
//     or a script with one can make a log line read as the opposite of what
//     happened. U+200B ZERO WIDTH SPACE is invisible and can be used to break up
//     a string someone is grepping for. None of them can appear in a message
//     that is genuinely worth reading.
//
// Everything else — including U+FFFD, which is what invalid UTF-8 decodes to
// here — survives, so malformed input shows up as replacement characters rather
// than being silently swallowed.
func sanitize(s string, limit int) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			continue
		}
		b.WriteRune(r)
	}
	return truncate(b.String(), limit)
}

// truncate caps s at limit bytes, marker included.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit - len(truncatedMarker)
	// Never split a rune: half a UTF-8 sequence shows up as a replacement
	// character in the log and makes the line's real length depend on where the
	// cut happened to land.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedMarker
}

// rotatingWriter appends to path and moves it aside once it grows past
// maxBytes, keeping `keep` older generations named path.1 … path.<keep>.
type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int

	f      *os.File
	size   int64
	closed bool
}

func newRotatingWriter(path string, maxBytes int64, keep int) (*rotatingWriter, error) {
	w := &rotatingWriter{path: path, maxBytes: maxBytes, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// Write never reports an error to its caller.
//
// It is half of an io.MultiWriter whose other half is stderr, and MultiWriter
// stops at the first writer that fails — so a returned error here would silence
// the console too, turning a full disk into total blindness. A line that cannot
// reach the file is dropped instead; logging itself keeps working.
func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return len(p), nil
	}
	if w.f == nil {
		// The file was lost — a rotation that could not complete, or a user who
		// deleted the directory mid-session. Keep trying on every line: log
		// volume is low, and getting the file back is worth more than the cost
		// of an open that fails.
		if err := w.open(); err != nil {
			return len(p), nil
		}
	}

	// The size>0 guard matters for a single line larger than the whole budget:
	// without it every such write would rotate first and produce an endless run
	// of empty generations.
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		w.rotate()
	}

	n, err := w.f.Write(p)
	w.size += int64(n)
	if err != nil {
		// Drop the descriptor so the next line retries from scratch.
		_ = w.f.Close()
		w.f = nil
	}
	return len(p), nil
}

// Close releases the file. A closed writer stops accepting lines rather than
// reopening the path, so a superseded writer cannot keep a second descriptor on
// the file its replacement is using.
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.f == nil {
		return nil
	}
	f := w.f
	w.f = nil
	return f.Close()
}

// open attaches to path in append mode and picks up its current length, so a
// restart into an existing log rotates on the real total rather than on what
// this process alone has written.
func (w *rotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, logPerm)
	if err != nil {
		return err
	}
	// OpenFile's mode is only used for a file it creates, and the umask filters
	// it even then. A log file an older build left at 0644 therefore stays 0644
	// for as long as the install lives — and rotation's os.Rename carries that
	// mode straight into .1 and .2, so the whole set stays readable. Chmod on
	// the descriptor is not filtered by the umask and fixes it in place.
	// Best-effort for the same reason as the directory: never stop logging over
	// a mode.
	if err := f.Chmod(logPerm); err != nil {
		log.Printf("logging: could not tighten the mode of %s: %v", w.path, err)
	}
	var size int64
	if info, err := f.Stat(); err == nil {
		size = info.Size()
	}
	w.f, w.size = f, size
	return nil
}

// rotate shifts the generations down — the oldest is dropped, the live file
// becomes .1 — and starts a fresh live file.
//
// Every step is best-effort. If any of them fails the writer ends up holding
// either the file it already had or the one it just moved aside, both of which
// still accept lines; a log that grows past its cap is a much smaller problem
// than a log that stops. Losing the ability to log is the one outcome this
// package must never produce.
func (w *rotatingWriter) rotate() {
	_ = w.f.Close()
	w.f = nil

	if w.keep < 1 {
		// Nothing is kept: the live file is simply restarted.
		_ = os.Remove(w.path)
		_ = w.open()
		return
	}

	// Drop the oldest generation, then shift the rest down. Rename replaces its
	// destination, so no other generation needs removing first — and not
	// removing them means a rename that fails leaves the older copy intact
	// rather than deleting it for nothing.
	_ = os.Remove(w.generation(w.keep))
	for i := w.keep; i > 1; i-- {
		_ = os.Rename(w.generation(i-1), w.generation(i))
	}

	if err := os.Rename(w.path, w.generation(1)); err != nil {
		// Could not move the live file aside; keep appending to it.
		_ = w.open()
		return
	}
	if err := w.open(); err != nil {
		// The fresh file could not be created. Put the rotated one back so the
		// next Write has something to reopen instead of nothing at all.
		_ = os.Rename(w.generation(1), w.path)
		_ = w.open()
	}
}

func (w *rotatingWriter) generation(n int) string {
	return fmt.Sprintf("%s.%d", w.path, n)
}
