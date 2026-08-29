package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	stateFileName = "state.json"
	// bakSuffix names the single rotated copy of the last state.json that
	// parsed. Only one generation is kept: it exists to survive a torn write,
	// not to provide history.
	bakSuffix = ".bak"
	// corruptPrefix is joined with a unix timestamp to quarantine an
	// unparseable state.json. The file is never deleted — a user who lost a
	// large layout can still pick it apart by hand.
	corruptPrefix = ".corrupt-"
	// failedPrefix is joined with a unix timestamp to move aside a state.json
	// that parsed but that the frontend could not rebuild into a layout. It is
	// deliberately distinct from corruptPrefix so the two failure modes stay
	// tellable apart by anyone digging through the config directory.
	failedPrefix = ".failed-"
	// statePerm is the mode every file this package publishes is written with.
	// These hold tab names and the working directory of each pane — the user's
	// project paths — so they are owner-only. The mode is named explicitly at
	// each call site because WriteFileDurable applies it with Chmod, which the
	// umask does not filter: a 0644 there really is world-readable, rather than
	// whatever the umask would have made of it. Files left at 0644 by an older
	// version are tightened by the next save, which renames a fresh inode over
	// them.
	statePerm = 0o600
	// tempSuffix is the scratch-name suffix WriteFileDurable hands to
	// os.CreateTemp, where "*" is the random part it substitutes. It doubles as
	// the tail of sweepTempFiles' glob, where the same "*" matches that random
	// part — one constant so renaming the scratch files cannot leave the sweep
	// looking for names nothing is called any more.
	tempSuffix = ".tmp-*"
	// quarantineAttempts bounds how many names QuarantineState tries before it
	// gives up: the timestamped one plus quarantineAttempts-1 numbered variants.
	quarantineAttempts = 100
	// tempSweepMinAge is how old a scratch file has to be before the sweep will
	// unlink it. See sweepTempFiles for why it is not zero.
	tempSweepMinAge = 10 * time.Minute
)

// Config manages the application's configuration directory and state persistence.
type Config struct {
	dir string
	// mu serializes every path that writes into the config directory. Wails
	// dispatches each binding call on its own goroutine, so the 30s autosave
	// can overlap a save triggered by a divider drag or by Cmd-Q — and each
	// save is a read-rotate-write cycle, not a single atomic step. Without this
	// lock two of them interleave and the loser's bytes end up spliced into the
	// file the survivor publishes.
	mu sync.Mutex
}

// New creates a Config, determining and creating the config directory once.
func New() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot determine home directory: %w", err)
	}
	dir := filepath.Join(home, ".config", "elterminalo")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("cannot create config directory: %w", err)
	}
	sweepTempFiles(dir)
	return &Config{dir: dir}, nil
}

// sweepTempFiles deletes WriteFileDurable's abandoned scratch files from dir.
//
// A crash between the CreateTemp and the rename — a panic, a kill -9, a power
// loss — leaves a state.json.tmp-<random> behind, and nothing ever comes back
// for it, so they accumulate for the life of the install.
//
// The age bound is what stops this from being a bug of its own. "Startup, so
// this process is the only writer and it has not written any yet" is only true
// when there is one process: ApplyUpdate deliberately runs `open -n` on the new
// bundle *before* this instance has saved, so the new instance's New() sweeps
// the config directory while the old instance is mid-save. Measured, that is
// exactly what happened — the sweep unlinked the in-flight temp and the old
// instance's rename failed with ENOENT, so the layout at the moment of the
// update was the one save that never landed. A scratch file that is still live
// is seconds old at most; ten minutes is far past any write that is still going
// to complete and far short of "these have been here since the last crash".
//
// Failures are ignored — a file that cannot be removed is litter, not a reason
// to refuse to start.
func sweepTempFiles(dir string) {
	matches, err := filepath.Glob(filepath.Join(dir, "*"+tempSuffix))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-tempSweepMinAge)
	for _, path := range matches {
		// Only ever unlink a plain file: the pattern is broad enough to catch a
		// directory somebody put here, and this is the user's config directory.
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.ModTime().After(cutoff) {
			// Young enough that another instance may still be writing it.
			continue
		}
		_ = os.Remove(path)
	}
}

// newWithDir builds a Config rooted at an already-existing directory. Only the
// tests use it; production code goes through New so the directory is created.
func newWithDir(dir string) *Config {
	return &Config{dir: dir}
}

// Dir returns the configuration directory path.
func (c *Config) Dir() string {
	return c.dir
}

func (c *Config) statePath() string {
	return filepath.Join(c.dir, stateFileName)
}

// WriteFileDurable writes data to path via a temp file that is fsync'd before
// the rename. Without the fsync a crash can leave the rename committed while
// the data is still only in the page cache, which is exactly how a power loss
// turns a saved layout into a zero-length file. The containing directory is
// fsync'd afterwards so the rename entry itself is on disk.
//
// The temp file gets a unique name per call. A fixed path+".tmp" would be
// shared by two concurrent writers: both truncate and write the same inode,
// both fsync it, and both rename it, so the survivor publishes a byte-level
// splice of the two payloads rather than either one of them. Config.mu already
// serializes the callers here, but the unique name means even a caller that
// bypasses the lock can only lose a whole write, never corrupt the file.
//
// perm is applied literally. os.WriteFile passes its mode to open(2), where the
// umask filters it; a Chmod is not filtered, so callers get exactly the bits
// they name — see statePerm.
//
// Exported so the other packages that publish a file into this same directory —
// internal/settings, today — write it exactly the way the layout is written,
// instead of each growing its own half of the fsync-and-rename dance. It also
// means sweepTempFiles cleans up after all of them: every scratch file, whoever
// wrote it, is named with the same tempSuffix the sweep globs for.
func WriteFileDurable(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+tempSuffix)
	if err != nil {
		return err
	}
	tmp := f.Name()
	// CreateTemp always opens with 0600; the published file has to carry the
	// caller's mode instead, and setting it before the rename means the file is
	// never visible at the final path with the wrong permissions.
	if err := f.Chmod(perm); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir flushes a directory entry so a completed rename survives a power
// loss. Failures are ignored on purpose: some filesystems refuse to open a
// directory for writing, and by this point the rename has already succeeded.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// SaveState writes the serialized state JSON to disk atomically and durably,
// rotating the previous good copy to state.json.bak first.
//
// What the backup does and does not buy: LoadState falls back to it exactly
// when state.json fails to parse, so it covers a torn or truncated primary and
// nothing else. A primary that is valid JSON but wrong — the fresh single-tab
// layout written after a restore that parsed and then failed to rebuild, say —
// is never recovered from, and the first content-changing save after that
// rotates the last good copy out of the single backup slot for good.
// QuarantineState is what covers that case, and only if the caller notices.
func (c *Config) SaveState(stateJSON string) error {
	// Read-rotate-write is three separate filesystem steps; two of them
	// interleaved would rotate one save's bytes under another save's primary.
	c.mu.Lock()
	defer c.mu.Unlock()

	path := c.statePath()
	data := []byte(stateJSON)

	if prev, err := os.ReadFile(path); err == nil {
		// The autosave rewrites the same bytes most of the time. Rotating on
		// every one of those would quickly push the only genuinely different
		// copy out of the single backup slot.
		if bytes.Equal(prev, data) {
			return nil
		}
		// Only ever promote a file that parses — a corrupt primary must not be
		// allowed to overwrite a good backup.
		if json.Valid(prev) {
			// A failed rotation is not fatal: saving the new state still beats
			// refusing to save anything.
			_ = WriteFileDurable(path+bakSuffix, prev, statePerm)
		}
	}

	return WriteFileDurable(path, data, statePerm)
}

// LoadState reads the saved state JSON from disk. A state.json that exists but
// does not parse is moved aside instead of being silently replaced by the next
// save, and the rotated backup is used in its place.
func (c *Config) LoadState() string {
	// Takes the write lock too: the quarantine branch below renames the very
	// file SaveState rotates and rewrites.
	c.mu.Lock()
	defer c.mu.Unlock()

	path := c.statePath()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "" // first run: nothing saved yet
		}
		// Present but unreadable (permissions, I/O error). Leave it alone and
		// try the backup rather than reporting "no layout".
		log.Printf("state: %s exists but cannot be read (%v); trying the backup", stateFileName, err)
		return c.loadStateBackup()
	}

	// json.Valid rejects empty and whitespace-only files too, which is what a
	// power loss during the old non-fsync'd write used to leave behind.
	if json.Valid(data) {
		log.Printf("state: loaded %s (%d bytes)", stateFileName, len(data))
		return string(data)
	}

	// Quarantine rather than delete: the next save would otherwise destroy the
	// only remaining trace of the layout.
	quarantine := fmt.Sprintf("%s%s%d", path, corruptPrefix, time.Now().Unix())
	if err := os.Rename(path, quarantine); err != nil {
		// Could not move it aside — still prefer the backup over the garbage.
		log.Printf("state: %s is corrupt (%d bytes) and could not be quarantined: %v", stateFileName, len(data), err)
		return c.loadStateBackup()
	}
	syncDir(c.dir)
	log.Printf("state: %s did not parse (%d bytes); quarantined as %s", stateFileName, len(data), filepath.Base(quarantine))

	return c.loadStateBackup()
}

// QuarantineState renames state.json to state.json.failed-<unix> and returns
// the new path, or ("", nil) when there is no state.json to move.
//
// This is the escape hatch for a layout that parsed but could not be rebuilt.
// LoadState cannot catch that: the file is valid JSON, so it hands it straight
// back and the backup is never consulted. The caller falls back to a fresh
// single-tab layout, and its next save would write that over the real one and
// then rotate the last good copy out of .bak. Moving the file aside means the
// next save starts a new file while the old layout survives on disk. Nothing
// is ever deleted here.
func (c *Config) QuarantineState() (string, error) {
	// Same lock as SaveState: a save that landed between the stat and the
	// rename would be quarantined instead of kept.
	c.mu.Lock()
	defer c.mu.Unlock()

	path := c.statePath()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", nil // nothing saved yet, nothing to move
		}
		return "", err
	}

	base := fmt.Sprintf("%s%s%d", path, failedPrefix, time.Now().Unix())
	quarantine := base
	// Two quarantines inside the same second must not clobber each other: the
	// file being moved is the user's only remaining copy of that layout. Every
	// candidate is tested, the last one included — the bound is on how many
	// names are tried, not on how many are checked, so the loop can never fall
	// out onto a name it never looked at. Exhausting them is not a reason to
	// overwrite one; it is a reason to leave state.json where it is and say so.
	for i := 1; fileExists(quarantine); i++ {
		if i >= quarantineAttempts {
			err := fmt.Errorf("cannot quarantine %s: %s and its %d numbered variants all exist",
				stateFileName, base, quarantineAttempts-1)
			log.Printf("state: %v", err)
			return "", err
		}
		quarantine = fmt.Sprintf("%s-%d", base, i)
	}

	if err := os.Rename(path, quarantine); err != nil {
		log.Printf("state: cannot move %s aside after a failed restore: %v", stateFileName, err)
		return "", err
	}
	syncDir(c.dir)
	log.Printf("state: layout could not be rebuilt; %s moved aside to %s", stateFileName, filepath.Base(quarantine))

	return quarantine, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// loadStateBackup returns the rotated backup, or "" if it is missing or is
// itself unparseable.
func (c *Config) loadStateBackup() string {
	data, err := os.ReadFile(c.statePath() + bakSuffix)
	if err != nil {
		log.Printf("state: no usable %s either (%v); starting from an empty layout", stateFileName+bakSuffix, err)
		return ""
	}
	if !json.Valid(data) {
		log.Printf("state: %s does not parse either (%d bytes); starting from an empty layout", stateFileName+bakSuffix, len(data))
		return ""
	}
	log.Printf("state: restored the layout from %s (%d bytes)", stateFileName+bakSuffix, len(data))
	return string(data)
}

// WindowGeometry stores the window size and position.
type WindowGeometry struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	X      int `json:"x"`
	Y      int `json:"y"`
}

// SaveWindowGeometry persists window size and position to disk.
func (c *Config) SaveWindowGeometry(g WindowGeometry) error {
	data, err := json.Marshal(g)
	if err != nil {
		return err
	}
	// Shares the lock with the state writes: one mutex over the whole config
	// directory is cheap here — these are user-driven saves, not a hot path —
	// and it means no future write path can be added without being serialized.
	c.mu.Lock()
	defer c.mu.Unlock()
	return WriteFileDurable(filepath.Join(c.dir, "window.json"), data, statePerm)
}

// LoadWindowGeometry reads the saved window geometry from disk.
func (c *Config) LoadWindowGeometry() *WindowGeometry {
	data, err := os.ReadFile(filepath.Join(c.dir, "window.json"))
	if err != nil {
		return nil
	}
	var g WindowGeometry
	if json.Unmarshal(data, &g) != nil {
		return nil
	}
	if g.Width < 400 || g.Height < 300 {
		return nil
	}
	// Reject positions that are unreasonably far off-screen
	if g.X < -10000 || g.X > 20000 || g.Y < -10000 || g.Y > 20000 {
		return nil
	}
	return &g
}
