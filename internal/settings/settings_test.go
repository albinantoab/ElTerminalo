package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig drops a config.json into a fresh temp dir and returns the dir.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", FileName, err)
	}
	return dir
}

// warnsAbout reports whether any warning mentions key.
func warnsAbout(warnings []string, key string) bool {
	for _, w := range warnings {
		if strings.Contains(w, key) {
			return true
		}
	}
	return false
}

func TestLoadMissingFileReturnsDefaultsSilently(t *testing.T) {
	got, warnings := Load(t.TempDir())
	if got != Defaults() {
		t.Errorf("Load of a missing file = %+v, want the defaults", got)
	}
	// A first run is not a problem, so it must not read like one in the log.
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

func TestLoadDefaultsMatchTheDocumentedContract(t *testing.T) {
	// The frontend hard-codes these for the first paint of a pane; if they drift
	// apart, a terminal is restyled the moment its settings arrive.
	want := Settings{
		FontFamily:   "'MonaspiceNe NFM', 'SF Mono', 'Menlo', monospace",
		FontSize:     12,
		LineHeight:   1.2,
		CursorStyle:  "block",
		CursorBlink:  true,
		Scrollback:   10000,
		Shell:        "",
		OptionIsMeta: true,
		Bell:         "sound",
		CopyOnSelect: true,
		ConfirmQuit:  "running",
	}
	if got := Defaults(); got != want {
		t.Errorf("Defaults() = %+v, want %+v", got, want)
	}
}

func TestLoadFullFile(t *testing.T) {
	dir := writeConfig(t, `{
	  "fontFamily": "Menlo, monospace",
	  "fontSize": 15,
	  "lineHeight": 1.4,
	  "cursorStyle": "bar",
	  "cursorBlink": false,
	  "scrollback": 500,
	  "shell": "/bin/bash",
	  "optionIsMeta": false,
	  "bell": "both",
	  "copyOnSelect": false,
	  "confirmQuit": "never"
	}`)
	got, warnings := Load(dir)
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	want := Settings{
		FontFamily: "Menlo, monospace", FontSize: 15, LineHeight: 1.4,
		CursorStyle: "bar", CursorBlink: false, Scrollback: 500,
		Shell: "/bin/bash", OptionIsMeta: false, Bell: "both",
		CopyOnSelect: false, ConfirmQuit: "never",
	}
	if got != want {
		t.Errorf("Load = %+v, want %+v", got, want)
	}
}

// The three default-true booleans are the reason Load decodes key by key: a
// whole-struct decode cannot tell "absent" from "false".
func TestLoadAbsentBooleansKeepTheirTrueDefaults(t *testing.T) {
	dir := writeConfig(t, `{"fontSize": 14}`)
	got, warnings := Load(dir)
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if !got.CursorBlink || !got.OptionIsMeta || !got.CopyOnSelect {
		t.Errorf("absent booleans were not defaulted to true: %+v", got)
	}
	if got.FontSize != 14 {
		t.Errorf("FontSize = %v, want 14", got.FontSize)
	}
}

func TestLoadExplicitFalseBooleansAreHonoured(t *testing.T) {
	dir := writeConfig(t, `{"cursorBlink": false, "optionIsMeta": false, "copyOnSelect": false}`)
	got, warnings := Load(dir)
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if got.CursorBlink || got.OptionIsMeta || got.CopyOnSelect {
		t.Errorf("explicit false was overridden: %+v", got)
	}
}

func TestLoadIgnoresUnknownKeys(t *testing.T) {
	dir := writeConfig(t, `{"fontSize": 20, "somethingFromTheFuture": {"a": [1,2]}, "_note": "hi"}`)
	got, warnings := Load(dir)
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if got.FontSize != 20 {
		t.Errorf("FontSize = %v, want 20", got.FontSize)
	}
}

func TestLoadMalformedJSONKeepsTheFileAndFallsBack(t *testing.T) {
	const broken = "{\n  \"fontSize\": 14,\n}\n" // trailing comma
	dir := writeConfig(t, broken)

	got, warnings := Load(dir)
	if got != Defaults() {
		t.Errorf("Load of a malformed file = %+v, want the defaults", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "does not parse") {
		t.Errorf("warnings = %v, want one about the file not parsing", warnings)
	}

	// The user's text is the whole point: a broken file must survive being read.
	after, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(after) != broken {
		t.Errorf("the malformed file was rewritten:\n%q", after)
	}
}

func TestLoadUnreadableFileFallsBack(t *testing.T) {
	dir := t.TempDir()
	// A directory where the file should be: open(2) succeeds, read(2) does not.
	if err := os.Mkdir(Path(dir), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got, warnings := Load(dir)
	if got != Defaults() {
		t.Errorf("Load = %+v, want the defaults", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "cannot be read") {
		t.Errorf("warnings = %v, want one about the file being unreadable", warnings)
	}
}

// The validation table. Each case supplies one key and states what Load should
// make of it, and whether the user is told.
func TestLoadValidation(t *testing.T) {
	def := Defaults()
	cases := []struct {
		name     string
		body     string
		want     func(Settings) bool
		wantWarn string // substring the warning must contain; "" means no warning
	}{
		// fontSize: number, 6–72, clamped.
		{"fontSize in range", `{"fontSize": 18}`, func(s Settings) bool { return s.FontSize == 18 }, ""},
		{"fontSize fractional", `{"fontSize": 13.5}`, func(s Settings) bool { return s.FontSize == 13.5 }, ""},
		{"fontSize at min", `{"fontSize": 6}`, func(s Settings) bool { return s.FontSize == 6 }, ""},
		{"fontSize at max", `{"fontSize": 72}`, func(s Settings) bool { return s.FontSize == 72 }, ""},
		{"fontSize below min", `{"fontSize": 2}`, func(s Settings) bool { return s.FontSize == 6 }, "fontSize"},
		{"fontSize above max", `{"fontSize": 400}`, func(s Settings) bool { return s.FontSize == 72 }, "fontSize"},
		{"fontSize negative", `{"fontSize": -1}`, func(s Settings) bool { return s.FontSize == 6 }, "fontSize"},
		{"fontSize wrong type", `{"fontSize": "big"}`, func(s Settings) bool { return s.FontSize == def.FontSize }, "fontSize"},
		{"fontSize null", `{"fontSize": null}`, func(s Settings) bool { return s.FontSize == def.FontSize }, ""},

		// lineHeight: number, 1.0–2.0, clamped.
		{"lineHeight in range", `{"lineHeight": 1.6}`, func(s Settings) bool { return s.LineHeight == 1.6 }, ""},
		{"lineHeight below min", `{"lineHeight": 0.5}`, func(s Settings) bool { return s.LineHeight == 1.0 }, "lineHeight"},
		{"lineHeight above max", `{"lineHeight": 3}`, func(s Settings) bool { return s.LineHeight == 2.0 }, "lineHeight"},
		{"lineHeight wrong type", `{"lineHeight": true}`, func(s Settings) bool { return s.LineHeight == def.LineHeight }, "lineHeight"},

		// cursorStyle: closed set, case-insensitive.
		{"cursorStyle underline", `{"cursorStyle": "underline"}`, func(s Settings) bool { return s.CursorStyle == "underline" }, ""},
		{"cursorStyle bar", `{"cursorStyle": "bar"}`, func(s Settings) bool { return s.CursorStyle == "bar" }, ""},
		{"cursorStyle mixed case", `{"cursorStyle": " Block "}`, func(s Settings) bool { return s.CursorStyle == "block" }, ""},
		{"cursorStyle unknown", `{"cursorStyle": "beam"}`, func(s Settings) bool { return s.CursorStyle == def.CursorStyle }, "cursorStyle"},
		{"cursorStyle wrong type", `{"cursorStyle": 3}`, func(s Settings) bool { return s.CursorStyle == def.CursorStyle }, "cursorStyle"},

		// scrollback: integer, 0–1,000,000, clamped.
		{"scrollback zero", `{"scrollback": 0}`, func(s Settings) bool { return s.Scrollback == 0 }, ""},
		{"scrollback at max", `{"scrollback": 1000000}`, func(s Settings) bool { return s.Scrollback == 1000000 }, ""},
		{"scrollback negative", `{"scrollback": -5}`, func(s Settings) bool { return s.Scrollback == 0 }, "scrollback"},
		{"scrollback huge", `{"scrollback": 99999999}`, func(s Settings) bool { return s.Scrollback == 1000000 }, "scrollback"},
		{"scrollback fractional", `{"scrollback": 10.5}`, func(s Settings) bool { return s.Scrollback == def.Scrollback }, "scrollback"},
		{"scrollback wrong type", `{"scrollback": "lots"}`, func(s Settings) bool { return s.Scrollback == def.Scrollback }, "scrollback"},

		// shell: "" or an absolute path. Whether it is executable is
		// ResolveShell's business, not Load's.
		{"shell empty", `{"shell": ""}`, func(s Settings) bool { return s.Shell == "" }, ""},
		{"shell absolute", `{"shell": "/opt/homebrew/bin/fish"}`, func(s Settings) bool { return s.Shell == "/opt/homebrew/bin/fish" }, ""},
		{"shell cleaned", `{"shell": "/bin/./zsh"}`, func(s Settings) bool { return s.Shell == "/bin/zsh" }, ""},
		{"shell relative", `{"shell": "bin/zsh"}`, func(s Settings) bool { return s.Shell == "" }, "shell"},
		{"shell tilde", `{"shell": "~/bin/zsh"}`, func(s Settings) bool { return s.Shell == "" }, "shell"},
		{"shell with a newline", "{\"shell\": \"/bin/zsh\\nrm -rf /\"}", func(s Settings) bool { return s.Shell == "" }, "shell"},
		{"shell wrong type", `{"shell": ["/bin/zsh"]}`, func(s Settings) bool { return s.Shell == "" }, "shell"},

		// bell: closed set.
		{"bell none", `{"bell": "none"}`, func(s Settings) bool { return s.Bell == "none" }, ""},
		{"bell visual", `{"bell": "visual"}`, func(s Settings) bool { return s.Bell == "visual" }, ""},
		{"bell both", `{"bell": "both"}`, func(s Settings) bool { return s.Bell == "both" }, ""},
		{"bell unknown", `{"bell": "klaxon"}`, func(s Settings) bool { return s.Bell == def.Bell }, "bell"},

		// confirmQuit: closed set.
		{"confirmQuit always", `{"confirmQuit": "always"}`, func(s Settings) bool { return s.ConfirmQuit == "always" }, ""},
		{"confirmQuit never", `{"confirmQuit": "never"}`, func(s Settings) bool { return s.ConfirmQuit == "never" }, ""},
		{"confirmQuit unknown", `{"confirmQuit": "maybe"}`, func(s Settings) bool { return s.ConfirmQuit == def.ConfirmQuit }, "confirmQuit"},

		// fontFamily: non-empty after sanitising.
		{"fontFamily set", `{"fontFamily": "Fira Code, monospace"}`, func(s Settings) bool { return s.FontFamily == "Fira Code, monospace" }, ""},
		{"fontFamily blank", `{"fontFamily": "   "}`, func(s Settings) bool { return s.FontFamily == def.FontFamily }, "fontFamily"},
		{"fontFamily control chars stripped", "{\"fontFamily\": \"Men\\u0000lo, mono\"}", func(s Settings) bool { return s.FontFamily == "Menlo, mono" }, ""},
		{"fontFamily wrong type", `{"fontFamily": 12}`, func(s Settings) bool { return s.FontFamily == def.FontFamily }, "fontFamily"},
		{
			"fontFamily css injection",
			`{"fontFamily": "monospace; } .xterm-rows { display:none } x{"}`,
			func(s Settings) bool { return !strings.ContainsAny(s.FontFamily, fontFamilyForbidden) },
			"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warnings := Load(writeConfig(t, tc.body))
			if !tc.want(got) {
				t.Errorf("Load(%s) = %+v, which does not satisfy the expectation", tc.body, got)
			}
			switch {
			case tc.wantWarn == "" && len(warnings) != 0:
				t.Errorf("warnings = %v, want none", warnings)
			case tc.wantWarn != "" && !warnsAbout(warnings, tc.wantWarn):
				t.Errorf("warnings = %v, want one naming %q", warnings, tc.wantWarn)
			}
		})
	}
}

func TestLoadFontFamilyIsCapped(t *testing.T) {
	long := strings.Repeat("A", 4000)
	body, err := json.Marshal(map[string]string{"fontFamily": long})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, _ := Load(writeConfig(t, string(body)))
	if len(got.FontFamily) > maxFontFamilyBytes {
		t.Errorf("FontFamily is %d bytes, want at most %d", len(got.FontFamily), maxFontFamilyBytes)
	}
	if len(got.FontFamily) == 0 {
		t.Error("FontFamily was emptied instead of truncated")
	}
}

// The value is interpolated raw into a <style> element by xterm's DOM renderer
// (`font-family: ${fontFamily};`), so anything that can close that declaration
// writes CSS for the whole page. config.json is the user's own 0600 file, so
// this crosses no privilege boundary — but a sanitiser that leaves the exit
// open is not doing the job it claims to.
func TestSanitizeFontFamilyClosesTheCSSEscapes(t *testing.T) {
	// A real font stack has to survive byte for byte, or the sanitiser gets
	// worked around instead of used.
	const stack = "'MonaspiceNe NFM', 'SF Mono', Menlo, monospace"
	if got := sanitizeFontFamily(stack); got != stack {
		t.Errorf("sanitizeFontFamily(%q) = %q, want it unchanged", stack, got)
	}

	for _, payload := range []string{
		`monospace; } .xterm-rows { display:none } x{`,
		`monospace; color: red`,
		// CSS escapes: "\3b" is a semicolon, so a value that keeps the backslash
		// keeps a way to spell every character above.
		`monospace\3b color:red`,
		// The declaration lands inside a <style> element, which "</style>" ends
		// whatever the CSS parser makes of the rest.
		`monospace</style><script>alert(1)</script>`,
	} {
		got := sanitizeFontFamily(payload)
		if strings.ContainsAny(got, fontFamilyForbidden) {
			t.Errorf("sanitizeFontFamily(%q) = %q, which still contains one of %q",
				payload, got, fontFamilyForbidden)
		}
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Settings{
		FontFamily: "Iosevka, monospace", FontSize: 16.5, LineHeight: 1.75,
		CursorStyle: "underline", CursorBlink: false, Scrollback: 250000,
		Shell: "/opt/homebrew/bin/fish", OptionIsMeta: false, Bell: "visual",
		CopyOnSelect: false, ConfirmQuit: "always",
	}
	if err := Save(dir, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, warnings := Load(dir)
	if len(warnings) != 0 {
		t.Errorf("a file we wrote ourselves produced warnings: %v", warnings)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestSaveDefaultsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Defaults()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, warnings := Load(dir)
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if got != Defaults() {
		t.Errorf("round trip = %+v, want the defaults", got)
	}
}

func TestSaveWritesAnEditableOwnerOnlyFile(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Defaults()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != filePerm {
		t.Errorf("mode = %#o, want %#o", got, filePerm)
	}

	data, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// It exists to be opened in an editor: indented, one key per line, newline
	// terminated.
	if !strings.Contains(string(data), "\n  \"fontSize\": 12,") {
		t.Errorf("not pretty-printed:\n%s", data)
	}
	if !strings.HasSuffix(string(data), "}\n") {
		t.Errorf("no trailing newline:\n%q", data)
	}
	// Every documented key has to be present, or "open the file and edit it"
	// is not a usable instruction.
	for _, key := range []string{
		"fontFamily", "fontSize", "lineHeight", "cursorStyle", "cursorBlink",
		"scrollback", "shell", "optionIsMeta", "bell", "copyOnSelect", "confirmQuit",
	} {
		if !strings.Contains(string(data), `"`+key+`"`) {
			t.Errorf("key %q is missing from the written file", key)
		}
	}
}

func TestResolveShell(t *testing.T) {
	dir := t.TempDir()

	exe := filepath.Join(dir, "myshell")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	notExec := filepath.Join(dir, "notexec")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	subdir := filepath.Join(dir, "adirectory")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(dir, "link-to-shell")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	const fallback = "/bin/zsh"
	cases := []struct {
		name       string
		configured string
		wantShell  string
		wantWarn   bool
	}{
		{"empty uses the fallback", "", fallback, false},
		{"blank uses the fallback", "   ", fallback, false},
		{"an executable regular file is used", exe, exe, false},
		{"a symlink to one is followed", link, link, false},
		{"a relative path is rejected", "myshell", fallback, true},
		{"a missing path is rejected", filepath.Join(dir, "nope"), fallback, true},
		{"a directory is rejected", subdir, fallback, true},
		{"a non-executable file is rejected", notExec, fallback, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shell, warning := ResolveShell(tc.configured, fallback)
			if shell != tc.wantShell {
				t.Errorf("shell = %q, want %q", shell, tc.wantShell)
			}
			if got := warning != ""; got != tc.wantWarn {
				t.Errorf("warning = %q, wantWarn = %v", warning, tc.wantWarn)
			}
		})
	}
}
