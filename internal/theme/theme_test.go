package theme

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeThemes(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte(body), 0o600); err != nil {
		t.Fatalf("write themes.json: %v", err)
	}
	return dir
}

func names(themes []Theme) []string {
	out := make([]string, 0, len(themes))
	for _, t := range themes {
		out = append(out, t.Name)
	}
	return out
}

// The failure this closes: Upsert used to treat "themes.json does not parse" as
// "there are no themes", so the next import wrote a file containing only the
// imported theme. One stray comma cost the user every custom theme they had —
// the exact inverse of the settings package's never-clobber policy.
func TestUpsertRefusesToWriteOverAnUnparseableFile(t *testing.T) {
	const corrupt = `{"themes": [{"name": "Mine",}]}`
	dir := writeThemes(t, corrupt)

	err := Upsert(dir, Theme{Name: "Imported", Background: "#000000"})
	if err == nil {
		t.Fatal("Upsert accepted a themes.json that does not parse")
	}
	// The message has to name the file: it is the only instruction the user
	// gets, and the import dialog shows nothing else.
	if !strings.Contains(err.Error(), Path(dir)) {
		t.Errorf("error %q does not name %s", err, Path(dir))
	}

	after, readErr := os.ReadFile(Path(dir))
	if readErr != nil {
		t.Fatalf("read back: %v", readErr)
	}
	if !bytes.Equal(after, []byte(corrupt)) {
		t.Errorf("themes.json was rewritten:\n got %s\nwant %s", after, corrupt)
	}
}

// The happy path over a file Upsert wrote itself is covered by
// TestUpsertReplacesByNameAndAppendsOtherwise. This is the other half of the
// stricter policy: a themes.json that parses is still fully honoured, however
// the user chose to format it, and the themes it holds survive an import.
func TestUpsertKeepsTheThemesInAHandEditedFile(t *testing.T) {
	dir := writeThemes(t, "{\n\t\"themes\": [\n\t\t{\"name\": \"Mine\", \"background\": \"#111111\"},\n\t\t{\"name\": \"Other\", \"background\": \"#222222\"}\n\t]\n}\n")

	if err := Upsert(dir, Theme{Name: "Imported", Background: "#333333"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := LoadUserThemes(dir)
	if err != nil {
		t.Fatalf("LoadUserThemes: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("themes = %v, want Mine, Other, Imported", names(got))
	}
	if got[0].Background != "#111111" || got[1].Background != "#222222" {
		t.Errorf("the pre-existing themes changed: %v", got[:2])
	}
	if got[2].Name != "Imported" {
		t.Errorf("themes = %v, want the import appended last", names(got))
	}
}

// The file used to be written 0644 through a fixed <path>.tmp with no fsync.
// The mode is the visible half; the durable write is what keeps two saves from
// splicing into one another and a power loss from committing a rename over
// unwritten bytes.
func TestSaveUserThemesWritesAnOwnerOnlyFileAndNoLitter(t *testing.T) {
	dir := t.TempDir()
	if err := SaveUserThemes(dir, []Theme{{Name: "Mine", Background: "#111111"}}); err != nil {
		t.Fatalf("SaveUserThemes: %v", err)
	}

	info, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != filePerm {
		t.Errorf("mode = %#o, want %#o", got, filePerm)
	}

	// The scratch file is renamed into place, never left behind.
	entries, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(entries) != 1 || filepath.Base(entries[0]) != FileName {
		t.Errorf("the config directory holds %v, want only %s", entries, FileName)
	}

	data, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// It exists to be edited by hand: indented, newline terminated.
	if !strings.Contains(string(data), "\n      \"name\": \"Mine\",") {
		t.Errorf("not pretty-printed:\n%s", data)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Errorf("no trailing newline:\n%q", data)
	}
}

// Reading is the lenient half: a themes.json nobody can parse costs the user
// their custom themes in the picker until they fix it, but it must never take
// the built-ins with it and it must never write.
func TestMergedFallsBackToTheBuiltInsWhenTheFileIsCorrupt(t *testing.T) {
	const corrupt = `{"themes": [{`
	dir := writeThemes(t, corrupt)

	if got, want := len(Merged(dir)), len(All()); got != want {
		t.Errorf("Merged returned %d themes, want the %d built-ins", got, want)
	}
	after, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(after, []byte(corrupt)) {
		t.Errorf("Merged rewrote themes.json:\n got %s\nwant %s", after, corrupt)
	}
}
