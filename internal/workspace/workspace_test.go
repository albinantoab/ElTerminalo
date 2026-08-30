package workspace

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// silenceLog keeps the "skipping x" lines a test provokes on purpose out of the
// test output.
func silenceLog(t *testing.T) {
	t.Helper()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
}

// twoTabsFourPanes is a layout in the frontend's shape: one tab with a single
// pane, one tab split three ways.
const twoTabsFourPanes = `{
  "version": 2,
  "themeName": "Terminalo",
  "activeTabIndex": 0,
  "tabs": [
    {"name": "one", "layout": {"type": "leaf", "cwd": "/tmp"}},
    {"name": "two", "layout": {
      "type": "split", "direction": "vertical", "ratio": 0.5,
      "children": [
        {"type": "leaf", "cwd": "/a"},
        {"type": "split", "direction": "horizontal", "children": [
          {"type": "leaf", "cwd": "/b"},
          {"type": "leaf", "cwd": "/c"}
        ]}
      ]
    }}
  ]
}`

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()

	if err := Save(dir, "My Project", twoTabsFourPanes); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(dir, "my-project")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertSameJSON(t, got, twoTabsFourPanes)
}

// This package stores the frontend's format and must not reinterpret it: a
// struct with named fields here would drop every key the other side of the
// bridge adds later, and the layout would come back missing whatever this Go
// file had not heard of.
func TestSaveKeepsKeysThisPackageKnowsNothingAbout(t *testing.T) {
	dir := t.TempDir()

	state := `{"version":9,"tabs":[{"layout":{"type":"leaf"},"somethingNew":{"a":[1,2,{"b":null}]}}],` +
		`"futureKey":"kept","htmlish":"a<b>c&d"}`
	if err := Save(dir, "future", state); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(dir, "future")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertSameJSON(t, got, state)
}

// assertSameJSON compares two JSON documents by value. Save re-indents what it
// stores, so the bytes differ by whitespace; nothing else may.
func assertSameJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("what Load returned does not parse: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("the fixture does not parse: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("the layout changed on the round trip:\n got %s\nwant %s", got, want)
	}
}

func TestSaveWritesTheDisplayNameAndOwnerOnlyPermissions(t *testing.T) {
	dir := t.TempDir()

	if err := Save(dir, "  My Project  ", `{"tabs":[]}`); err != nil {
		t.Fatalf("Save: %v", err)
	}

	path := filepath.Join(Dir(dir), "my-project.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	// The layout names tabs and records every pane's working directory.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the workspace file is mode %#o, want 0600", perm)
	}
	if perm := dirPerm; perm != 0o700 {
		t.Fatalf("dirPerm is %#o, want 0700", perm)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s stored
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("the file we wrote does not parse: %v", err)
	}
	if s.Name != "My Project" {
		t.Fatalf("the file records the name %q, want the trimmed display name", s.Name)
	}
	if s.SavedUnix <= 0 {
		t.Fatalf("savedUnix is %d, want a real timestamp", s.SavedUnix)
	}
}

func TestSaveRefusesAnEmptyNameOrInvalidJSON(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{"", "   ", "\t\n"} {
		if err := Save(dir, name, `{}`); err == nil {
			t.Fatalf("Save accepted the name %q", name)
		}
	}

	for _, state := range []string{"", "   ", "{", `{"tabs":[},`, "not json at all"} {
		if err := Save(dir, "broken", state); err == nil {
			t.Fatalf("Save accepted %q as a layout", state)
		}
	}

	// And nothing was left behind by the refusals.
	if got := List(dir); len(got) != 0 {
		t.Fatalf("List found %d workspace(s) after only failed saves: %+v", len(got), got)
	}
}

func TestSaveUnderTheSameNameReplacesRatherThanNumbering(t *testing.T) {
	dir := t.TempDir()

	if err := Save(dir, "Notes", `{"tabs":[{"layout":{"type":"leaf"}}]}`); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Same name, different case and padding: still the same workspace.
	if err := Save(dir, " notes ", `{"tabs":[{"layout":{"type":"leaf"}},{"layout":{"type":"leaf"}}]}`); err != nil {
		t.Fatalf("Save again: %v", err)
	}

	got := List(dir)
	if len(got) != 1 {
		t.Fatalf("List found %d workspaces, want 1: %+v", len(got), got)
	}
	if got[0].Tabs != 2 {
		t.Fatalf("the second save did not replace the first: tabs=%d, want 2", got[0].Tabs)
	}
}

// The failure this covers: two different names that slug to the same string.
// Overwriting would destroy a layout the user never asked to replace.
func TestCollidingNamesGetNumberedSlugsAndBothSurvive(t *testing.T) {
	dir := t.TempDir()

	names := []string{"My Project", "my project!", "MY  PROJECT", "my/project"}
	for _, name := range names {
		if err := Save(dir, name, `{"tabs":[{"layout":{"type":"leaf"}}]}`); err != nil {
			t.Fatalf("Save(%q): %v", name, err)
		}
	}

	got := List(dir)
	if len(got) != len(names) {
		t.Fatalf("List found %d workspaces, want %d: %+v", len(got), len(names), got)
	}

	slugs := map[string]bool{}
	seen := map[string]bool{}
	for _, info := range got {
		if slugs[info.Slug] {
			t.Fatalf("two workspaces share the slug %q", info.Slug)
		}
		slugs[info.Slug] = true
		seen[info.Name] = true

		// Every slug List reports has to be loadable — that is the contract the
		// picker relies on.
		if _, err := Load(dir, info.Slug); err != nil {
			t.Fatalf("Load(%q): %v", info.Slug, err)
		}
	}
	for _, name := range names {
		if !seen[name] {
			t.Fatalf("List lost the workspace named %q: %+v", name, got)
		}
	}
	// The first one keeps the plain slug; the rest are numbered from 2.
	for _, want := range []string{"my-project", "my-project-2", "my-project-3", "my-project-4"} {
		if !slugs[want] {
			t.Fatalf("no workspace was stored as %q: %+v", want, got)
		}
	}
}

func TestSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Simple", "simple"},
		{"My Project", "my-project"},
		{"  padded  ", "padded"},
		{"lots   of    space", "lots-of-space"},
		{"punctuation!?#$%^&*()", "punctuation"},
		{"a/b/c", "a-b-c"},
		{"../../etc/passwd", "etc-passwd"},
		{"already-a-slug-2", "already-a-slug-2"},
		{"Trailing---", "trailing"},
		{"---Leading", "leading"},
		{"Ünïcødé", "n-c-d"},
		{"日本語", fallbackSlug},
		{"", fallbackSlug},
		{"!!!", fallbackSlug},
		{"emoji 🚀 name", "emoji-name"},
		{"CamelCase123", "camelcase123"},
	}
	for _, c := range cases {
		if got := Slug(c.in); got != c.want {
			t.Errorf("Slug(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// Idempotence is what lets the App bindings take either a name or a slug.
	for _, c := range cases {
		once := Slug(c.in)
		if twice := Slug(once); twice != once {
			t.Errorf("Slug is not idempotent: Slug(%q) = %q, then %q", c.in, once, twice)
		}
	}

	// And the cap holds, with no dash left dangling where it was cut.
	long := Slug(strings.Repeat("ab-", 200))
	if len(long) > maxSlugLen {
		t.Errorf("Slug of a long name is %d characters, want at most %d", len(long), maxSlugLen)
	}
	if strings.HasSuffix(long, "-") {
		t.Errorf("Slug(%q) ends in a dash", long)
	}
	if err := validSlug(long); err != nil {
		t.Errorf("Slug produced something validSlug rejects: %v", err)
	}
}

// A slug is turned straight into a path, and it arrives from the webview.
func TestLoadAndDeleteRejectSlugsThatEscapeTheDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "real", `{"tabs":[]}`); err != nil {
		t.Fatal(err)
	}
	// A file just outside the workspaces directory, to prove the traversal is
	// not merely failing on "no such file".
	outside := filepath.Join(dir, "secret.json")
	if err := os.WriteFile(outside, []byte(`{"name":"secret","state":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, slug := range []string{
		"", "..", "../secret", "../../etc/passwd", "/etc/passwd",
		"real/../../secret", "Real", "real.json", "-real", "real-", "re--al",
		strings.Repeat("a", maxSlugFileLen+1),
	} {
		if _, err := Load(dir, slug); err == nil {
			t.Errorf("Load accepted the slug %q", slug)
		}
		if err := Delete(dir, slug); err == nil {
			t.Errorf("Delete accepted the slug %q", slug)
		}
	}

	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("the file outside the workspaces directory is gone: %v", err)
	}
}

func TestListOnAMissingDirectoryIsEmptyNotNil(t *testing.T) {
	got := List(filepath.Join(t.TempDir(), "nothing", "here"))
	if got == nil {
		t.Fatal("List returned nil; it crosses the Wails bridge and must marshal to []")
	}
	if len(got) != 0 {
		t.Fatalf("List found %d workspaces in a directory that does not exist", len(got))
	}
}

func TestListSkipsUnparseableFilesAndSortsByName(t *testing.T) {
	silenceLog(t)
	dir := t.TempDir()

	for _, name := range []string{"zebra", "Apple", "mango"} {
		if err := Save(dir, name, `{"tabs":[{"layout":{"type":"leaf"}}]}`); err != nil {
			t.Fatalf("Save(%q): %v", name, err)
		}
	}
	// One file that is not JSON, and one that is not a workspace file at all.
	if err := os.WriteFile(filepath.Join(Dir(dir), "broken.json"), []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(Dir(dir), "notes.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := List(dir)
	if len(got) != 3 {
		t.Fatalf("List found %d workspaces, want 3: %+v", len(got), got)
	}
	want := []string{"Apple", "mango", "zebra"}
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("List[%d].Name = %q, want %q (full: %+v)", i, got[i].Name, w, got)
		}
	}
}

func TestCountsWalkANestedLayout(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "nested", twoTabsFourPanes); err != nil {
		t.Fatal(err)
	}

	got := List(dir)
	if len(got) != 1 {
		t.Fatalf("List found %d workspaces, want 1", len(got))
	}
	if got[0].Tabs != 2 {
		t.Errorf("Tabs = %d, want 2", got[0].Tabs)
	}
	if got[0].Panes != 4 {
		t.Errorf("Panes = %d, want 4", got[0].Panes)
	}
}

func TestCountsHandleAV1LayoutAndNonsense(t *testing.T) {
	cases := []struct {
		name              string
		state             string
		wantTabs, wantPan int
	}{
		{"v1 single root", `{"version":1,"layout":{"type":"split","children":[{"type":"leaf"},{"type":"leaf"}]}}`, 1, 2},
		{"no tabs and no layout", `{"version":2,"tabs":[]}`, 0, 0},
		{"a split with no children still counts as a pane",
			`{"tabs":[{"layout":{"type":"split"}}]}`, 1, 1},
		{"a layout that is not an object at all", `[1,2,3]`, 0, 0},
		{"a tab with a null layout", `{"tabs":[{"layout":null}]}`, 1, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tabs, panes := count(json.RawMessage(c.state))
			if tabs != c.wantTabs || panes != c.wantPan {
				t.Fatalf("count = (%d tabs, %d panes), want (%d, %d)", tabs, panes, c.wantTabs, c.wantPan)
			}
		})
	}
}

// A layout can nest as deeply as a JSON parser allows, so the walk is bounded.
func TestCountPanesIsBounded(t *testing.T) {
	var open, close string
	for range maxLayoutDepth + 50 {
		open += `{"children":[`
		close += `]}`
	}
	state := `{"tabs":[{"layout":` + open + `{"type":"leaf"}` + close + `}]}`

	tabs, panes := count(json.RawMessage(state))
	if tabs != 1 {
		t.Fatalf("tabs = %d, want 1", tabs)
	}
	// The point is that it returns rather than that it returns any particular
	// number: past the depth bound the walk stops and reports nothing.
	if panes != 0 {
		t.Fatalf("panes = %d, want 0 past the depth bound", panes)
	}
}

func TestDelete(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "Doomed", `{"tabs":[]}`); err != nil {
		t.Fatal(err)
	}
	if err := Delete(dir, "doomed"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := List(dir); len(got) != 0 {
		t.Fatalf("List still reports %d workspaces after the delete: %+v", len(got), got)
	}
	if err := Delete(dir, "doomed"); err == nil {
		t.Fatal("deleting a workspace that is not there reported success")
	}
	if _, err := Load(dir, "doomed"); err == nil {
		t.Fatal("loading a deleted workspace reported success")
	}
}

// A file left by hand — or by a version that did not record a name — still has
// to be listable and loadable.
func TestListNamesAnUnnamedFileAfterItsSlug(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(Dir(dir), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(Dir(dir), "hand-written.json"),
		[]byte(`{"state":{"tabs":[{"layout":{"type":"leaf"}}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got := List(dir)
	if len(got) != 1 {
		t.Fatalf("List found %d workspaces, want 1", len(got))
	}
	if got[0].Name != "hand-written" {
		t.Fatalf("Name = %q, want the slug", got[0].Name)
	}
	if _, err := Load(dir, got[0].Slug); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// Four differently-named workspaces that slug alike, saved at once, must leave
// four files behind.
//
// Every Wails binding runs on its own goroutine, so this is two panes' "Save
// Workspace…" prompts answered at the same moment — and before saveMu it lost
// three of the four every time: freeSlug reads the directory to pick a slug and
// Save writes it, with nothing in between to stop a second call reading the same
// empty directory and picking the same one. Losing a saved layout in silence is
// the worst outcome this package has.
func TestConcurrentSavesOfNamesThatSlugAlikeKeepThemAll(t *testing.T) {
	dir := t.TempDir()

	// Four names that are different to a user and identical to Slug: the
	// separators all collapse to the same "-". Different case-insensitively
	// too, or freeSlug would correctly treat them as updates of one another and
	// one file would be the right answer.
	names := []string{"Alpha One", "Alpha-One", "Alpha_One", "Alpha.One"}
	want := Slug(names[0])
	if want == "" {
		t.Fatal("Slug returned nothing")
	}
	for _, name := range names[1:] {
		if got := Slug(name); got != want {
			t.Fatalf("precondition failed: %q slugs to %q, not to %q — the names have to collide or this test proves nothing", name, got, want)
		}
	}

	errs := make(chan error, len(names))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // all four inside Save at once, not one after another
			errs <- Save(dir, name, twoTabsFourPanes)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	got := List(dir)
	if len(got) != len(names) {
		t.Fatalf("List found %d workspaces after %d concurrent saves, want %d: %+v", len(got), len(names), len(names), got)
	}

	slugs := map[string]bool{}
	saved := map[string]bool{}
	for _, info := range got {
		if slugs[info.Slug] {
			t.Errorf("two workspaces share the slug %q", info.Slug)
		}
		slugs[info.Slug] = true
		saved[info.Name] = true
		if _, err := Load(dir, info.Slug); err != nil {
			t.Errorf("Load(%q): %v", info.Slug, err)
		}
	}
	for _, name := range names {
		if !saved[name] {
			t.Errorf("%q was saved without an error and is not on the disk; it was overwritten by another save", name)
		}
	}
}
