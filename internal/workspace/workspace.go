// Package workspace stores named snapshots of the frontend's layout.
//
// A workspace is the same JSON the frontend already autosaves as state.json,
// kept under a name the user chose. state.json is a single slot that the app
// rewrites constantly; a workspace is a copy that nothing overwrites on its
// own, so "the four panes I use for this project" survives opening something
// else and quitting.
//
// This package deliberately does not understand the layout. The state JSON is
// stored as a json.RawMessage and handed back unchanged, so every key survives
// — including the ones added on the other side of the bridge after this file
// was written, which a Go struct with named fields would silently drop. Only
// the whitespace differs: the file is re-indented so it reads like every other
// file in the config directory. The single thing read out of the layout is the
// tab and pane count, from the cut-down shapes at the bottom of this file, and
// a layout those cannot make sense of costs a wrong count and nothing else.
package workspace

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/albinanto/elterminalo/internal/config"
)

const (
	// DirName is the subdirectory of the config directory the files live in.
	DirName = "workspaces"

	// fileExt is the extension every workspace file carries. It is also what
	// List globs for, so a stray file of another kind in the directory is
	// ignored rather than logged as unparseable.
	fileExt = ".json"

	// filePerm keeps a workspace owner-only. The layout names tabs and records
	// the working directory of every pane — the user's project paths — which is
	// the same reason state.json is 0600.
	filePerm = 0o600

	// dirPerm keeps the directory itself owner-only, so the file *names* — which
	// are slugs of names the user typed — are not readable either.
	dirPerm = 0o700

	// maxSlugLen bounds the slug built from a display name. Names are shown from
	// inside the file, so the slug only has to be a readable, unique filename;
	// 64 characters is long enough to stay recognisable and short enough that
	// the numbered collision variants below cannot approach a filesystem limit.
	maxSlugLen = 64

	// maxSlugFileLen bounds a slug arriving from a caller. It is maxSlugLen plus
	// room for the longest collision suffix Save can append ("-1000").
	maxSlugFileLen = maxSlugLen + 5

	// maxCollisions bounds Save's search for a free numbered slug. Reaching it
	// means a thousand differently-named workspaces slugged to the same string,
	// which is a bug or an attack, not a user.
	maxCollisions = 1000

	// fallbackSlug is used when a name slugs to nothing at all — a name written
	// entirely in a script with no ASCII letters or digits, say. The display
	// name is kept inside the file, so nothing is lost; these just collide with
	// each other and get numbered.
	fallbackSlug = "workspace"

	// maxLayoutDepth bounds how far the pane count walks into a saved layout.
	// The file is user data that has been through a JSON parser, so it can nest
	// as deeply as it likes; a real layout is a handful of splits deep.
	maxLayoutDepth = 100
)

// Info describes one saved workspace, without its layout. It is what the
// frontend lists in the "Open Workspace" picker, so it carries both the name
// the user typed and the slug every other call in this package takes.
type Info struct {
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	SavedUnix int64  `json:"savedUnix"`
	Tabs      int    `json:"tabs"`
	Panes     int    `json:"panes"`
}

// stored is the on-disk format. The state is held as a RawMessage so it goes
// back out byte for byte — see the package comment.
type stored struct {
	Name      string          `json:"name"`
	SavedUnix int64           `json:"savedUnix"`
	State     json.RawMessage `json:"state"`
}

// Dir returns the directory workspaces are stored in, given the app's config
// directory. Exported so callers can reveal it in Finder without rebuilding the
// path themselves.
func Dir(configDir string) string {
	return filepath.Join(configDir, DirName)
}

func path(configDir, slug string) string {
	return filepath.Join(Dir(configDir), slug+fileExt)
}

// saveMu serializes the pick-a-slug-then-write half of Save.
//
// freeSlug decides which file to write by reading the directory, and Save then
// writes it — with nothing in between stopping a second Save from reading the
// same directory and picking the same free slug. Two differently-named
// workspaces would then both land on that one file and one of them would be
// gone, which is exactly what Save's contract above promises cannot happen.
// It is reachable: every binding runs on its own goroutine, so two panes'
// "Save Workspace…" prompts answered at once are two concurrent calls.
//
// A mutex rather than an O_EXCL create on the candidate, because the check is
// not only "does this file exist" — freeSlug also reads the file to see whether
// it holds *this* name, in which case the save is an update and must reuse the
// slug. An exclusive create cannot express that, and a mutex costs a saving
// user nothing: this is a few hundred kilobytes written behind a prompt, not a
// hot path.
//
// It guards this process only. Two copies of the app saving the same name at
// the same instant can still race, which the durable write reduces to "one of
// the two survives whole" rather than a half-written file — the same guarantee
// state.json has.
var saveMu sync.Mutex

// Save writes stateJSON under name, creating the workspaces directory if it is
// not there yet.
//
// Saving under a name that already exists replaces it — that is what a user who
// retypes a name means. Two *different* names that slug to the same string do
// not: the second one gets a numbered slug, so neither file is lost and both
// show up in List under the names they were given.
func Save(configDir, name, stateJSON string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("a workspace needs a name")
	}
	// Checked here rather than at load time. A workspace that does not parse is
	// worthless, and the moment to say so is while the user is still looking at
	// the thing they tried to save.
	if !json.Valid([]byte(stateJSON)) {
		return fmt.Errorf("the layout for %q is not valid JSON", name)
	}

	dir := Dir(configDir)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("cannot create %s: %w", dir, err)
	}

	// Held from the slug decision to the write that claims it — see saveMu. The
	// encode is inside it too, which costs nothing worth splitting the critical
	// section for.
	saveMu.Lock()
	defer saveMu.Unlock()

	slug, err := freeSlug(configDir, name)
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(stored{
		Name:      name,
		SavedUnix: time.Now().Unix(),
		State:     json.RawMessage(stateJSON),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode the workspace %q: %w", name, err)
	}

	// The same fsync-then-rename every other file in the config directory is
	// published with, so a crash mid-save cannot leave a half-written workspace
	// where a whole one used to be.
	if err := config.WriteFileDurable(path(configDir, slug), data, filePerm); err != nil {
		return fmt.Errorf("cannot write the workspace %q: %w", name, err)
	}
	return nil
}

// freeSlug picks the slug Save should write name to: the plain one when the
// slot is free or already holds this same name, and the first free numbered
// variant otherwise.
//
// A file that exists but cannot be read or parsed counts as occupied. It is
// somebody's workspace with a problem, and overwriting it would be the one
// outcome worse than refusing the slug.
func freeSlug(configDir, name string) (string, error) {
	base := Slug(name)
	candidate := base
	for i := 2; ; i++ {
		existing, err := read(configDir, candidate)
		if os.IsNotExist(err) {
			return candidate, nil
		}
		if err == nil && strings.EqualFold(strings.TrimSpace(existing.Name), name) {
			return candidate, nil // same name: this is an update, not a collision
		}
		if i > maxCollisions {
			return "", fmt.Errorf("cannot save %q: %s and its %d numbered variants are all taken",
				name, base, maxCollisions-1)
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
}

// List returns every saved workspace, sorted by display name. A missing
// directory is not an error — it just means nothing has been saved yet.
//
// A file that does not parse is skipped with a log line rather than failing the
// whole listing: one bad file must not make the other workspaces unreachable.
func List(configDir string) []Info {
	// Never nil: this is returned straight across the Wails bridge, where a nil
	// slice marshals to null and the frontend would have to guard for it.
	out := []Info{}

	entries, err := os.ReadDir(Dir(configDir))
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("workspace: cannot list %s: %v", Dir(configDir), err)
		}
		return out
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), fileExt) {
			continue
		}
		slug := strings.TrimSuffix(entry.Name(), fileExt)
		s, err := read(configDir, slug)
		if err != nil {
			log.Printf("workspace: skipping %q: %v", entry.Name(), err)
			continue
		}
		tabs, panes := count(s.State)
		name := strings.TrimSpace(s.Name)
		if name == "" {
			// Written by hand, or by a version that did not record one. The
			// slug is a worse name than the one the user typed, but it is a
			// name, and an unnamed row in the picker is unopenable.
			name = slug
		}
		out = append(out, Info{
			Name:      name,
			Slug:      slug,
			SavedUnix: s.SavedUnix,
			Tabs:      tabs,
			Panes:     panes,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if li != lj {
			return li < lj
		}
		// Two workspaces whose names differ only in case still need a stable
		// order, or the picker reshuffles between listings.
		return out[i].Slug < out[j].Slug
	})
	return out
}

// Load returns the state JSON saved under slug. It is the same JSON Save was
// given — every key, in the same order, with the same values — reformatted to
// the file's indentation and nothing more.
func Load(configDir, slug string) (string, error) {
	s, err := read(configDir, slug)
	if err != nil {
		return "", err
	}
	if len(s.State) == 0 {
		return "", fmt.Errorf("the workspace %q has no layout in it", slug)
	}
	return string(s.State), nil
}

// Delete removes the workspace saved under slug.
func Delete(configDir, slug string) error {
	if err := validSlug(slug); err != nil {
		return err
	}
	if err := os.Remove(path(configDir, slug)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("there is no workspace called %q", slug)
		}
		return fmt.Errorf("cannot delete the workspace %q: %w", slug, err)
	}
	return nil
}

// read loads and decodes one workspace file. The os.IsNotExist-ness of its
// error is preserved, because freeSlug branches on it.
func read(configDir, slug string) (stored, error) {
	if err := validSlug(slug); err != nil {
		return stored{}, err
	}
	data, err := os.ReadFile(path(configDir, slug))
	if err != nil {
		return stored{}, err
	}
	var s stored
	if err := json.Unmarshal(data, &s); err != nil {
		return stored{}, fmt.Errorf("%s%s does not parse: %w", slug, fileExt, err)
	}
	return s, nil
}

// Slug turns a display name into the filename it is stored under: lower case,
// [a-z0-9-] only, runs of separators collapsed, bounded.
//
// It is idempotent — Slug(Slug(x)) == Slug(x) — which is what lets a caller
// hand either a name or a slug to the App bindings and get the same file.
func Slug(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			// Everything else — spaces, punctuation, and every non-ASCII letter
			// — becomes one separator. Transliterating "Ünïcødé" would be a
			// table this app has no other use for, and the name the user typed
			// is kept inside the file either way.
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}

	slug := strings.Trim(b.String(), "-")
	if len(slug) > maxSlugLen {
		slug = strings.Trim(slug[:maxSlugLen], "-")
	}
	if slug == "" {
		return fallbackSlug
	}
	return slug
}

// validSlug rejects anything that is not a slug this package could have
// written. The value reaches here from the frontend, so this is also what keeps
// a name like "../../.ssh/id_rsa" from being turned into a path.
//
// It is a character-class check rather than `Slug(s) == s` because Save's
// collision variants are longer than maxSlugLen allows.
func validSlug(slug string) error {
	if slug == "" {
		return fmt.Errorf("no workspace name given")
	}
	if len(slug) > maxSlugFileLen {
		return fmt.Errorf("%q is not a workspace name", slug)
	}
	if strings.HasPrefix(slug, "-") || strings.HasSuffix(slug, "-") || strings.Contains(slug, "--") {
		return fmt.Errorf("%q is not a workspace name", slug)
	}
	for _, r := range slug {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return fmt.Errorf("%q is not a workspace name", slug)
		}
	}
	return nil
}

// count reports how many tabs and panes a saved layout holds. Both are zero for
// a layout it cannot make sense of — the counts are decoration in the picker,
// and a workspace whose shape this package does not recognise still opens.
func count(state json.RawMessage) (tabs, panes int) {
	if len(state) == 0 {
		return 0, 0
	}
	var s savedState
	if err := json.Unmarshal(state, &s); err != nil {
		return 0, 0
	}
	if len(s.Tabs) == 0 {
		// A v1 layout: one root, no tabs. The frontend still migrates these.
		if s.Layout == nil {
			return 0, 0
		}
		return 1, countPanes(s.Layout, 0)
	}
	for _, tab := range s.Tabs {
		panes += countPanes(tab.Layout, 0)
	}
	return len(s.Tabs), panes
}

// countPanes counts the leaves of one tab's split tree. A node with no children
// is a pane whatever its "type" says — a "split" that lost its children is a
// broken layout, and counting it as one pane is closer to the truth than zero.
func countPanes(n *savedNode, depth int) int {
	if n == nil || depth > maxLayoutDepth {
		return 0
	}
	if len(n.Children) == 0 {
		return 1
	}
	total := 0
	for _, child := range n.Children {
		total += countPanes(child, depth+1)
	}
	return total
}

// The three shapes below mirror SavedState, SavedTab and SavedSplitNode in
// frontend/src/types.ts, cut down to the fields the counts need. They are used
// for counting only — the state itself is stored and returned as raw bytes.
type savedState struct {
	Tabs   []savedTab `json:"tabs"`
	Layout *savedNode `json:"layout"` // v1 layouts: a single root, no tabs
}

type savedTab struct {
	Layout *savedNode `json:"layout"`
}

type savedNode struct {
	Children []*savedNode `json:"children"`
}
