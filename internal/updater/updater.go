package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

const (
	githubRepo      = "albinantoab/ElTerminalo"
	apiURL          = "https://api.github.com/repos/" + githubRepo + "/releases/latest"
	checkTimeout    = 5 * time.Second
	downloadTimeout = 5 * time.Minute

	// checksumsAssetName is what scripts/release.sh publishes alongside the dmg
	// and the zip: one `<sha256>  <filename>` line per artifact, as written by
	// `shasum -a 256`.
	checksumsAssetName = "checksums-sha256.txt"

	// maxChecksumsBytes bounds the checksums download. The real file is a
	// couple of hundred bytes; anything remotely near this cap is not one.
	maxChecksumsBytes = 64 << 10

	// maxReleaseBytes bounds the release JSON. GitHub's payload for a release
	// with a handful of assets is a few kilobytes; a megabyte is far past any
	// legitimate one, and without a bound a hostile or broken response can make
	// the decoder allocate until the app dies.
	maxReleaseBytes = 1 << 20

	// maxDownloadBytes bounds the zip. The signed bundle is well under 100 MiB,
	// so half a gigabyte leaves plenty of headroom while still capping what a
	// redirect to an endless response body can write into the user's temp
	// directory. Hitting the cap is an error, not a truncation: a partial zip
	// must never reach the checksum step and be reported as a mismatch.
	maxDownloadBytes = 512 << 20

	// macOSAssetToken is required in an asset's name on top of the architecture
	// token. Without it a release that also ships "…-linux-amd64.zip" hands that
	// asset to an amd64 Mac: it downloads, it passes the checksum — the release
	// really does list it — and only dies at findApp, after a five-minute
	// download, with an error that says nothing about what went wrong.
	macOSAssetToken = "macos"
)

// The external tools this package runs, by absolute path.
//
// Resolving them through $PATH made every gate here defeatable by the thing it
// is meant to protect against: a user (or anything running as the user) that
// puts a "codesign" earlier in $PATH which exits 0 and prints a matching
// TeamIdentifier= turns signature checking, team matching and Gatekeeper into
// three no-ops, and the app then installs whatever the download produced.
// /usr/bin and /usr/sbin are on the read-only system volume and are what the
// system's own tooling uses.
const (
	dittoPath    = "/usr/bin/ditto"
	codesignPath = "/usr/bin/codesign"
	spctlPath    = "/usr/sbin/spctl"

	// OpenPath is exported because package main launches the same tool from the
	// "Reveal logs in Finder" binding. One definition, with the reasoning above
	// attached to it, rather than two string literals that can drift apart.
	OpenPath = "/usr/bin/open"
)

// errUpdateInProgress is returned when ApplyUpdate is already running. The
// second click on "Update" must not start a second download, a second extract
// and — worst of all — a second swap of /Applications.
var errUpdateInProgress = errors.New("an update is already being installed")

// updateInFlight is the guard behind errUpdateInProgress. It is deliberately
// never cleared once swapBundle has returned nil — whether or not the relaunch
// that follows succeeded. At that point the new bundle is in place, so a second
// run could only overwrite a good install with another copy of the same bytes;
// and a failed relaunch is exactly the case where the UI offers "Failed —
// retry", which without the latch would do that swap all over again.
var updateInFlight atomic.Bool

// commandRunner runs an external tool and returns everything it printed.
//
// Combined output, not just stdout: `codesign -dv` writes its whole report to
// stderr, so a stdout-only runner would see nothing at all.
type commandRunner func(name string, args ...string) ([]byte, error)

// runCommand is a variable so tests can inject a fake and assert which
// verification commands ran, in which order, without a signed bundle or a
// working Gatekeeper on the machine running the tests.
//
// The error is passed through exactly as exec produced it, never wrapped into a
// message: a caller has to be able to tell "the tool ran and said no" from "the
// tool never ran", and the only thing carrying that distinction is the error's
// type. See toolRejected.
var runCommand commandRunner = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// toolRejected reports whether err is a tool's verdict rather than a failure to
// run it at all.
//
// exec.Cmd.Run returns an *exec.ExitError only when the process started, ran to
// completion and exited non-zero — that is codesign saying the bundle is bad.
// Everything else it can return means the check never happened: the fork failed
// under login-time load, the binary is missing or not executable, a pipe or the
// filesystem gave out. Those must not be read as a verdict. Treating them as one
// is how a transient failure at startup used to downgrade the app to the backup
// and delete the newer bundle.
func toolRejected(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

// osRename is a variable for the same reason runCommand is. The branches that
// decide whether a failed install leaves the user with an app at all are the
// ones reached when a rename fails, and a real filesystem gives a test no way to
// make the *second* of two renames in the same directory fail. Production always
// uses os.Rename; every rename in this package goes through here so there is one
// seam rather than a special case.
var osRename = os.Rename

// UpdateInfo contains information about an available update.
type UpdateInfo struct {
	Available  bool   `json:"available"`
	CurrentVer string `json:"currentVersion"`
	LatestVer  string `json:"latestVersion"`
	URL        string `json:"url"`
}

// CleanupStaleBackup resolves the running bundle and hands it to
// cleanupStaleBackup. Safe to call on every startup; a no-op outside a bundle.
//
// It runs from a goroutine in startup (a backup makes it shell out to codesign,
// which is long enough to be visible in a cold start), so it has to be safe
// against an ApplyUpdate that starts while it is still running: the two would
// otherwise be renaming the same three paths at once. It claims the same
// updateInFlight latch ApplyUpdate does, which makes them mutually exclusive in
// both directions:
//
//   - an install already running → this returns without touching anything, which
//     is right: the backup it would look at is one ApplyUpdate is in the middle
//     of creating;
//   - this already running → ApplyUpdate returns errUpdateInProgress and the
//     user's click is refused. Refusing rather than waiting is deliberate: the
//     window is one codesign of a bundle that only exists after an interrupted
//     install, the retry is a second click, and a wait here would mean blocking
//     a binding on a subprocess.
//
// Unlike ApplyUpdate's, this claim is always released — nothing it does makes a
// later install unsafe.
func CleanupStaleBackup() {
	appPath, err := currentAppPath()
	if err != nil {
		return
	}
	cleanupStaleBackupGuarded(appPath)
}

// cleanupStaleBackupGuarded is CleanupStaleBackup minus the path lookup, so the
// interlock above can be tested without a real bundle to run from.
func cleanupStaleBackupGuarded(appPath string) {
	if !updateInFlight.CompareAndSwap(false, true) {
		log.Printf("update: an install is in flight; skipping the stale-backup cleanup")
		return
	}
	// Only ever released here, on the path that took it: ApplyUpdate's own claim
	// is latched on purpose and must not be cleared by this function returning.
	defer updateInFlight.Store(false)
	cleanupStaleBackup(appPath)
}

// cleanupStaleBackup decides what to do with a .app.backup left behind by an
// earlier install: delete it, or put it back.
//
// It used to be an unconditional RemoveAll, and that made it the second half of
// a way to lose the app entirely. swapBundle moved the installed bundle to
// .backup and only then started copying the new one into place, so a force-quit
// or a power loss during the copy left /Applications/ElTerminalo.app missing or
// half-written with the only intact copy sitting in .backup — which this
// function then deleted at the next launch. swapBundle no longer opens that
// window (see below), but a backup from a *previous* version of this code, or
// from an install interrupted between the two renames, still can, and the
// backup is the last copy either way.
//
// So the backup is only ever deleted when the installed app is provably fine:
//
//	installed app         | backup exists | action
//	----------------------|---------------|--------------------------------------
//	missing               | yes           | restore the backup
//	present, verify failed| yes           | restore the backup, park the bad one
//	present, cannot verify| yes           | nothing; both bundles stay
//	present, verifies     | yes           | delete the backup and any parked one
//	anything              | no            | nothing
//
// "Verifies" is `codesign --verify --strict`, run through the same runner as
// the install-time checks — a bundle whose copy was cut short fails it, which is
// exactly the case that has to be caught. --deep is deliberately left off: this
// is a startup path and the shallow check already catches a truncated tree.
//
// "Verify failed" and "cannot verify" are different answers and get different
// treatment. codesign exiting non-zero is a verdict about the bundle; codesign
// failing to run — a fork that could not be satisfied under login-time load, a
// missing or unexecutable tool, an I/O error — says nothing about the bundle at
// all. Both arrive here as a non-nil error, and reading the second as the first
// is a silent downgrade to the older version, on a machine that is merely busy,
// with no way back. So only a verdict may move anything.
func cleanupStaleBackup(appPath string) {
	backupPath := appPath + ".backup"
	if _, err := os.Stat(backupPath); err != nil {
		return // the normal case: nothing was left behind
	}

	if _, err := os.Stat(appPath); err != nil {
		log.Printf("update: %s is missing but %s is present; restoring the backup", appPath, backupPath)
		if rerr := osRename(backupPath, appPath); rerr != nil {
			log.Printf("update: could not restore %s from %s: %v", appPath, backupPath, rerr)
			return
		}
		log.Printf("update: restored %s from the backup left by an interrupted install", appPath)
		return
	}

	out, err := runCommand(codesignPath, "--verify", "--strict", appPath)
	switch {
	case err == nil:
		// Fall through to the deletions below.
	case toolRejected(err):
		log.Printf("update: %s fails signature verification (%v: %s); restoring the backup",
			appPath, err, trimOutput(out))
		restoreBackup(appPath, backupPath)
		return
	default:
		// Nothing is renamed and nothing is deleted: the installed app stays the
		// one the user launched, the backup stays available, and the next start
		// asks again.
		log.Printf("updater: cannot verify installed app (%v); leaving backup in place", err)
		return
	}

	if err := os.RemoveAll(backupPath); err != nil {
		log.Printf("update: %s verifies but the stale backup %s could not be removed: %v", appPath, backupPath, err)
		return
	}
	log.Printf("update: %s verifies; removed the stale backup %s left by an earlier install", appPath, backupPath)

	// This is the one moment a parked bundle is safe to drop. Until the installed
	// app has proved itself, a .broken left by an earlier restore is the newest
	// copy of the app on the machine and deleting it is exactly what this whole
	// function exists to stop.
	brokenPath := appPath + ".broken"
	if _, serr := os.Stat(brokenPath); serr != nil {
		return
	}
	if err := os.RemoveAll(brokenPath); err != nil {
		log.Printf("update: could not remove the parked bundle %s: %v", brokenPath, err)
		return
	}
	log.Printf("update: removed %s, parked by an earlier restore", brokenPath)
}

// restoreBackup swaps a bundle that failed verification out for the backup.
//
// The broken bundle is renamed aside rather than deleted first: if the second
// rename then fails, putting it back is the difference between "the app is the
// one that was already there" and "there is no app".
//
// It is then *kept* at .broken rather than deleted. It is the newer of the two
// bundles, and a codesign verdict is not proof that it is worthless — a
// clobbered resource, an interrupted copy or a signature this machine cannot
// evaluate all land here, and some of them are recoverable by hand. Deleting it
// made the downgrade permanent and unrecoverable, which is far more damage than
// this function is entitled to do at startup on the strength of one check. The
// path is logged so a user who has just been put back on an older version can
// find it, and cleanupStaleBackup removes it at the next start where the
// installed app verifies.
func restoreBackup(appPath, backupPath string) {
	brokenPath := appPath + ".broken"
	// A .broken already sitting here was condemned by an earlier restore and has
	// been superseded since; it is the only bundle this function may delete, and
	// it has to go for the rename below to have anywhere to land.
	os.RemoveAll(brokenPath)
	if err := osRename(appPath, brokenPath); err != nil {
		log.Printf("update: could not move the unverifiable %s aside: %v; leaving the backup at %s", appPath, err, backupPath)
		return
	}
	if err := osRename(backupPath, appPath); err != nil {
		log.Printf("update: could not restore %s from %s: %v; putting the unverifiable bundle back", appPath, backupPath, err)
		if rerr := osRename(brokenPath, appPath); rerr != nil {
			log.Printf("update: and could not put it back either: %v; the app is now at %s", rerr, brokenPath)
		}
		return
	}
	log.Printf("update: restored %s from the backup; the unverifiable bundle is kept at %s", appPath, brokenPath)
}

// Check queries GitHub for the latest release and compares with the current version.
func Check(currentVersion string) UpdateInfo {
	info := UpdateInfo{CurrentVer: currentVersion}

	release, err := fetchLatestRelease()
	if err != nil {
		log.Printf("update check: failed: %v", err)
		return info
	}

	latest := strings.TrimPrefix(release.TagName, "v")
	info.LatestVer = latest
	info.URL = release.HTMLURL

	if latest != "" && latest != currentVersion && isNewer(latest, currentVersion) {
		info.Available = true
	}
	log.Printf("update check: current=%s latest=%s available=%t", currentVersion, latest, info.Available)

	return info
}

// ApplyUpdate downloads the latest release, verifies it, replaces the running
// app bundle and launches the new one. It returns nil once the new instance has
// been started; quitting this one is the caller's job, so that the normal
// shutdown path runs and the layout is saved.
//
// Nothing under /Applications is touched until all of these have passed:
//
//  1. the asset name matches the architecture this binary was built for — no
//     fallback, because installing an amd64 build over an arm64 one produces an
//     app that will not launch;
//  2. the zip's SHA-256 matches the line for that filename in the release's
//     checksums-sha256.txt;
//  3. the bundle extracted from it (with ditto, so symlinks, resource forks and
//     the signature survive) passes codesign --verify --strict --deep, carries
//     the same TeamIdentifier as the bundle we are running, and is accepted by
//     Gatekeeper.
//
// Before this, the zip was unpacked with archive/zip — which writes symlinks out
// as regular files — and installed without any verification at all: whatever a
// GitHub URL returned was executed.
func ApplyUpdate() error {
	// First statement on purpose: a second click must cost nothing, not a
	// second five-minute download.
	//
	// The startup stale-backup cleanup claims the same flag, so for the length of
	// one codesign at launch — and only when a previous install was interrupted —
	// this can also refuse a first click. That is the safe way round: the two
	// would otherwise be renaming the same three paths at once. The retry is one
	// more click.
	if !updateInFlight.CompareAndSwap(false, true) {
		log.Printf("update: refused, an install or the startup backup check is already running")
		return errUpdateInProgress
	}
	// installed latches the guard once the bundle is on disk. Everything before
	// the swap is retryable and clears it on the way out; nothing after the swap
	// is, because the retry would be an install on top of the new version.
	installed := false
	defer func() {
		if !installed {
			updateInFlight.Store(false)
		}
	}()

	appPath, err := currentAppPath()
	if err != nil {
		log.Printf("update: cannot locate the running app bundle: %v", err)
		return fmt.Errorf("cannot locate app bundle: %w", err)
	}
	log.Printf("update: starting; running bundle is %s (%s)", appPath, runtime.GOARCH)

	release, err := fetchLatestRelease()
	if err != nil {
		log.Printf("update: cannot read the latest release: %v", err)
		return err
	}

	zipAsset, err := findZipAsset(release.Assets, runtime.GOARCH)
	if err != nil {
		log.Printf("update: %v", err)
		return err
	}
	sumsAsset, err := findChecksumsAsset(release.Assets)
	if err != nil {
		log.Printf("update: %v", err)
		return err
	}
	// Both URLs come out of a JSON document fetched over the network, so they
	// are attacker-controlled the moment anything sits between us and GitHub.
	// Neither is fetched until it has been checked.
	if err := validateAssetURL(zipAsset.BrowserDownloadURL); err != nil {
		log.Printf("update: REJECTED, %s: %v", zipAsset.Name, err)
		return fmt.Errorf("%s: %w", zipAsset.Name, err)
	}
	if err := validateAssetURL(sumsAsset.BrowserDownloadURL); err != nil {
		log.Printf("update: REJECTED, %s: %v", sumsAsset.Name, err)
		return fmt.Errorf("%s: %w", sumsAsset.Name, err)
	}
	log.Printf("update: release %s selected asset %s", release.TagName, zipAsset.Name)

	tmpDir, err := os.MkdirTemp("", "elterminalo-update-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	// Reachable now that this function returns instead of calling os.Exit.
	defer os.RemoveAll(tmpDir)

	zipPath := filepath.Join(tmpDir, filepath.Base(zipAsset.Name))
	size, err := downloadFile(zipAsset.BrowserDownloadURL, zipPath)
	if err != nil {
		log.Printf("update: download of %s failed: %v", zipAsset.Name, err)
		return fmt.Errorf("download failed: %w", err)
	}
	log.Printf("update: downloaded %s (%d bytes)", zipAsset.Name, size)

	sums, err := downloadChecksums(sumsAsset.BrowserDownloadURL)
	if err != nil {
		log.Printf("update: cannot read %s: %v", checksumsAssetName, err)
		return fmt.Errorf("cannot read %s: %w", checksumsAssetName, err)
	}
	if err := verifyChecksum(zipPath, sums, filepath.Base(zipAsset.Name)); err != nil {
		log.Printf("update: REJECTED, %v", err)
		return err
	}
	log.Printf("update: checksum ok for %s", zipAsset.Name)

	extractDir := filepath.Join(tmpDir, "extracted")
	if err := extractZip(zipPath, extractDir); err != nil {
		log.Printf("update: extraction failed: %v", err)
		return fmt.Errorf("extraction failed: %w", err)
	}

	newAppPath, err := findApp(extractDir)
	if err != nil {
		log.Printf("update: %v", err)
		return fmt.Errorf("no .app found in update: %w", err)
	}

	if err := verifyBundle(newAppPath, appPath); err != nil {
		log.Printf("update: REJECTED, %v", err)
		return err
	}
	log.Printf("update: signature, team identity and Gatekeeper all accept %s", filepath.Base(newAppPath))

	if err := swapBundle(newAppPath, appPath); err != nil {
		log.Printf("update: install failed: %v", err)
		return err
	}
	// Set the moment the bundle is on disk, not after the relaunch. This flag is
	// what keeps updateInFlight latched, and a latched updateInFlight is what
	// makes a second ApplyUpdate return errUpdateInProgress. If it were set
	// after the relaunch, a relaunch failure would clear the guard and the UI's
	// "Failed — retry" would run the whole download-verify-swap again on top of
	// a bundle that is already the new version: swapBundle would move the
	// freshly installed app to .backup and copy an identical tree over it, for
	// nothing, with a fresh window in which something can go wrong.
	installed = true
	log.Printf("update: swapped %s into place", appPath)

	// Started and reaped: a Start with no matching Wait leaves the launcher as a
	// zombie child of this process. It normally has milliseconds to live — the
	// caller quits right after — but "normally" stops being true the moment the
	// quit handshake is already in flight and this instance sticks around.
	launch := exec.Command(OpenPath, "-n", appPath)
	if err := launch.Start(); err != nil {
		// Not a failed update — a failed *launch* of a successful one. The error
		// has to say so, because the only correct next step for the user is to
		// open the app again by hand, and retrying the install is refused from
		// here on.
		log.Printf("update: installed, but relaunch failed: %v", err)
		return fmt.Errorf("the update is installed but the new version could not be launched (%w); quit and open El Terminalo again", err)
	}
	go func() { _ = launch.Wait() }()
	log.Printf("update: relaunched %s; handing back to quit this instance", appPath)

	return nil
}

// swapBundle replaces the installed bundle with the verified one.
//
// Order matters more than anything else here. The previous version renamed the
// installed app to .backup and *then* ran ditto to copy the new one into its
// place, which meant /Applications/ElTerminalo.app was missing or half-written
// for however long a several-hundred-megabyte tree copy takes. A force-quit or
// a power loss inside that window left the machine with no usable app, and the
// only intact copy was the .backup that CleanupStaleBackup then deleted at the
// next launch.
//
// So the copy happens first, into a sibling path, while the installed bundle is
// still untouched and still the one the user would launch. Only once it is
// complete do the two renames run, and a rename is a single metadata operation:
// there is no instant at which appPath is a partial tree.
//
//  1. ditto new → app.new     (slow; app untouched and launchable throughout)
//  2. rename app → app.backup (instant)
//  3. rename app.new → app    (instant)
//  4. remove app.backup
//
// A crash between 2 and 3 leaves the app missing with a complete .backup beside
// it, which is precisely what cleanupStaleBackup restores from. A failure of 3
// itself renames the backup straight back.
func swapBundle(newAppPath, appPath string) error {
	backupPath := appPath + ".backup"
	stagedPath := appPath + ".new"
	os.RemoveAll(stagedPath)

	// ditto, not a hand-rolled tree copy: the extracted bundle carries symlinks,
	// resource forks and extended attributes that are part of what codesign and
	// Gatekeeper just accepted. Copying it with anything that drops those would
	// install a bundle that no longer matches the one that was verified.
	//
	// It also has to be a copy rather than a rename of the extracted bundle: the
	// temp directory is very likely on a different filesystem, where a rename
	// cannot work at all.
	if out, err := runCommand(dittoPath, newAppPath, stagedPath); err != nil {
		os.RemoveAll(stagedPath)
		return fmt.Errorf("failed to stage the update next to the installed app: %w (%s)", err, trimOutput(out))
	}

	// Any backup still sitting here is from an install that already finished —
	// cleanupStaleBackup would have restored it otherwise — and rename onto a
	// non-empty directory fails, so it has to go before step 2.
	os.RemoveAll(backupPath)

	if err := osRename(appPath, backupPath); err != nil {
		os.RemoveAll(stagedPath)
		return fmt.Errorf("failed to backup current app: %w", err)
	}

	if err := osRename(stagedPath, appPath); err != nil {
		if rerr := osRename(backupPath, appPath); rerr != nil {
			// Worst case: no app at appPath, the previous one at .backup and the
			// staged one still at .new. cleanupStaleBackup restores from .backup
			// at the next start, and nothing anywhere ever installs from .new —
			// it is litter, not a copy anybody can use — so it goes here too,
			// exactly as it does on every other failure branch.
			os.RemoveAll(stagedPath)
			return fmt.Errorf("failed to install update (%w) and could not restore the previous app from %s: %v",
				err, backupPath, rerr)
		}
		os.RemoveAll(stagedPath)
		return fmt.Errorf("failed to install update: %w", err)
	}

	os.RemoveAll(backupPath)
	return nil
}

// --- release metadata ---

type ghRelease struct {
	TagName string    `json:"tag_name"`
	HTMLURL string    `json:"html_url"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func fetchLatestRelease() (ghRelease, error) {
	var release ghRelease

	resp, err := newHTTPClient(checkTimeout).Get(apiURL)
	if err != nil {
		return release, fmt.Errorf("failed to check releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return release, fmt.Errorf("failed to check releases: HTTP %d", resp.StatusCode)
	}
	// Bounded: the decoder allocates from whatever the body hands it, so an
	// endless response body is otherwise an out-of-memory kill on a path that
	// runs unattended once a day. A real release payload is a few kilobytes, so
	// a body that reaches this cap is not one and the truncation shows up as a
	// parse error rather than as a silently short release.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseBytes)).Decode(&release); err != nil {
		return release, fmt.Errorf("failed to parse release: %w", err)
	}
	return release, nil
}

// allowedAssetHostSuffixes are the domains a release asset may be fetched from.
// Everything scripts/release.sh publishes is served from one of them, and the
// URLs come out of a JSON document rather than out of this source file, so
// without this the release metadata gets to name the host the update is
// downloaded from.
var allowedAssetHostSuffixes = []string{"github.com", "githubusercontent.com"}

// validateAssetURL rejects an asset URL that is not https on a GitHub host.
//
// The checksum and signature gates downstream are what make the *contents*
// trustworthy; this is about not making the request in the first place. A
// plain-http URL is one an on-path attacker can answer, and an arbitrary host
// turns the release metadata into a way to make the app fetch from anywhere —
// including a link-local or file-serving address on the user's own network.
func validateAssetURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("download URL %q does not parse: %w", raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("download URL %q is not https; refusing to fetch it", raw)
	}
	// Hostname() strips the port and the brackets around a literal IPv6 address,
	// so neither can be used to sneak past the suffix test.
	host := strings.ToLower(u.Hostname())
	for _, suffix := range allowedAssetHostSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return nil
		}
	}
	return fmt.Errorf("download URL %q is hosted on %q, not on GitHub; refusing to fetch it", raw, host)
}

// newHTTPClient builds the client every request in this package goes through.
//
// The redirect policy is the reason it exists. Go follows redirects by default
// and will happily follow an https URL to a plain-http one, which would undo
// the scheme check on the original URL: a 302 from GitHub — or from anything
// impersonating it — could move the download onto a channel an on-path attacker
// controls. The host is deliberately *not* pinned across redirects: GitHub
// serves release assets by redirecting to a CDN whose host varies, and the
// checksum and signature gates are what decide whether the bytes are acceptable.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing to follow a redirect to %s://%s: updates are only fetched over https",
					req.URL.Scheme, req.URL.Host)
			}
			// http.Client's own default; restated because setting CheckRedirect
			// replaces it, and without it a redirect loop never terminates.
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}
}

// findZipAsset picks the release macOS zip built for goarch.
//
// Both tokens are required. Architecture alone is not enough: a release that
// also ships Linux or Windows artifacts names them "…-linux-amd64.zip" and
// "…-windows-amd64.zip", and on an amd64 Mac an architecture-only match picks
// whichever of those the release happens to list first. That asset downloads
// fine and passes the checksum — the release genuinely lists it — and the run
// only ends at findApp, several minutes in, with an error about a missing .app.
//
// There is also no fallback to "some macOS zip". The old code had one, and on a
// release that happened to publish only the other architecture it would install
// a bundle that cannot run on this machine — over the working one, with the
// backup already deleted. A clear error is the only safe answer.
func findZipAsset(assets []ghAsset, goarch string) (ghAsset, error) {
	tokens := assetArchTokens(goarch)
	if tokens == nil {
		return ghAsset{}, fmt.Errorf("no update available for architecture %q", goarch)
	}
	// Two passes so an exact architecture match always beats a universal build.
	for _, token := range tokens {
		for _, a := range assets {
			name := strings.ToLower(a.Name)
			if strings.HasSuffix(name, ".zip") && hasToken(name, macOSAssetToken) && hasToken(name, token) {
				return a, nil
			}
		}
	}
	return ghAsset{}, fmt.Errorf("release has no macOS zip built for %s; refusing to install a build for another platform or architecture", goarch)
}

// assetArchTokens lists the names scripts/release.sh puts in an asset for this
// architecture, most specific first. "universal" is accepted for either one: a
// fat binary really does run here.
func assetArchTokens(goarch string) []string {
	switch goarch {
	case "arm64":
		return []string{"arm64", "universal"}
	case "amd64":
		return []string{"amd64", "universal"}
	default:
		return nil
	}
}

func findChecksumsAsset(assets []ghAsset) (ghAsset, error) {
	for _, a := range assets {
		if strings.EqualFold(a.Name, checksumsAssetName) {
			return a, nil
		}
	}
	return ghAsset{}, fmt.Errorf("release publishes no %s; refusing to install an unverified update", checksumsAssetName)
}

// hasToken reports whether name contains token with non-alphanumeric characters
// (or nothing at all) on both sides.
//
// A plain substring test would accept an "…-arm64e.zip" asset — a different ABI
// — as an arm64 build, and any future "…-amd64-slim…" naming as an exact match.
// Requiring a delimiter means an unrecognised name is rejected rather than
// guessed at, which for a step that overwrites /Applications is the right way
// to be wrong.
func hasToken(name, token string) bool {
	for i := 0; i+len(token) <= len(name); {
		j := strings.Index(name[i:], token)
		if j < 0 {
			return false
		}
		start := i + j
		if !isAlnumAt(name, start-1) && !isAlnumAt(name, start+len(token)) {
			return true
		}
		i = start + 1
	}
	return false
}

func isAlnumAt(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// --- download and integrity ---

func downloadFile(rawURL, dest string) (int64, error) {
	resp, err := newHTTPClient(downloadTimeout).Get(rawURL)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// One byte past the cap, so a body that is exactly at the limit still
	// succeeds and one byte over is detectable. Reaching it is an error rather
	// than a truncation: a short zip would otherwise be reported downstream as a
	// checksum mismatch, which says the release is corrupt when what actually
	// happened is that the response was longer than anything we will accept.
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return n, err
	}
	if n > maxDownloadBytes {
		return n, fmt.Errorf("download exceeds the %d byte limit; refusing it", int64(maxDownloadBytes))
	}
	return n, nil
}

func downloadChecksums(rawURL string) (map[string]string, error) {
	resp, err := newHTTPClient(checkTimeout).Get(rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxChecksumsBytes))
	if err != nil {
		return nil, err
	}
	sums := parseChecksums(data)
	if len(sums) == 0 {
		return nil, errors.New("no checksum lines found")
	}
	return sums, nil
}

// parseChecksums reads `shasum -a 256` output into filename → hash.
//
// Both of shasum's separators are handled: two spaces for text mode and " *"
// for binary mode. Only the first space is treated as the separator, so a
// filename containing spaces survives intact. Lines whose first field is not 64
// hex characters are skipped rather than trusted.
func parseChecksums(data []byte) map[string]string {
	sums := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		hash, rest, ok := strings.Cut(line, " ")
		if !ok || !isSHA256Hex(hash) {
			continue
		}
		name := strings.TrimPrefix(strings.TrimLeft(rest, " "), "*")
		if name == "" {
			continue
		}
		sums[name] = strings.ToLower(hash)
	}
	return sums
}

func isSHA256Hex(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// verifyChecksum compares the file at path against the release's own listing.
// A name with no entry is a failure, not a pass: an unlisted artifact is
// exactly as unverified as one whose hash does not match.
func verifyChecksum(path string, sums map[string]string, name string) error {
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("%s lists no entry for %s; refusing to install an unverified download", checksumsAssetName, name)
	}
	got, err := fileSHA256(path)
	if err != nil {
		return fmt.Errorf("cannot hash the download: %w", err)
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s: download is %s, release lists %s", name, got, want)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractZip unpacks src into dest with ditto.
//
// archive/zip cannot be used here. It has no notion of a symlink and writes one
// out as a regular file containing the link text, and it drops the resource
// forks and extended attributes that ditto stored — several of which are part of
// what codesign checks. release.sh builds the zip with `ditto -c -k
// --sequesterRsrc --keepParent`; this is the matching half of that.
func extractZip(src, dest string) error {
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	if out, err := runCommand(dittoPath, "-x", "-k", src, dest); err != nil {
		return fmt.Errorf("ditto: %w (%s)", err, trimOutput(out))
	}
	return nil
}

// --- signature verification ---

// verifyBundle decides whether the extracted bundle may replace the running one.
//
// The team identifier of the running bundle is read the same way as the new
// one's rather than compared against a constant: a hardcoded ID would have to be
// updated by hand the day the signing identity changes, and the failure mode of
// forgetting is that every install is rejected — or, if someone "fixes" it by
// deleting the check, that none are.
//
// The order matters. The cheap structural check comes first, the identity check
// before Gatekeeper, and nothing here touches /Applications, so any failure
// leaves the installed app exactly as it was.
func verifyBundle(newApp, currentApp string) error {
	if out, err := runCommand(codesignPath, "--verify", "--strict", "--deep", newApp); err != nil {
		return fmt.Errorf("downloaded bundle failed signature verification: %w (%s)", err, trimOutput(out))
	}

	newTeam, err := teamIdentifier(newApp)
	if err != nil {
		return fmt.Errorf("downloaded bundle: %w", err)
	}
	currentTeam, err := teamIdentifier(currentApp)
	if err != nil {
		return fmt.Errorf("running bundle: %w", err)
	}
	if newTeam != currentTeam {
		return fmt.Errorf("downloaded bundle is signed by team %s but this app is signed by %s; refusing to install", newTeam, currentTeam)
	}

	if out, err := runCommand(spctlPath, "-a", "-t", "exec", newApp); err != nil {
		return fmt.Errorf("Gatekeeper rejected the downloaded bundle: %w (%s)", err, trimOutput(out))
	}
	return nil
}

func teamIdentifier(app string) (string, error) {
	out, err := runCommand(codesignPath, "-dv", "--verbose=2", app)
	if err != nil {
		return "", fmt.Errorf("cannot read the signature of %s: %w (%s)", app, err, trimOutput(out))
	}
	return parseTeamIdentifier(string(out))
}

// parseTeamIdentifier pulls the team out of a `codesign -dv --verbose=2` report,
// which is printed to stderr as a block of key=value lines.
//
// "not set" — what an ad-hoc or unsigned bundle reports — is an error rather
// than a value. Treating it as one would make two unsigned bundles compare
// equal, which is precisely the case the comparison exists to catch.
func parseTeamIdentifier(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		id, ok := strings.CutPrefix(strings.TrimSpace(line), "TeamIdentifier=")
		if !ok {
			continue
		}
		id = strings.TrimSpace(id)
		if id == "" || strings.EqualFold(id, "not set") {
			return "", fmt.Errorf("has no team identifier (%q): ad-hoc and unsigned builds cannot be matched", id)
		}
		return id, nil
	}
	return "", errors.New("codesign reported no TeamIdentifier")
}

// trimOutput folds a tool's output into something that fits on one log line.
func trimOutput(out []byte) string {
	s := strings.TrimSpace(string(out))
	const max = 512
	if len(s) > max {
		s = s[:max] + "…"
	}
	return strings.ReplaceAll(s, "\n", "; ")
}

// --- paths and versions ---

func currentAppPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	// Walk up to find the .app directory
	dir := exe
	for i := 0; i < 5; i++ {
		dir = filepath.Dir(dir)
		if strings.HasSuffix(dir, ".app") {
			return dir, nil
		}
	}
	return "", fmt.Errorf("not running inside a .app bundle")
}

func findApp(dir string) (string, error) {
	var appPath string
	// Discarded deliberately: the callback swallows every per-entry error and the
	// only thing it ever returns is SkipAll, which Walk reports as success — so
	// this return value cannot be anything but nil. Whether a bundle was found is
	// the appPath test below, and that is the answer callers need.
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && strings.HasSuffix(path, ".app") {
			appPath = path
			return filepath.SkipAll
		}
		return nil
	})
	if appPath == "" {
		return "", fmt.Errorf("no .app bundle found")
	}
	return appPath, nil
}

func isNewer(latest, current string) bool {
	lp := splitVersion(latest)
	cp := splitVersion(current)
	for i := 0; i < 3; i++ {
		if lp[i] > cp[i] {
			return true
		}
		if lp[i] < cp[i] {
			return false
		}
	}
	return false
}

func splitVersion(v string) [3]int {
	var parts [3]int
	segments := strings.SplitN(v, ".", 3)
	for i, s := range segments {
		if i >= 3 {
			break
		}
		fmt.Sscanf(s, "%d", &parts[i])
	}
	return parts
}
