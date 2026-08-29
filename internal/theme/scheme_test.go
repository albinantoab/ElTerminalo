package theme

import (
	"strings"
	"testing"
)

// A real-shaped .itermcolors. Trimmed to the keys that matter plus the ones we
// have to prove are ignored: Alpha Component, Color Space, and a whole key
// ("Badge Color") that has no Theme field at all. The components are the ones
// Apple's plist writer produces — long decimals, no leading zero suppression —
// so the rounding is exercised on the values a real file carries.
const itermFixture = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Ansi 0 Color</key>
	<dict>
		<key>Alpha Component</key>
		<real>1</real>
		<key>Blue Component</key>
		<real>0.0</real>
		<key>Color Space</key>
		<string>sRGB</string>
		<key>Green Component</key>
		<real>0.0</real>
		<key>Red Component</key>
		<real>0.0</real>
	</dict>
	<key>Ansi 1 Color</key>
	<dict>
		<key>Blue Component</key>
		<real>0.0</real>
		<key>Green Component</key>
		<real>0.0</real>
		<key>Red Component</key>
		<real>1.0</real>
	</dict>
	<key>Ansi 4 Color</key>
	<dict>
		<key>Blue Component</key>
		<real>1.0</real>
		<key>Green Component</key>
		<real>0.40000000596046448</real>
		<key>Red Component</key>
		<real>0.20000000298023224</real>
	</dict>
	<key>Ansi 15 Color</key>
	<dict>
		<key>Blue Component</key>
		<real>1</real>
		<key>Green Component</key>
		<real>1</real>
		<key>Red Component</key>
		<real>1</real>
	</dict>
	<key>Background Color</key>
	<dict>
		<key>Alpha Component</key>
		<real>1</real>
		<key>Blue Component</key>
		<real>0.13333334028720856</real>
		<key>Color Space</key>
		<string>sRGB</string>
		<key>Green Component</key>
		<real>0.066666670143604279</real>
		<key>Red Component</key>
		<real>0.0</real>
	</dict>
	<key>Badge Color</key>
	<dict>
		<key>Alpha Component</key>
		<real>0.5</real>
		<key>Blue Component</key>
		<real>0.0</real>
		<key>Green Component</key>
		<real>0.1491314172744751</real>
		<key>Red Component</key>
		<real>1</real>
	</dict>
	<key>Cursor Color</key>
	<dict>
		<key>Blue Component</key>
		<real>0.5</real>
		<key>Green Component</key>
		<real>0.5</real>
		<key>Red Component</key>
		<real>0.5</real>
	</dict>
	<key>Foreground Color</key>
	<dict>
		<key>Blue Component</key>
		<real>0.80000001192092896</real>
		<key>Green Component</key>
		<real>0.80000001192092896</real>
		<key>Red Component</key>
		<real>0.80000001192092896</real>
	</dict>
	<key>Selection Color</key>
	<dict>
		<key>Blue Component</key>
		<real>0.40000000596046448</real>
		<key>Green Component</key>
		<real>0.20000000298023224</real>
		<key>Red Component</key>
		<real>0.10000000149011612</real>
	</dict>
</dict>
</plist>
`

// A Ghostty theme file, with the shapes real ones use: a comment, a blank line,
// palette entries with and without spaces around the '=', a bare rrggbb value,
// keys we recognise but have no field for, and a key we do not know at all.
const ghosttyFixture = `# Fixture Theme
# vim: ft=ghostty

palette = 0=#000000
palette = 1=#ff0000
palette=4=#3366ff
palette = 15=#ffffff
background = 001122
foreground = #cccccc
cursor-color = #808080
cursor-text = #000000
selection-background = #1a3366
selection-foreground = #ffffff
font-family = Some Font
`

func TestParseSchemeITermColors(t *testing.T) {
	got, err := ParseScheme("Fixture", []byte(itermFixture))
	if err != nil {
		t.Fatalf("ParseScheme: %v", err)
	}

	checks := map[string]struct{ got, want string }{
		"Name":        {got.Name, "Fixture"},
		"Background":  {got.Background, "#001122"},
		"Foreground":  {got.Foreground, "#cccccc"},
		"CursorColor": {got.CursorColor, "#808080"},
		"SelectionBg": {got.SelectionBg, "#1a3366"},
		"Black":       {got.Black, "#000000"},
		"Red":         {got.Red, "#ff0000"},
		"Blue":        {got.Blue, "#3366ff"},
		"BrightWhite": {got.BrightWhite, "#ffffff"},
	}
	for field, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %s, want %s", field, c.got, c.want)
		}
	}

	// Slots the file left out fall back to xterm's defaults, not to "".
	if got.Green != xtermDefaultANSI[2] {
		t.Errorf("Green = %s, want the xterm default %s", got.Green, xtermDefaultANSI[2])
	}
	if got.BrightBlack != xtermDefaultANSI[8] {
		t.Errorf("BrightBlack = %s, want the xterm default %s", got.BrightBlack, xtermDefaultANSI[8])
	}
}

func TestParseSchemeGhostty(t *testing.T) {
	got, err := ParseScheme("Fixture", []byte(ghosttyFixture))
	if err != nil {
		t.Fatalf("ParseScheme: %v", err)
	}

	checks := map[string]struct{ got, want string }{
		"Background":  {got.Background, "#001122"}, // bare rrggbb, no '#'
		"Foreground":  {got.Foreground, "#cccccc"},
		"CursorColor": {got.CursorColor, "#808080"},
		"SelectionBg": {got.SelectionBg, "#1a3366"},
		"Black":       {got.Black, "#000000"},
		"Red":         {got.Red, "#ff0000"},
		"Blue":        {got.Blue, "#3366ff"}, // written as palette=4=... with no spaces
		"BrightWhite": {got.BrightWhite, "#ffffff"},
	}
	for field, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %s, want %s", field, c.got, c.want)
		}
	}
	if got.Yellow != xtermDefaultANSI[3] {
		t.Errorf("Yellow = %s, want the xterm default %s", got.Yellow, xtermDefaultANSI[3])
	}
}

// The two fixtures describe the same scheme, so the same Theme has to come out
// of both — the whole point of having one ParseScheme rather than two importers.
func TestParseSchemeFormatsAgree(t *testing.T) {
	fromITerm, err := ParseScheme("Fixture", []byte(itermFixture))
	if err != nil {
		t.Fatalf("iTerm: %v", err)
	}
	fromGhostty, err := ParseScheme("Fixture", []byte(ghosttyFixture))
	if err != nil {
		t.Fatalf("Ghostty: %v", err)
	}
	if fromITerm != fromGhostty {
		t.Errorf("the two formats produced different themes:\n iTerm:   %+v\n Ghostty: %+v", fromITerm, fromGhostty)
	}
}

// The derived UI colours are the documented formulas, computed here by hand
// against the fixture's background (#001122) and blue (#3366ff).
func TestParseSchemeDerivedUIColors(t *testing.T) {
	got, err := ParseScheme("Fixture", []byte(ghosttyFixture))
	if err != nil {
		t.Fatalf("ParseScheme: %v", err)
	}

	checks := map[string]struct{ got, want string }{
		// Accent is the scheme's blue.
		"Accent":       {got.Accent, "#3366ff"},
		"BorderActive": {got.BorderActive, "#3366ff"},
		"StatusFg":     {got.StatusFg, "#3366ff"},
		// AccentDim is the blue with a quarter of its brightness removed:
		// 0x33*0.75 = 38.25 -> 38, 0x66*0.75 = 76.5 -> 77 (round half up),
		// 0xff*0.75 = 191.25 -> 191.
		"AccentDim": {got.AccentDim, "#264dbf"},
		// Border lifts the background 10% towards white: 0 + 255*0.1 = 25.5 -> 26
		// (0x1a), 17 + 238*0.1 = 40.8 -> 41 (0x29), 34 + 221*0.1 = 56.1 -> 56 (0x38).
		"Border": {got.Border, "#1a2938"},
		// StatusBg darkens the background 10%: 0, 0x11*0.9 = 15.3 -> 15 (0x0f),
		// 0x22*0.9 = 30.6 -> 31 (0x1f).
		"StatusBg": {got.StatusBg, "#000f1f"},
	}
	for field, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %s, want %s", field, c.got, c.want)
		}
	}
}

func TestParseSchemeFillsMissingCursorAndSelection(t *testing.T) {
	got, err := ParseScheme("Bare", []byte("background = #000000\nforeground = #ffffff\n"))
	if err != nil {
		t.Fatalf("ParseScheme: %v", err)
	}
	if got.CursorColor != "#ffffff" {
		t.Errorf("CursorColor = %s, want the foreground #ffffff", got.CursorColor)
	}
	// Selection is the background lifted 20% towards white: 0 + 51 = 51 (0x33).
	if got.SelectionBg != "#333333" {
		t.Errorf("SelectionBg = %s, want #333333", got.SelectionBg)
	}
}

func TestParseSchemeErrors(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string // substring the message must carry
	}{
		{"empty", "", "Ghostty"},
		{"no colours at all", "font-size = 14\n# nothing here\n", "Ghostty"},
		{"ghostty without a background", "foreground = #ffffff\n", "background and foreground"},
		{"ghostty without a foreground", "background = #000000\n", "background and foreground"},
		{"plist that is not a colour scheme", "<?xml version=\"1.0\"?>\n<plist><dict><key>a</key><string>b</string></dict></plist>", "iTerm2"},
		{"truncated plist", "<plist><dict><key>Background Color</key><dict><key>Red Component</key><real>1", "iTerm2"},
		{"garbage that opens like XML", "<not xml at all", "iTerm2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseScheme("X", []byte(tc.data))
			if err == nil {
				t.Fatalf("ParseScheme(%q) succeeded, want an error", tc.data)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// An .itermcolors whose extension is missing, or a Ghostty theme with no
// extension at all, still has to end up with a usable name.
func TestSchemeName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/tmp/Solarized Dark.itermcolors", "Solarized Dark"},
		{"Dracula.itermcolors", "Dracula"},
		{"/themes/catppuccin-mocha", "catppuccin-mocha"},
		{"a.b.itermcolors", "a.b"},
		{".itermcolors", "Imported Scheme"}, // all extension, no stem
		{"   ", "Imported Scheme"},
		{"Solarized   Dark.itermcolors", "Solarized Dark"}, // runs of spaces folded
		{"Ember‮Dark.itermcolors", "EmberDark"},            // right-to-left override stripped
		{"tab\there.itermcolors", "tab here"},
		{strings.Repeat("x", 200) + ".itermcolors", strings.Repeat("x", maxSchemeNameBytes)},
	}
	for _, tc := range cases {
		if got := SchemeName(tc.in); got != tc.want {
			t.Errorf("SchemeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseHexColor(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"#1d1f21", "#1d1f21", true},
		{"1d1f21", "#1d1f21", true},
		{"#ABCDEF", "#abcdef", true},
		{"#abc", "#aabbcc", true},
		{`"#1d1f21"`, "#1d1f21", true},
		{"  #1d1f21  ", "#1d1f21", true},
		{"", "", false},
		{"#12345", "", false},
		{"#gggggg", "", false},
		{"rgb(1,2,3)", "", false},
	}
	for _, tc := range cases {
		got, ok := parseHexColor(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseHexColor(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestDarkenAndLightenSaturate(t *testing.T) {
	if got := darken("#ffffff", 1.0); got != "#000000" {
		t.Errorf("darken to nothing = %s, want #000000", got)
	}
	if got := lighten("#000000", 1.0); got != "#ffffff" {
		t.Errorf("lighten to white = %s, want #ffffff", got)
	}
	if got := darken("#000000", 0.5); got != "#000000" {
		t.Errorf("darkening black = %s, want #000000", got)
	}
	if got := lighten("#ffffff", 0.5); got != "#ffffff" {
		t.Errorf("lightening white = %s, want #ffffff", got)
	}
}

func TestUpsertReplacesByNameAndAppendsOtherwise(t *testing.T) {
	dir := t.TempDir()

	first := Theme{Name: "Imported", Background: "#000000", Foreground: "#ffffff"}
	if err := Upsert(dir, first); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	other := Theme{Name: "Another", Background: "#111111", Foreground: "#eeeeee"}
	if err := Upsert(dir, other); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Same name, different case: replaces rather than duplicating, because
	// Merged resolves names case-insensitively too.
	replacement := Theme{Name: "imported", Background: "#222222", Foreground: "#dddddd"}
	if err := Upsert(dir, replacement); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := LoadUserThemes(dir)
	if err != nil {
		t.Fatalf("LoadUserThemes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("themes = %d, want 2: %+v", len(got), got)
	}
	if got[0] != replacement {
		t.Errorf("themes[0] = %+v, want %+v", got[0], replacement)
	}
	if got[1] != other {
		t.Errorf("themes[1] = %+v, want %+v", got[1], other)
	}
}
