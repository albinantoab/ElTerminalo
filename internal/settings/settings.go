// Package settings holds the user-editable preferences that live in
// ~/.config/elterminalo/config.json.
//
// The file is the only supported way to change how the terminal looks and
// behaves; there is no preferences window, and there is deliberately no code
// path that rewrites the file behind the user's back. Everything here is
// therefore built around one rule: whatever is on disk is the user's text, and
// a value we cannot use is repaired *in memory* — never in the file.
//
// Loading is a pure function of the bytes, with one exception called out on
// ResolveShell, so the whole validation table is testable without a
// filesystem.
package settings

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/albinanto/elterminalo/internal/config"
)

// FileName is the settings file's name inside the config directory.
const FileName = "config.json"

// filePerm keeps the settings file owner-only. It can name a shell outside the
// default $PATH and a font the user has installed — not secrets, but not
// something every account on the machine needs either, and it matches the mode
// every other file this app publishes carries.
const filePerm = 0o600

// Valid values for the "cursorStyle" key. They are the three xterm.js accepts;
// anything else would be dropped on the floor by the frontend.
const (
	CursorStyleBlock     = "block"
	CursorStyleUnderline = "underline"
	CursorStyleBar       = "bar"
)

// Valid values for the "bell" key. "sound" rings the system alert, "visual"
// flashes the pane, "both" does each, "none" does neither.
const (
	BellNone   = "none"
	BellSound  = "sound"
	BellVisual = "visual"
	BellBoth   = "both"
)

// Valid values for the "confirmQuit" key. "running" asks only when quitting
// would kill something, "always" asks unconditionally, "never" never asks.
const (
	ConfirmQuitRunning = "running"
	ConfirmQuitAlways  = "always"
	ConfirmQuitNever   = "never"
)

// Bounds for the numeric keys. Out-of-range values are clamped rather than
// rejected: a user who typed 200 for fontSize wants "as big as it goes", and
// silently falling back to 12 would look like the setting does nothing.
const (
	minFontSize   = 6
	maxFontSize   = 72
	minLineHeight = 1.0
	maxLineHeight = 2.0
	minScrollback = 0
	maxScrollback = 1_000_000

	// maxFontFamilyBytes caps the font stack. The frontend hands this string to
	// xterm.js, which puts it into a CSS font-family declaration, so an
	// unbounded value is a way to make the page's style sheet arbitrarily
	// large. A stack of a dozen families fits comfortably.
	maxFontFamilyBytes = 256
)

// DefaultFontFamily is the font stack the terminal falls back to. It has to
// stay in step with what the frontend hard-codes for a pane created before the
// settings arrive, or the first paint would differ from every later one.
const DefaultFontFamily = "'MonaspiceNe NFM', 'SF Mono', 'Menlo', monospace"

// Settings is the whole of config.json. Every field is exported to the
// frontend through the Wails bindings under exactly the JSON name here.
type Settings struct {
	FontFamily   string  `json:"fontFamily"`
	FontSize     float64 `json:"fontSize"`
	LineHeight   float64 `json:"lineHeight"`
	CursorStyle  string  `json:"cursorStyle"`
	CursorBlink  bool    `json:"cursorBlink"`
	Scrollback   int     `json:"scrollback"`
	Shell        string  `json:"shell"`
	OptionIsMeta bool    `json:"optionIsMeta"`
	Bell         string  `json:"bell"`
	CopyOnSelect bool    `json:"copyOnSelect"`
	ConfirmQuit  string  `json:"confirmQuit"`
}

// Defaults returns the settings an install with no config.json runs with.
//
// These are not arbitrary: the font, size, line height, cursor style and blink
// are the exact options the frontend passes to `new Terminal({...})` today, so
// adding a settings file changed nothing for anyone who never opens it.
func Defaults() Settings {
	return Settings{
		FontFamily:   DefaultFontFamily,
		FontSize:     12,
		LineHeight:   1.2,
		CursorStyle:  CursorStyleBlock,
		CursorBlink:  true,
		Scrollback:   10000,
		Shell:        "",
		OptionIsMeta: true,
		Bell:         BellSound,
		CopyOnSelect: true,
		ConfirmQuit:  ConfirmQuitRunning,
	}
}

// Path returns the location of the settings file inside configDir.
func Path(configDir string) string {
	return filepath.Join(configDir, FileName)
}

// Load reads config.json and returns a fully populated Settings.
//
// It never fails. A missing file is the ordinary first-run case and yields the
// defaults with no warning at all; an unreadable or unparseable one yields the
// defaults with a warning; and every individual key that is absent, of the
// wrong type or out of range is filled from the defaults with a warning naming
// it. The caller logs the warnings — this package does not, so the tests can
// assert on them and so a second Load for a different purpose does not double
// up the log.
//
// Nothing is ever written from here. A config.json that does not parse is left
// exactly as the user typed it; replacing a file full of edits with defaults
// because of one stray comma is the one outcome this function must not have.
func Load(configDir string) (Settings, []string) {
	s := Defaults()

	data, err := os.ReadFile(Path(configDir))
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, []string{fmt.Sprintf("%s cannot be read (%v); using the defaults", FileName, err)}
	}

	// Decoded a key at a time, into a map first, rather than straight into
	// Settings. Two things go wrong with the direct decode. encoding/json
	// reports only the *first* type error in a document and leaves that field
	// zeroed, so one mistyped line is both unnameable and indistinguishable
	// from an absent key. And a bool that is simply missing lands as false,
	// which would silently turn off every key whose default is true —
	// cursorBlink, optionIsMeta, copyOnSelect — for anyone whose file was
	// written before that key existed.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return s, []string{fmt.Sprintf(
			"%s does not parse (%v); using the defaults — the file is left untouched so the edit can be fixed",
			FileName, err)}
	}

	l := &loader{raw: raw}

	if v := new(string); l.decode("fontFamily", v) {
		if clean := sanitizeFontFamily(*v); clean != "" {
			s.FontFamily = clean
		} else {
			l.warnf("fontFamily is empty; keeping %q", s.FontFamily)
		}
	}
	if v := new(float64); l.decode("fontSize", v) {
		s.FontSize = l.clampFloat("fontSize", *v, minFontSize, maxFontSize, s.FontSize)
	}
	if v := new(float64); l.decode("lineHeight", v) {
		s.LineHeight = l.clampFloat("lineHeight", *v, minLineHeight, maxLineHeight, s.LineHeight)
	}
	if v := new(string); l.decode("cursorStyle", v) {
		s.CursorStyle = l.oneOf("cursorStyle", *v, s.CursorStyle,
			CursorStyleBlock, CursorStyleUnderline, CursorStyleBar)
	}
	if v := new(bool); l.decode("cursorBlink", v) {
		s.CursorBlink = *v
	}
	if v := new(int); l.decode("scrollback", v) {
		s.Scrollback = int(l.clampFloat("scrollback", float64(*v), minScrollback, maxScrollback, float64(s.Scrollback)))
	}
	if v := new(string); l.decode("shell", v) {
		s.Shell = l.shellPath(*v)
	}
	if v := new(bool); l.decode("optionIsMeta", v) {
		s.OptionIsMeta = *v
	}
	if v := new(string); l.decode("bell", v) {
		s.Bell = l.oneOf("bell", *v, s.Bell, BellNone, BellSound, BellVisual, BellBoth)
	}
	if v := new(bool); l.decode("copyOnSelect", v) {
		s.CopyOnSelect = *v
	}
	if v := new(string); l.decode("confirmQuit", v) {
		s.ConfirmQuit = l.oneOf("confirmQuit", *v, s.ConfirmQuit,
			ConfirmQuitRunning, ConfirmQuitAlways, ConfirmQuitNever)
	}

	// Keys we do not know are ignored in silence. They are how a config file
	// written by a newer build survives a downgrade, and how a user leaves
	// themselves a note.

	return s, l.warnings
}

// Save writes s to config.json.
//
// Pretty-printed with a trailing newline because the file exists to be opened
// in an editor, and durable — through the same temp-fsync-rename path the saved
// layout uses — because a torn write here is a file the user then has to repair
// by hand.
func Save(configDir string, s Settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return config.WriteFileDurable(Path(configDir), data, filePerm)
}

// ResolveShell decides which shell a new session spawns.
//
// configured is the settings file's "shell" and fallback is what the app would
// use without it ($SHELL, or /bin/zsh). The second return value is a warning to
// log when the configured shell had to be rejected, and is empty otherwise.
//
// This is the one part of the package that touches the filesystem, which is why
// it is not folded into Load: a shell on a volume that happens to be unmounted
// when settings are read must not cause the key to be reported as broken every
// time something asks for the font size.
func ResolveShell(configured, fallback string) (shell, warning string) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return fallback, ""
	}
	if !filepath.IsAbs(configured) {
		return fallback, fmt.Sprintf("shell %q is not an absolute path; using %q instead", configured, fallback)
	}
	// Stat, not Lstat: /bin/sh and every shell installed by Homebrew is reached
	// through at least one symlink, and following them is the whole point.
	info, err := os.Stat(configured)
	if err != nil {
		return fallback, fmt.Sprintf("shell %q cannot be used (%v); using %q instead", configured, err, fallback)
	}
	if !info.Mode().IsRegular() {
		return fallback, fmt.Sprintf("shell %q is not a regular file; using %q instead", configured, fallback)
	}
	// Any of the three execute bits. Checking only the owner's would reject a
	// shell installed 0755 root:wheel for every user but root.
	if info.Mode().Perm()&0o111 == 0 {
		return fallback, fmt.Sprintf("shell %q is not executable; using %q instead", configured, fallback)
	}
	return configured, ""
}

// loader accumulates the warnings produced while decoding one document.
type loader struct {
	raw      map[string]json.RawMessage
	warnings []string
}

func (l *loader) warnf(format string, args ...any) {
	l.warnings = append(l.warnings, fmt.Sprintf(format, args...))
}

// decode pulls key out of the document into v. It reports false when the key is
// absent — leave the default alone, no warning — and when the value is the
// wrong shape, in which case a warning naming the key has been recorded.
func (l *loader) decode(key string, v any) bool {
	msg, ok := l.raw[key]
	if !ok {
		return false
	}
	// A literal null is "no value", and has to be handled here rather than left
	// to json.Unmarshal: unmarshalling null into a float64 or a bool succeeds
	// and leaves the destination *unchanged*, which for the zero value we hand
	// in reads as an explicit 0 or false — so `"fontSize": null` would be
	// clamped up to the minimum instead of falling back to 12.
	if string(msg) == "null" {
		return false
	}
	if err := json.Unmarshal(msg, v); err != nil {
		l.warnf("%s is not the expected type (%v); keeping the default", key, err)
		return false
	}
	return true
}

// clampFloat brings v inside [lo, hi], warning about what it had to change.
// A NaN or an infinity is not clampable and falls back to the default.
func (l *loader) clampFloat(key string, v, lo, hi, fallback float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		l.warnf("%s is not a finite number; keeping %v", key, fallback)
		return fallback
	}
	switch {
	case v < lo:
		l.warnf("%s is %v, below the minimum %v; clamped", key, v, lo)
		return lo
	case v > hi:
		l.warnf("%s is %v, above the maximum %v; clamped", key, v, hi)
		return hi
	}
	return v
}

// oneOf normalises v against a closed set of values, falling back when it
// matches none of them. Comparison is case- and space-insensitive so that
// "Block" and " block " both work; the stored value is always the canonical
// lower-case spelling, which is what the frontend switches on.
func (l *loader) oneOf(key, v, fallback string, allowed ...string) string {
	norm := strings.ToLower(strings.TrimSpace(v))
	for _, a := range allowed {
		if norm == a {
			return a
		}
	}
	l.warnf("%s is %q, which is not one of %s; keeping %q", key, v, strings.Join(allowed, ", "), fallback)
	return fallback
}

// shellPath validates the "shell" key's shape — absolute, printable — and
// returns "" (meaning "use the default shell") when it is unusable. Whether the
// path names something executable is ResolveShell's question, not this one's.
func (l *loader) shellPath(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.ContainsFunc(v, unicode.IsControl) {
		l.warnf("shell contains control characters; ignoring it and using the default shell")
		return ""
	}
	if !filepath.IsAbs(v) {
		l.warnf("shell %q is not an absolute path; ignoring it and using the default shell", v)
		return ""
	}
	return filepath.Clean(v)
}

// fontFamilyForbidden are the characters dropped from a font stack on top of
// the invisible classes. None of them can appear in a font family name, and
// each one is a way out of the declaration the value is interpolated into:
//
//   - ";" ends the declaration and starts another, and "}" ends the whole rule.
//     Between them, "monospace; } .xterm-rows { display:none } x{" is a value
//     that rewrites the page's style sheet.
//   - "\" is CSS's escape character, so leaving it in leaves a way to spell any
//     of the others past a filter that only looks for the literal characters.
//   - "<" and ">" because the declaration lands inside a <style> element, where
//     "</style" ends the element itself whatever the CSS parser makes of it.
const fontFamilyForbidden = ";{}\\<>"

// sanitizeFontFamily strips what cannot legally appear in a CSS font-family
// list and bounds the length.
//
// The value crosses into the page and is set as a style — xterm.js's DOM
// renderer interpolates it into a <style> element as `font-family: ${value};`,
// with no escaping of its own — so this is the only thing standing between the
// file and the page's CSS. Control characters and the format/line-separator
// classes go for the same reason they go from a log line: they are invisible or
// they break the surrounding syntax. fontFamilyForbidden goes because it is
// what turns a value into a rule.
//
// Quotes, commas, spaces, hyphens, digits and letters all survive: a real stack
// is `'MonaspiceNe NFM', 'SF Mono', Menlo, monospace`, and a sanitiser that
// broke that would be worked around rather than used.
//
// config.json is the user's own 0600 file, so nothing here crosses a privilege
// boundary — this is a typo and a copy-paste guard, not a defence against an
// attacker who can already write the file. Returns "" when nothing usable is
// left, and the caller keeps the default.
func sanitizeFontFamily(v string) string {
	var b strings.Builder
	b.Grow(len(v))
	for _, r := range v {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			continue
		}
		if strings.ContainsRune(fontFamilyForbidden, r) {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if len(out) > maxFontFamilyBytes {
		cut := maxFontFamilyBytes
		// Never leave half a rune behind: the tail would render as a
		// replacement character inside a font name.
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = strings.TrimSpace(out[:cut])
	}
	return out
}
