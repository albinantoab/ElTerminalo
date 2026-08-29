package theme

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The two formats worth importing. Both are plain text and both are what the
// theme galleries people actually link to hand out, so ParseScheme sniffs
// between them rather than asking the user which one they picked.
const (
	// iTerm2's .itermcolors: an XML property list.
	formatITerm = "iTerm2 .itermcolors"
	// Ghostty's theme files: `key = value` lines.
	formatGhostty = "Ghostty theme"
)

// maxSchemeNameBytes bounds the name derived from a filename. It ends up in
// themes.json, in the palette, and in a <select>; a 4 KB filename must not
// become a 4 KB menu entry.
const maxSchemeNameBytes = 64

// fallbackSchemeName is used when a file's name sanitises down to nothing —
// e.g. it was called ".itermcolors" with no stem at all.
const fallbackSchemeName = "Imported Scheme"

// derived colour adjustments. The scheme formats carry a terminal palette and
// nothing else, but a Theme also drives the app's own chrome, so the six UI
// colours are computed from the palette. Each factor is stated once, here,
// because "the border looks wrong" is a complaint about this number.
const (
	// accentDimDarken makes the pressed/secondary accent by taking a quarter of
	// the blue's brightness away.
	accentDimDarken = 0.25
	// borderLighten lifts the background just far enough to separate two panes
	// against it without drawing a visible line.
	borderLighten = 0.10
	// statusBgDarken pushes the status bar a shade below the terminal so it
	// reads as chrome rather than as more terminal.
	statusBgDarken = 0.10
	// selectionLighten builds a selection colour for a scheme that shipped
	// without one.
	selectionLighten = 0.20
)

// xtermDefaultANSI fills any of the 16 slots a scheme leaves out. These are
// xterm's own defaults, so a partial scheme degrades to "the colour every other
// terminal would have used" rather than to black-on-black.
var xtermDefaultANSI = [16]string{
	"#000000", "#cd0000", "#00cd00", "#cdcd00",
	"#0000ee", "#cd00cd", "#00cdcd", "#e5e5e5",
	"#7f7f7f", "#ff0000", "#00ff00", "#ffff00",
	"#5c5cff", "#ff00ff", "#00ffff", "#ffffff",
}

// SchemeName turns an imported file's name into a theme name: the basename
// without its extension, stripped of anything that has no business in a menu
// entry and bounded in length.
func SchemeName(filename string) string {
	base := filepath.Base(filename)
	base = strings.TrimSuffix(base, filepath.Ext(base))

	// Control characters, the format class and the exotic line separators go
	// for the same reason they go from a log line: they are invisible or they
	// reverse the rendering of everything after them, and a downloaded theme
	// file's name is not something we choose.
	var b strings.Builder
	b.Grow(len(base))
	for _, r := range base {
		// Whitespace is tested first, and deliberately: a tab is both a space
		// and a control character, and turning it into a separator keeps
		// "tab\there" two words instead of welding it into one.
		if unicode.IsSpace(r) {
			b.WriteRune(' ')
			continue
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			continue
		}
		b.WriteRune(r)
	}
	name := strings.Join(strings.Fields(b.String()), " ")

	if len(name) > maxSchemeNameBytes {
		cut := maxSchemeNameBytes
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = strings.TrimSpace(name[:cut])
	}
	if name == "" {
		return fallbackSchemeName
	}
	return name
}

// ParseScheme converts an exported colour scheme into a Theme called name.
//
// It sniffs the format: a document that starts with '<' is an iTerm2
// .itermcolors property list, anything else is read as a Ghostty theme. Those
// are the only two shapes either format can take — a plist is XML and a Ghostty
// theme is `key = value` lines — so the check is both cheap and total.
//
// The error is written to be shown to the user: it names the format we thought
// we were reading and what was missing or malformed about it.
func ParseScheme(name string, data []byte) (Theme, error) {
	var (
		colors schemeColors
		err    error
	)
	format := formatGhostty
	// A UTF-8 BOM ahead of the declaration is common enough in files that have
	// been through a Windows editor, and it would otherwise hide the '<'.
	head := bytes.TrimSpace(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	if bytes.HasPrefix(head, []byte("<")) {
		format = formatITerm
		colors, err = parseITermColors(data)
	} else {
		colors, err = parseGhosttyTheme(data)
	}
	if err != nil {
		return Theme{}, fmt.Errorf("%s: %w", format, err)
	}
	if colors.Background == "" || colors.Foreground == "" {
		return Theme{}, fmt.Errorf("%s: no background and foreground colours found; is this a colour scheme?", format)
	}
	return colors.toTheme(name), nil
}

// schemeColors is what both parsers produce: the terminal palette, with ""
// standing for a colour the file did not carry.
type schemeColors struct {
	ANSI        [16]string
	Background  string
	Foreground  string
	Cursor      string
	SelectionBg string
}

// toTheme maps the palette onto a Theme, filling the gaps and deriving the six
// colours the app's own chrome needs.
func (c schemeColors) toTheme(name string) Theme {
	ansi := c.ANSI
	for i, v := range ansi {
		if v == "" {
			ansi[i] = xtermDefaultANSI[i]
		}
	}

	cursor := c.Cursor
	if cursor == "" {
		// A cursor the colour of the text is what a terminal does when it is
		// told nothing else.
		cursor = c.Foreground
	}
	selection := c.SelectionBg
	if selection == "" {
		selection = lighten(c.Background, selectionLighten)
	}

	// The accent is the scheme's blue. It is the one slot that is a
	// recognisable "UI colour" in every palette — reds mean errors and greens
	// mean success, so neither can carry the app's chrome — and using a colour
	// from the palette rather than a fixed one is what makes an imported scheme
	// look like a theme instead of like a repainted terminal inside our window.
	accent := ansi[4]

	return Theme{
		Name:       name,
		Background: c.Background,
		Foreground: c.Foreground,

		Accent:    accent,
		AccentDim: darken(accent, accentDimDarken),
		// Lifted off the background, not off the foreground: a border has to
		// separate two panes without being a line the eye lands on.
		Border:       lighten(c.Background, borderLighten),
		BorderActive: accent,
		StatusBg:     darken(c.Background, statusBgDarken),
		StatusFg:     accent,

		CursorColor: cursor,
		SelectionBg: selection,

		Black: ansi[0], Red: ansi[1], Green: ansi[2], Yellow: ansi[3],
		Blue: ansi[4], Magenta: ansi[5], Cyan: ansi[6], White: ansi[7],
		BrightBlack: ansi[8], BrightRed: ansi[9], BrightGreen: ansi[10],
		BrightYellow: ansi[11], BrightBlue: ansi[12], BrightMagenta: ansi[13],
		BrightCyan: ansi[14], BrightWhite: ansi[15],
	}
}

// --- iTerm2 .itermcolors -----------------------------------------------------

// iTerm2 key names. "Ansi N Color" is a component dict per palette slot; the
// four named ones are the same shape. Files also carry "Alpha Component" and
// "Color Space" inside each dict, and a handful of keys we have no field for
// ("Badge Color", "Link Color", …); all of those are ignored.
const (
	itermAnsiPrefix = "Ansi "
	itermAnsiSuffix = " Color"
)

func parseITermColors(data []byte) (schemeColors, error) {
	root, err := parsePlistRootDict(data)
	if err != nil {
		return schemeColors{}, err
	}

	var out schemeColors
	for i := range out.ANSI {
		key := fmt.Sprintf("%s%d%s", itermAnsiPrefix, i, itermAnsiSuffix)
		if hex, ok := itermColor(root[key]); ok {
			out.ANSI[i] = hex
		}
	}
	out.Background, _ = itermColor(root["Background Color"])
	out.Foreground, _ = itermColor(root["Foreground Color"])
	out.Cursor, _ = itermColor(root["Cursor Color"])
	out.SelectionBg, _ = itermColor(root["Selection Color"])

	return out, nil
}

// itermColor turns one component dict into #rrggbb. Components are reals in
// 0–1; anything outside that (a P3 file can carry a slightly negative value
// once converted) is clamped rather than rejected.
func itermColor(v plistValue) (string, bool) {
	if v.dict == nil {
		return "", false
	}
	r, rok := v.dict["Red Component"].number()
	g, gok := v.dict["Green Component"].number()
	b, bok := v.dict["Blue Component"].number()
	if !rok || !gok || !bok {
		return "", false
	}
	return hexOf(component(r), component(g), component(b)), true
}

// component scales a 0–1 plist real onto 0–255.
func component(v float64) int {
	return clampByte(int(math.Round(v * 255)))
}

// plistValue is the subset of a property list this package understands: nested
// dictionaries and numbers. Strings, arrays, data and booleans are skipped
// wholesale — nothing in a colour scheme needs them.
type plistValue struct {
	dict  map[string]plistValue
	num   float64
	isNum bool
}

func (v plistValue) number() (float64, bool) { return v.num, v.isNum }

// parsePlistRootDict returns the top-level <dict> of a property list.
//
// Hand-rolled over encoding/xml rather than pulled in as a dependency: this is
// a couple of dozen lines for the two node types that matter, and encoding/xml
// will not resolve the external DTD every .itermcolors file declares, which is
// the property a plist parser most needs here.
func parsePlistRootDict(data []byte) (map[string]plistValue, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil, fmt.Errorf("no <dict> found in the property list")
		}
		if err != nil {
			return nil, fmt.Errorf("malformed XML: %w", err)
		}
		// The first <dict> in document order is the plist's root value. Taking
		// it directly means the <plist> wrapper, the XML declaration and the
		// DOCTYPE all need no special handling.
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "dict" {
			return parsePlistDict(dec)
		}
	}
}

// parsePlistDict consumes the body of a <dict> whose start element has already
// been read, up to and including its </dict>.
func parsePlistDict(dec *xml.Decoder) (map[string]plistValue, error) {
	out := make(map[string]plistValue)
	var pendingKey string
	var havePendingKey bool

	for {
		tok, err := dec.Token()
		if err != nil {
			// EOF included: a dict that never closes is a truncated file.
			return nil, fmt.Errorf("unterminated <dict>: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "key" {
				text, err := plistText(dec)
				if err != nil {
					return nil, err
				}
				pendingKey, havePendingKey = text, true
				continue
			}
			value, err := parsePlistValue(dec, t)
			if err != nil {
				return nil, err
			}
			// A value with no key before it is a malformed plist; dropping it
			// is friendlier than refusing the whole file over a stray node.
			if havePendingKey {
				out[pendingKey] = value
				havePendingKey = false
			}
		case xml.EndElement:
			if t.Name.Local == "dict" {
				return out, nil
			}
		}
	}
}

// parsePlistValue reads the element start has just opened, and everything up to
// its matching end tag.
func parsePlistValue(dec *xml.Decoder, start xml.StartElement) (plistValue, error) {
	switch start.Name.Local {
	case "dict":
		d, err := parsePlistDict(dec)
		return plistValue{dict: d}, err
	case "real", "integer":
		text, err := plistText(dec)
		if err != nil {
			return plistValue{}, err
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			// Not fatal: the file may carry a number we do not need. The key
			// simply has no usable value, and the caller falls back.
			return plistValue{}, nil
		}
		return plistValue{num: f, isNum: true}, nil
	default:
		// string, data, array, true, false — skipped whole, children included.
		if err := dec.Skip(); err != nil {
			return plistValue{}, fmt.Errorf("malformed <%s>: %w", start.Name.Local, err)
		}
		return plistValue{}, nil
	}
}

// plistText returns the character data of the element that was just opened,
// consuming its end tag.
func plistText(dec *xml.Decoder) (string, error) {
	var b strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("unterminated element: %w", err)
		}
		switch t := tok.(type) {
		case xml.CharData:
			b.Write(t)
		case xml.StartElement:
			// Not expected inside <key>/<real>, but a nested element must not
			// make the loop mistake the child's end tag for our own.
			if err := dec.Skip(); err != nil {
				return "", fmt.Errorf("malformed <%s>: %w", t.Name.Local, err)
			}
		case xml.EndElement:
			return b.String(), nil
		}
	}
}

// --- Ghostty themes ----------------------------------------------------------

func parseGhosttyTheme(data []byte) (schemeColors, error) {
	var out schemeColors

	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		// A comment is a line that *starts* with '#'. Trimming from the first
		// '#' anywhere would eat every colour in the file.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch key {
		case "palette":
			// `palette = N=#rrggbb`, where the index and the colour are
			// themselves separated by '='.
			idxText, colorText, ok := strings.Cut(value, "=")
			if !ok {
				continue
			}
			idx, err := strconv.Atoi(strings.TrimSpace(idxText))
			if err != nil || idx < 0 || idx > 15 {
				continue
			}
			if hex, ok := parseHexColor(colorText); ok {
				out.ANSI[idx] = hex
			}
		case "background":
			if hex, ok := parseHexColor(value); ok {
				out.Background = hex
			}
		case "foreground":
			if hex, ok := parseHexColor(value); ok {
				out.Foreground = hex
			}
		case "cursor-color":
			if hex, ok := parseHexColor(value); ok {
				out.Cursor = hex
			}
		case "selection-background":
			if hex, ok := parseHexColor(value); ok {
				out.SelectionBg = hex
			}
		case "cursor-text", "selection-foreground":
			// Recognised so they are not mistaken for junk, but a Theme has no
			// field for either: xterm.js derives both from the colour behind
			// them.
		default:
			// Ghostty theme files are configuration fragments and can carry
			// anything else the app understands. Ignored.
		}
	}

	return out, nil
}

// --- colour helpers ----------------------------------------------------------

// parseHexColor accepts #rrggbb, rrggbb, #rgb and rgb.
func parseHexColor(s string) (string, bool) {
	s = strings.TrimSpace(s)
	// Quotes come off before the '#', not after: some Ghostty themes write
	// "#1d1f21" with the quotes outside the hash.
	s = strings.Trim(s, `"'`)
	s = strings.TrimPrefix(s, "#")

	switch len(s) {
	case 3:
		// #abc means #aabbcc.
		var expanded strings.Builder
		for _, r := range s {
			expanded.WriteRune(r)
			expanded.WriteRune(r)
		}
		s = expanded.String()
	case 6:
	default:
		return "", false
	}

	n, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return "", false
	}
	return hexOf(int(n>>16&0xff), int(n>>8&0xff), int(n&0xff)), true
}

// splitHex takes a colour this package produced back apart. It only ever sees
// #rrggbb, so a failure means a caller passed something that was never a
// colour, and black is the safe answer.
func splitHex(s string) (r, g, b int) {
	rr, gg, bb, ok := 0, 0, 0, false
	if hex, valid := parseHexColor(s); valid {
		n, err := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
		if err == nil {
			rr, gg, bb, ok = int(n>>16&0xff), int(n>>8&0xff), int(n&0xff), true
		}
	}
	if !ok {
		return 0, 0, 0
	}
	return rr, gg, bb
}

func hexOf(r, g, b int) string {
	return fmt.Sprintf("#%02x%02x%02x", clampByte(r), clampByte(g), clampByte(b))
}

func clampByte(v int) int {
	switch {
	case v < 0:
		return 0
	case v > 255:
		return 255
	}
	return v
}

// darken scales every channel towards black by f (0–1).
func darken(color string, f float64) string {
	r, g, b := splitHex(color)
	scale := func(v int) int { return int(math.Round(float64(v) * (1 - f))) }
	return hexOf(scale(r), scale(g), scale(b))
}

// lighten moves every channel f of the way to white.
func lighten(color string, f float64) string {
	r, g, b := splitHex(color)
	lift := func(v int) int { return int(math.Round(float64(v) + (255-float64(v))*f)) }
	return hexOf(lift(r), lift(g), lift(b))
}
