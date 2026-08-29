package updater

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// --- checksum parsing and verification ---

func TestParseChecksumsReadsReleaseFormat(t *testing.T) {
	// Exactly what scripts/release.sh writes: `shasum -a 256 "$DMG" "$ZIP"`,
	// two spaces between hash and name.
	const file = "" +
		"1b4f0e9851971998e732078544c96b36c3d01cedf7caa332359d6f1d83567014  ElTerminalo-1.0.2-macos-arm64.dmg\n" +
		"60303ae22b998861bce3b28f33eec1be758a213c86c93c076dbe9f558c11c752  ElTerminalo-1.0.2-macos-arm64.zip\n"

	sums := parseChecksums([]byte(file))
	if len(sums) != 2 {
		t.Fatalf("parsed %d entries, want 2: %v", len(sums), sums)
	}
	want := "60303ae22b998861bce3b28f33eec1be758a213c86c93c076dbe9f558c11c752"
	if got := sums["ElTerminalo-1.0.2-macos-arm64.zip"]; got != want {
		t.Fatalf("zip hash = %q, want %q", got, want)
	}
}

func TestParseChecksumsSkipsJunkLines(t *testing.T) {
	const file = "" +
		"# a comment\n" +
		"\n" +
		"not-a-hash  something.zip\n" +
		"1b4f0e9851971998e732078544c96b36c3d01cedf7caa332359d6f1d83567014\n" + // no filename
		"1b4f0e9851971998e732078544c96b36c3d01cedf7caa332359d6f1d83567014  \n" + // blank filename
		"1B4F0E9851971998E732078544C96B36C3D01CEDF7CAA332359D6F1D83567014 *binary mode.zip\n" +
		"1b4f0e9851971998e732078544c96b36c3d01cedf7caa332359d6f1d83567014  name with spaces.zip\n"

	sums := parseChecksums([]byte(file))
	if len(sums) != 2 {
		t.Fatalf("parsed %d entries, want 2: %v", len(sums), sums)
	}
	// shasum's binary-mode " *" separator, and an uppercase hash, both normalise.
	if got := sums["binary mode.zip"]; got != "1b4f0e9851971998e732078544c96b36c3d01cedf7caa332359d6f1d83567014" {
		t.Fatalf("binary-mode entry = %q", got)
	}
	if _, ok := sums["name with spaces.zip"]; !ok {
		t.Fatalf("a filename containing spaces was truncated: %v", sums)
	}
}

func TestVerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "update.zip")
	payload := []byte("pretend this is an app bundle\n")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	sum := sha256.Sum256(payload)
	correct := hex.EncodeToString(sum[:])

	t.Run("match", func(t *testing.T) {
		if err := verifyChecksum(path, map[string]string{"update.zip": correct}, "update.zip"); err != nil {
			t.Fatalf("verifyChecksum on a matching file: %v", err)
		}
	})

	t.Run("uppercase hash still matches", func(t *testing.T) {
		up := map[string]string{"update.zip": strings.ToUpper(correct)}
		if err := verifyChecksum(path, up, "update.zip"); err != nil {
			t.Fatalf("verifyChecksum with an uppercase listing: %v", err)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		wrong := map[string]string{"update.zip": strings.Repeat("0", 64)}
		err := verifyChecksum(path, wrong, "update.zip")
		if err == nil {
			t.Fatal("verifyChecksum accepted a file whose hash does not match")
		}
		if !strings.Contains(err.Error(), correct) {
			t.Fatalf("error should name the hash actually computed, got: %v", err)
		}
	})

	t.Run("no entry for the file", func(t *testing.T) {
		// An unlisted artifact is exactly as unverified as a mismatched one.
		if err := verifyChecksum(path, map[string]string{"other.zip": correct}, "update.zip"); err == nil {
			t.Fatal("verifyChecksum accepted a file the checksums do not list")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		missing := filepath.Join(dir, "gone.zip")
		if err := verifyChecksum(missing, map[string]string{"gone.zip": correct}, "gone.zip"); err == nil {
			t.Fatal("verifyChecksum accepted a file that does not exist")
		}
	})
}

// --- asset selection ---

func TestFindZipAsset(t *testing.T) {
	asset := func(name string) ghAsset {
		return ghAsset{Name: name, BrowserDownloadURL: "https://example.invalid/" + name}
	}
	arm := asset("ElTerminalo-1.0.2-macos-arm64.zip")
	amd := asset("ElTerminalo-1.0.2-macos-amd64.zip")
	universal := asset("ElTerminalo-1.0.2-macos-universal.zip")
	dmg := asset("ElTerminalo-1.0.2-macos-arm64.dmg")
	sums := asset(checksumsAssetName)
	arm64e := asset("ElTerminalo-1.0.2-macos-arm64e.zip")
	// The reason the platform token is required as well: these carry the same
	// architecture token as the macOS build, and an architecture-only match
	// picks whichever the release lists first. They download, they pass the
	// checksum, and the run only fails at findApp.
	linuxAMD := asset("ElTerminalo-1.0.2-linux-amd64.zip")
	linuxARM := asset("ElTerminalo-1.0.2-linux-arm64.zip")
	windowsAMD := asset("ElTerminalo-1.0.2-windows-amd64.zip")
	linuxUniversal := asset("ElTerminalo-1.0.2-linux-universal.zip")

	cases := []struct {
		name    string
		goarch  string
		assets  []ghAsset
		want    string
		wantErr bool
	}{
		{"arm64 picks the arm64 zip", "arm64", []ghAsset{dmg, amd, arm, sums}, arm.Name, false},
		{"amd64 picks the amd64 zip", "amd64", []ghAsset{dmg, arm, amd, sums}, amd.Name, false},
		{"universal is accepted on arm64", "arm64", []ghAsset{universal, sums}, universal.Name, false},
		{"universal is accepted on amd64", "amd64", []ghAsset{universal, sums}, universal.Name, false},
		{"exact architecture beats universal", "arm64", []ghAsset{universal, arm}, arm.Name, false},
		{"no fallback to the other architecture", "arm64", []ghAsset{amd, dmg, sums}, "", true},
		{"a dmg is not an installable asset", "arm64", []ghAsset{dmg, sums}, "", true},
		{"arm64e is a different ABI, not arm64", "arm64", []ghAsset{arm64e, sums}, "", true},
		{"empty release", "arm64", nil, "", true},
		{"unsupported architecture", "riscv64", []ghAsset{arm, amd, universal}, "", true},

		{"a linux zip for this architecture is not installable", "amd64", []ghAsset{linuxAMD, sums}, "", true},
		{"a windows zip for this architecture is not installable", "amd64", []ghAsset{windowsAMD, sums}, "", true},
		{"a linux zip for this architecture is not installable (arm64)", "arm64", []ghAsset{linuxARM, sums}, "", true},
		{"a linux universal zip is not installable", "arm64", []ghAsset{linuxUniversal, sums}, "", true},
		{
			"the macOS zip is picked out of a multi-platform release",
			"amd64",
			// Listed before the macOS one on purpose: first match wins within a
			// pass, so an architecture-only test would return linuxAMD here.
			[]ghAsset{linuxAMD, windowsAMD, dmg, amd, sums},
			amd.Name,
			false,
		},
		{
			"a multi-platform release with no macOS build for this architecture is refused",
			"amd64",
			[]ghAsset{linuxAMD, windowsAMD, arm, sums},
			"",
			true,
		},
		{
			"a linux build never stands in for a missing macOS universal one",
			"arm64",
			[]ghAsset{linuxUniversal, linuxARM, sums},
			"",
			true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := findZipAsset(tc.assets, tc.goarch)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("findZipAsset picked %q, want an error", got.Name)
				}
				return
			}
			if err != nil {
				t.Fatalf("findZipAsset: %v", err)
			}
			if got.Name != tc.want {
				t.Fatalf("findZipAsset picked %q, want %q", got.Name, tc.want)
			}
		})
	}
}

func TestFindChecksumsAsset(t *testing.T) {
	assets := []ghAsset{
		{Name: "ElTerminalo-1.0.2-macos-arm64.zip"},
		{Name: "CHECKSUMS-SHA256.TXT", BrowserDownloadURL: "https://example.invalid/sums"},
	}
	got, err := findChecksumsAsset(assets)
	if err != nil {
		t.Fatalf("findChecksumsAsset: %v", err)
	}
	if got.BrowserDownloadURL != "https://example.invalid/sums" {
		t.Fatalf("picked %+v", got)
	}

	if _, err := findChecksumsAsset(assets[:1]); err == nil {
		t.Fatal("a release with no checksums file must be refused")
	}
}

// --- signature verification, with an injected runner ---

type scriptedReply struct {
	out string
	err error
}

// The two kinds of failure a runner can report, which the package has to keep
// apart: a tool that ran and said no, and a tool that never ran.

// verdictErr is what exec produces when a tool ran to completion and exited
// non-zero — an *exec.ExitError wrapping a real wait status. os.ProcessState has
// no exported fields, so an honest one has to come from an honest process; a
// shell that exits 1 is the cheapest, and nothing here touches the network.
func verdictErr(t *testing.T) error {
	t.Helper()
	err := exec.Command("/bin/sh", "-c", "exit 1").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("a command that exits 1 produced %T (%v), want an *exec.ExitError", err, err)
	}
	return exitErr
}

// execFailures are the ways codesign can fail to run at all. None of them is an
// *exec.ExitError, and none of them says anything about the bundle: reading one
// as a verdict is what used to downgrade the app and delete the newer bundle.
func execFailures() []struct {
	name string
	err  error
} {
	return []struct {
		name string
		err  error
	}{
		// The one that actually happens: many processes are starting at login and
		// the fork cannot be satisfied.
		{"fork failed under load", &exec.Error{Name: codesignPath, Err: errors.New("resource temporarily unavailable")}},
		{"tool missing", &exec.Error{Name: codesignPath, Err: exec.ErrNotFound}},
		{"not executable", &fs.PathError{Op: "fork/exec", Path: codesignPath, Err: fs.ErrPermission}},
		// Wrapped rather than returned bare, because errors.As has to see through
		// whatever a future caller puts around it.
		{"wrapped I/O error", fmt.Errorf("reading codesign output: %w", io.ErrUnexpectedEOF)},
	}
}

// TestToolRejectedSeparatesAVerdictFromAFailureToRun pins the distinction the
// startup path hangs on.
func TestToolRejectedSeparatesAVerdictFromAFailureToRun(t *testing.T) {
	if toolRejected(nil) {
		t.Fatal("a nil error is not a rejection")
	}
	verdict := verdictErr(t)
	if !toolRejected(verdict) {
		t.Fatalf("%T (%v) is codesign's verdict and must read as one", verdict, verdict)
	}
	if !toolRejected(fmt.Errorf("codesign --verify: %w", verdict)) {
		t.Fatal("a wrapped *exec.ExitError must still read as a verdict")
	}
	for _, tc := range execFailures() {
		if toolRejected(tc.err) {
			t.Errorf("%s (%T) must not read as a verdict about the bundle", tc.name, tc.err)
		}
	}
}

// scriptedRunner records every command it is asked to run and answers from a
// table keyed by the whole command line, so a test can assert both what ran and
// in what order.
type scriptedRunner struct {
	calls   []string
	replies map[string]scriptedReply
}

func (r *scriptedRunner) run(name string, args ...string) ([]byte, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, cmd)
	reply, ok := r.replies[cmd]
	if !ok {
		return nil, fmt.Errorf("test did not script: %s", cmd)
	}
	return []byte(reply.out), reply.err
}

func installRunner(t *testing.T, r *scriptedRunner) {
	t.Helper()
	prev := runCommand
	t.Cleanup(func() { runCommand = prev })
	runCommand = r.run
}

const (
	testNewApp = "/tmp/elterminalo-update/ElTerminalo.app"
	testCurApp = "/Applications/ElTerminalo.app"
)

// verifySequence is the exact order verifyBundle must run its checks in, and
// the whole of what it is allowed to run.
//
// The absolute paths are the assertion, not incidental formatting: a $PATH
// lookup here is what lets a fake "codesign" earlier in $PATH exit 0 and print
// a matching TeamIdentifier=, which turns all three gates into no-ops.
func verifySequence() []string {
	return []string{
		codesignPath + " --verify --strict --deep " + testNewApp,
		codesignPath + " -dv --verbose=2 " + testNewApp,
		codesignPath + " -dv --verbose=2 " + testCurApp,
		spctlPath + " -a -t exec " + testNewApp,
	}
}

func TestToolsAreResolvedByAbsolutePath(t *testing.T) {
	for _, tool := range []string{dittoPath, codesignPath, spctlPath, OpenPath} {
		if !strings.HasPrefix(tool, "/") {
			t.Errorf("%q is not an absolute path; a $PATH lookup makes every gate in this package defeatable", tool)
		}
	}
}

func signedReport(team string) string {
	return "Executable=/x\nIdentifier=com.elterminalo\nTeamIdentifier=" + team + "\nSealed Resources version=2\n"
}

// happyReplies scripts every verification command as succeeding, both bundles
// signed by the same team.
func happyReplies() map[string]scriptedReply {
	seq := verifySequence()
	return map[string]scriptedReply{
		seq[0]: {},
		seq[1]: {out: signedReport("Z4D9F3U5MP")},
		seq[2]: {out: signedReport("Z4D9F3U5MP")},
		seq[3]: {out: testNewApp + ": accepted\n"},
	}
}

func TestVerifyBundleRunsChecksInOrder(t *testing.T) {
	r := &scriptedRunner{replies: happyReplies()}
	installRunner(t, r)

	if err := verifyBundle(testNewApp, testCurApp); err != nil {
		t.Fatalf("verifyBundle on a well-signed bundle: %v", err)
	}
	if !slices.Equal(r.calls, verifySequence()) {
		t.Fatalf("verifyBundle ran\n  %v\nwant\n  %v", r.calls, verifySequence())
	}
}

// Every check must be able to stop the install on its own, and must stop it
// before the later ones run — the point is that nothing reaches /Applications.
func TestVerifyBundleStopsAtTheFirstFailure(t *testing.T) {
	seq := verifySequence()
	for i, failing := range seq {
		t.Run(fmt.Sprintf("step %d: %s", i+1, failing), func(t *testing.T) {
			replies := happyReplies()
			replies[failing] = scriptedReply{out: "rejected: bad thing", err: errors.New("exit status 1")}
			r := &scriptedRunner{replies: replies}
			installRunner(t, r)

			err := verifyBundle(testNewApp, testCurApp)
			if err == nil {
				t.Fatal("verifyBundle accepted a bundle that failed a check")
			}
			if len(r.calls) != i+1 {
				t.Fatalf("ran %v after step %d failed; nothing past the failure may run", r.calls, i+1)
			}
			if !slices.Equal(r.calls, seq[:i+1]) {
				t.Fatalf("ran\n  %v\nwant\n  %v", r.calls, seq[:i+1])
			}
		})
	}
}

func TestVerifyBundleRejectsDifferentTeam(t *testing.T) {
	seq := verifySequence()
	replies := happyReplies()
	replies[seq[1]] = scriptedReply{out: signedReport("ATTACKER99")}
	r := &scriptedRunner{replies: replies}
	installRunner(t, r)

	err := verifyBundle(testNewApp, testCurApp)
	if err == nil {
		t.Fatal("verifyBundle accepted a bundle signed by a different team")
	}
	if !strings.Contains(err.Error(), "ATTACKER99") || !strings.Contains(err.Error(), "Z4D9F3U5MP") {
		t.Fatalf("error should name both teams, got: %v", err)
	}
	// Gatekeeper is never consulted once the identity is wrong.
	if len(r.calls) != 3 {
		t.Fatalf("ran %v, want to stop after reading both team identifiers", r.calls)
	}
}

func TestParseTeamIdentifier(t *testing.T) {
	cases := []struct {
		name    string
		report  string
		want    string
		wantErr bool
	}{
		{"normal report", signedReport("Z4D9F3U5MP"), "Z4D9F3U5MP", false},
		{"indented line", "  TeamIdentifier=ABCDEFGHIJ  \n", "ABCDEFGHIJ", false},
		{"ad-hoc", "Identifier=x\nTeamIdentifier=not set\n", "", true},
		{"empty value", "TeamIdentifier=\n", "", true},
		{"absent", "Identifier=x\nSealed Resources version=2\n", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTeamIdentifier(tc.report)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseTeamIdentifier(%q) = %q, want an error", tc.report, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTeamIdentifier: %v", err)
			}
			if got != tc.want {
				t.Fatalf("parseTeamIdentifier = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- extraction with the real ditto ---

func TestExtractZipPreservesSymlinks(t *testing.T) {
	if _, err := os.Stat(dittoPath); err != nil {
		t.Skip("ditto is not available on this platform")
	}

	staging := t.TempDir()
	app := filepath.Join(staging, "ElTerminalo.app")
	macOS := filepath.Join(app, "Contents", "MacOS")
	if err := os.MkdirAll(macOS, 0o755); err != nil {
		t.Fatalf("staging the bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(macOS, "ElTerminalo"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing the executable: %v", err)
	}
	// The reason archive/zip had to go: it writes this out as a regular file
	// containing the text "MacOS/ElTerminalo".
	link := filepath.Join(app, "Contents", "Current")
	if err := os.Symlink("MacOS/ElTerminalo", link); err != nil {
		t.Fatalf("creating the symlink: %v", err)
	}

	zipPath := filepath.Join(t.TempDir(), "update.zip")
	if out, err := exec.Command(dittoPath, "-c", "-k", "--sequesterRsrc", "--keepParent", app, zipPath).CombinedOutput(); err != nil {
		t.Fatalf("ditto -c -k: %v (%s)", err, out)
	}

	dest := filepath.Join(t.TempDir(), "extracted")
	if err := extractZip(zipPath, dest); err != nil {
		t.Fatalf("extractZip: %v", err)
	}

	got, err := findApp(dest)
	if err != nil {
		t.Fatalf("findApp in the extracted tree: %v", err)
	}
	if filepath.Base(got) != "ElTerminalo.app" {
		t.Fatalf("findApp returned %s", got)
	}

	info, err := os.Lstat(filepath.Join(got, "Contents", "Current"))
	if err != nil {
		t.Fatalf("the symlink did not survive extraction: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("Contents/Current came out as a %s, not a symlink", info.Mode())
	}
	target, err := os.Readlink(filepath.Join(got, "Contents", "Current"))
	if err != nil || target != "MacOS/ElTerminalo" {
		t.Fatalf("symlink points at %q (err %v), want %q", target, err, "MacOS/ElTerminalo")
	}

	body, err := os.ReadFile(filepath.Join(got, "Contents", "MacOS", "ElTerminalo"))
	if err != nil {
		t.Fatalf("reading the extracted executable: %v", err)
	}
	if string(body) != "#!/bin/sh\nexit 0\n" {
		t.Fatalf("extracted executable holds %q", body)
	}
}

func TestExtractZipReportsDittoFailure(t *testing.T) {
	if _, err := os.Stat(dittoPath); err != nil {
		t.Skip("ditto is not available on this platform")
	}
	notAZip := filepath.Join(t.TempDir(), "broken.zip")
	if err := os.WriteFile(notAZip, []byte("this is not a zip"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	if err := extractZip(notAZip, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("extractZip accepted a file that is not a zip")
	}
}

// --- concurrency guard ---

// A second click on Update while the first download is running must be refused
// outright: it must not reach the network, and above all must not start a
// second swap of /Applications.
func TestApplyUpdateRefusesConcurrentRun(t *testing.T) {
	if !updateInFlight.CompareAndSwap(false, true) {
		t.Fatal("updateInFlight was already set before the test started")
	}
	t.Cleanup(func() { updateInFlight.Store(false) })

	// Any command reaching the runner would mean the guard let execution past
	// its first statement.
	installRunner(t, &scriptedRunner{replies: map[string]scriptedReply{}})

	err := ApplyUpdate()
	if !errors.Is(err, errUpdateInProgress) {
		t.Fatalf("ApplyUpdate returned %v, want %v", err, errUpdateInProgress)
	}
}

// --- URL validation ---

func TestValidateAssetURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		ok   bool
	}{
		{"a real release asset URL", "https://github.com/albinantoab/ElTerminalo/releases/download/v1.0.2/ElTerminalo-1.0.2-macos-arm64.zip", true},
		{"the CDN assets redirect to", "https://objects.githubusercontent.com/github-production-release-asset/1/2", true},
		{"raw.githubusercontent.com", "https://raw.githubusercontent.com/a/b/c", true},
		{"the api host", "https://api.github.com/repos/x/y/releases/latest", true},
		{"uppercase host", "https://GitHub.COM/a/b.zip", true},
		{"an explicit https port", "https://github.com:443/a/b.zip", true},

		{"plain http", "http://github.com/a/b.zip", false},
		{"file", "file:///Applications/evil.zip", false},
		{"ftp", "ftp://github.com/a/b.zip", false},
		{"no scheme at all", "github.com/a/b.zip", false},
		{"another host entirely", "https://evil.example/a/b.zip", false},
		// The suffix test is on a label boundary, so none of these pass by
		// merely ending in the right characters.
		{"a lookalike suffix", "https://notgithub.com/a/b.zip", false},
		{"the host as a prefix of another", "https://github.com.evil.example/a/b.zip", false},
		{"the host inside the path", "https://evil.example/github.com/a.zip", false},
		{"the host in userinfo", "https://github.com@evil.example/a.zip", false},
		{"an IP literal", "https://93.184.216.34/a.zip", false},
		{"an IPv6 literal", "https://[::1]/a.zip", false},
		{"localhost", "https://localhost:8080/a.zip", false},
		{"empty", "", false},
		{"not a URL", "://::", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAssetURL(tc.url)
			if tc.ok && err != nil {
				t.Fatalf("validateAssetURL(%q) = %v, want it accepted", tc.url, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("validateAssetURL(%q) accepted a URL it must refuse", tc.url)
			}
		})
	}
}

func TestHTTPClientRefusesADowngradingRedirect(t *testing.T) {
	check := newHTTPClient(checkTimeout).CheckRedirect
	if check == nil {
		t.Fatal("the client follows redirects with no policy at all")
	}

	must := func(rawURL string, wantErr bool) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatalf("building the request: %v", err)
		}
		err = check(req, nil)
		if wantErr && err == nil {
			t.Fatalf("a redirect to %s was allowed", rawURL)
		}
		if !wantErr && err != nil {
			t.Fatalf("a redirect to %s was refused: %v", rawURL, err)
		}
	}
	// The point: the scheme check on the original URL is worthless if a 302 can
	// move the download onto plain http.
	must("http://objects.githubusercontent.com/a.zip", true)
	must("https://objects.githubusercontent.com/a.zip", false)

	// Setting CheckRedirect replaces http.Client's own loop guard, so the
	// replacement has to carry one.
	req, err := http.NewRequest(http.MethodGet, "https://github.com/a.zip", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if err := check(req, make([]*http.Request, 10)); err == nil {
		t.Fatal("the redirect chain is unbounded")
	}
}

// --- installing: swapBundle and cleanupStaleBackup ---

// quietLog sends the package's log lines to a buffer for one test. These two
// paths log every branch on purpose, which is what makes a failed install
// diagnosable, and which would otherwise bury the test output.
func quietLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	prevOut, prevFlags := log.Writer(), log.Flags()
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	return &buf
}

// writeBundle stages a directory tree that stands in for an .app. marker is the
// content of its executable, so a test can tell which bundle it is looking at.
func writeBundle(t *testing.T, path, marker string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatalf("staging %s: %v", path, err)
	}
	if err := os.WriteFile(filepath.Join(path, "Contents", "MacOS", "ElTerminalo"), []byte(marker), 0o755); err != nil {
		t.Fatalf("staging %s: %v", path, err)
	}
}

// bundleMarker reports which bundle is at path, or "" when there is none.
func bundleMarker(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(path, "Contents", "MacOS", "ElTerminalo"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

// installRunnerFunc installs an arbitrary runner for one test.
func installRunnerFunc(t *testing.T, fn commandRunner) {
	t.Helper()
	prev := runCommand
	t.Cleanup(func() { runCommand = prev })
	runCommand = fn
}

// installRenamer replaces os.Rename for one test. Every other test in here
// drives swapBundle over a real directory; this exists for the one branch a real
// filesystem cannot produce — see TestSwapBundleRemovesTheStagedCopyWhenTheRestoreAlsoFails.
func installRenamer(t *testing.T, fn func(oldpath, newpath string) error) {
	t.Helper()
	prev := osRename
	t.Cleanup(func() { osRename = prev })
	osRename = fn
}

// swapRunner stands in for the real tools during a swap. ditto is answered by
// actually copying the tree, so the renames that follow it operate on a real
// filesystem in the state production would put them in.
type swapRunner struct {
	calls []string
	// dittoErr makes ditto fail.
	dittoErr error
	// dittoLies makes ditto exit 0 without producing anything, which is what
	// exercises the recovery path in the second rename.
	dittoLies bool
	// duringDitto runs while ditto is "copying" — the window in which the old
	// code had already moved the installed app aside.
	duringDitto func()
}

func (r *swapRunner) run(name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	if name == dittoPath && len(args) == 2 {
		if r.duringDitto != nil {
			r.duringDitto()
		}
		if r.dittoErr != nil {
			return []byte("ditto: some copy error"), r.dittoErr
		}
		if r.dittoLies {
			return nil, nil
		}
		return nil, os.CopyFS(args[1], os.DirFS(args[0]))
	}
	return nil, fmt.Errorf("test did not script: %s", strings.Join(append([]string{name}, args...), " "))
}

func TestSwapBundleCopiesBeforeItTouchesTheInstalledApp(t *testing.T) {
	quietLog(t)
	root := t.TempDir()
	appPath := filepath.Join(root, "ElTerminalo.app")
	newAppPath := filepath.Join(t.TempDir(), "ElTerminalo.app")
	writeBundle(t, appPath, "installed")
	writeBundle(t, newAppPath, "update")

	// The whole point of the ordering: while the slow copy runs, the app the
	// user would launch is still the complete old one. The previous version had
	// already renamed it away by this moment, so a force-quit here left nothing
	// at appPath and cleanupStaleBackup then deleted the only intact copy.
	var duringCopy string
	r := &swapRunner{duringDitto: func() { duringCopy = bundleMarker(t, appPath) }}
	installRunnerFunc(t, r.run)

	if err := swapBundle(newAppPath, appPath); err != nil {
		t.Fatalf("swapBundle: %v", err)
	}

	if duringCopy != "installed" {
		t.Fatalf("during the copy the installed app was %q, want it untouched and launchable", duringCopy)
	}
	if got := bundleMarker(t, appPath); got != "update" {
		t.Fatalf("after the swap %s holds %q, want the update", appPath, got)
	}
	// ditto's destination is the staging path, never the installed one.
	want := []string{dittoPath + " " + newAppPath + " " + appPath + ".new"}
	if !slices.Equal(r.calls, want) {
		t.Fatalf("swapBundle ran\n  %v\nwant\n  %v", r.calls, want)
	}
	for _, leftover := range []string{appPath + ".new", appPath + ".backup"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Fatalf("%s survived a successful swap (stat err = %v)", leftover, err)
		}
	}
}

func TestSwapBundleLeavesTheAppAloneWhenTheCopyFails(t *testing.T) {
	quietLog(t)
	root := t.TempDir()
	appPath := filepath.Join(root, "ElTerminalo.app")
	newAppPath := filepath.Join(t.TempDir(), "ElTerminalo.app")
	writeBundle(t, appPath, "installed")
	writeBundle(t, newAppPath, "update")

	r := &swapRunner{dittoErr: errors.New("exit status 1")}
	installRunnerFunc(t, r.run)

	if err := swapBundle(newAppPath, appPath); err == nil {
		t.Fatal("swapBundle reported success on a failed copy")
	}
	if got := bundleMarker(t, appPath); got != "installed" {
		t.Fatalf("%s holds %q after a failed copy, want the untouched original", appPath, got)
	}
	for _, leftover := range []string{appPath + ".new", appPath + ".backup"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Fatalf("%s was left behind by a failed swap (stat err = %v)", leftover, err)
		}
	}
}

func TestSwapBundleRestoresTheBackupWhenTheSecondRenameFails(t *testing.T) {
	quietLog(t)
	root := t.TempDir()
	appPath := filepath.Join(root, "ElTerminalo.app")
	newAppPath := filepath.Join(t.TempDir(), "ElTerminalo.app")
	writeBundle(t, appPath, "installed")
	writeBundle(t, newAppPath, "update")

	// A ditto that exits 0 without producing its destination: the app has been
	// renamed to .backup by then, so the only thing standing between the user
	// and no app at all is the rename back.
	installRunnerFunc(t, (&swapRunner{dittoLies: true}).run)

	if err := swapBundle(newAppPath, appPath); err == nil {
		t.Fatal("swapBundle reported success when the staged bundle never appeared")
	}
	if got := bundleMarker(t, appPath); got != "installed" {
		t.Fatalf("%s holds %q, want the previous app restored from the backup", appPath, got)
	}
	if _, err := os.Stat(appPath + ".backup"); !os.IsNotExist(err) {
		t.Fatalf("the backup was not consumed by the restore (stat err = %v)", err)
	}
}

// The worst branch: the staged bundle could not be moved into place and the
// backup could not be moved back either. There is no app at appPath, and the
// next start's cleanupStaleBackup is what puts the previous one back — from
// .backup, which is why that one has to survive. .new does not: nothing anywhere
// installs from it, so leaving it behind is several hundred megabytes of litter
// beside /Applications that no later run will ever look at.
//
// Both renames land in the same directory and are inverses of each other, so no
// real filesystem will fail the second after allowing the first; osRename is the
// seam that makes the branch reachable.
func TestSwapBundleRemovesTheStagedCopyWhenTheRestoreAlsoFails(t *testing.T) {
	quietLog(t)
	root := t.TempDir()
	appPath := filepath.Join(root, "ElTerminalo.app")
	newAppPath := filepath.Join(t.TempDir(), "ElTerminalo.app")
	writeBundle(t, appPath, "installed")
	writeBundle(t, newAppPath, "update")

	installRunnerFunc(t, (&swapRunner{}).run)
	// Only renames *into* the installed path fail, so the app → .backup move that
	// comes first still goes through and the failure starts where it has to.
	installRenamer(t, func(oldpath, newpath string) error {
		if newpath == appPath {
			return errors.New("simulated: rename onto the installed path failed")
		}
		return os.Rename(oldpath, newpath)
	})

	err := swapBundle(newAppPath, appPath)
	if err == nil {
		t.Fatal("swapBundle reported success when neither rename worked")
	}
	// The message is the only thing the user gets, and where the previous app is
	// now sitting is the one fact worth having.
	if !strings.Contains(err.Error(), appPath+".backup") {
		t.Fatalf("the error does not say where the previous app is: %v", err)
	}
	if got := bundleMarker(t, appPath+".backup"); got != "installed" {
		t.Fatalf("%s holds %q, want the previous app kept for the next start to restore", appPath+".backup", got)
	}
	if _, serr := os.Stat(appPath + ".new"); !os.IsNotExist(serr) {
		t.Fatalf("%s was left behind (stat err = %v)", appPath+".new", serr)
	}
}

// verifyCall is what cleanupStaleBackup runs to decide whether the installed
// bundle survived an interrupted install.
func verifyCall(appPath string) string {
	return codesignPath + " --verify --strict " + appPath
}

func TestCleanupStaleBackupRestoresWhenTheAppIsMissing(t *testing.T) {
	quietLog(t)
	root := t.TempDir()
	appPath := filepath.Join(root, "ElTerminalo.app")
	writeBundle(t, appPath+".backup", "previous")

	// A crash between swapBundle's two renames. Nothing may be run here — there
	// is no bundle to verify — and above all the backup must not be deleted.
	var calls []string
	installRunnerFunc(t, func(name string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(append([]string{name}, args...), " "))
		return nil, fmt.Errorf("nothing should run: %s", name)
	})

	cleanupStaleBackup(appPath)

	if got := bundleMarker(t, appPath); got != "previous" {
		t.Fatalf("%s holds %q, want the backup restored", appPath, got)
	}
	if _, err := os.Stat(appPath + ".backup"); !os.IsNotExist(err) {
		t.Fatalf("the backup is still there after being restored (stat err = %v)", err)
	}
	if len(calls) != 0 {
		t.Fatalf("ran %v; there is no installed bundle to verify", calls)
	}
}

func TestCleanupStaleBackupRestoresWhenCodesignRejectsTheApp(t *testing.T) {
	buf := quietLog(t)
	root := t.TempDir()
	appPath := filepath.Join(root, "ElTerminalo.app")
	// A half-copied bundle from the old swap order: present, so the previous
	// version of this function saw nothing wrong and deleted the good copy.
	writeBundle(t, appPath, "half-copied")
	writeBundle(t, appPath+".backup", "previous")

	verdict := verdictErr(t)
	installRunnerFunc(t, func(name string, args ...string) ([]byte, error) {
		if strings.Join(append([]string{name}, args...), " ") == verifyCall(appPath) {
			return []byte("code object is not signed at all"), verdict
		}
		return nil, fmt.Errorf("test did not script: %s", name)
	})

	cleanupStaleBackup(appPath)

	if got := bundleMarker(t, appPath); got != "previous" {
		t.Fatalf("%s holds %q, want the backup restored over the unverifiable bundle", appPath, got)
	}
	if _, err := os.Stat(appPath + ".backup"); !os.IsNotExist(err) {
		t.Fatalf("the backup was not consumed by the restore (stat err = %v)", err)
	}
	// The bundle that was moved aside is the newer of the two and survives. A
	// codesign verdict is a reason to stop running it, not a reason to make the
	// downgrade permanent, and the user has to be able to find it.
	brokenPath := appPath + ".broken"
	if got := bundleMarker(t, brokenPath); got != "half-copied" {
		t.Fatalf("%s holds %q, want the rejected bundle kept there", brokenPath, got)
	}
	if !strings.Contains(buf.String(), brokenPath) {
		t.Fatalf("the log never names where the rejected bundle went:\n%s", buf.String())
	}
}

// The failure this whole distinction exists for: codesign could not be run, so
// there is no verdict, so nothing may move. Treating this as a rejection put the
// user back on the older version and deleted the newer bundle — permanently, and
// on a machine whose only fault was being busy at login.
func TestCleanupStaleBackupLeavesBothBundlesWhenCodesignCannotRun(t *testing.T) {
	for _, tc := range execFailures() {
		t.Run(tc.name, func(t *testing.T) {
			buf := quietLog(t)
			root := t.TempDir()
			appPath := filepath.Join(root, "ElTerminalo.app")
			writeBundle(t, appPath, "installed")
			writeBundle(t, appPath+".backup", "previous")

			var calls []string
			installRunnerFunc(t, func(name string, args ...string) ([]byte, error) {
				calls = append(calls, strings.Join(append([]string{name}, args...), " "))
				return nil, tc.err
			})

			cleanupStaleBackup(appPath)

			if want := []string{verifyCall(appPath)}; !slices.Equal(calls, want) {
				t.Fatalf("ran\n  %v\nwant\n  %v", calls, want)
			}
			if got := bundleMarker(t, appPath); got != "installed" {
				t.Fatalf("%s holds %q, want the installed app left exactly as it was", appPath, got)
			}
			if got := bundleMarker(t, appPath+".backup"); got != "previous" {
				t.Fatalf("%s holds %q, want the backup left in place for the next start", appPath+".backup", got)
			}
			if _, err := os.Stat(appPath + ".broken"); !os.IsNotExist(err) {
				t.Fatalf("something was moved aside on a run that reached no verdict (stat err = %v)", err)
			}
			if !strings.Contains(buf.String(), "cannot verify installed app") {
				t.Fatalf("the log does not say the check never happened:\n%s", buf.String())
			}
		})
	}
}

// The other half of keeping .broken: it does not accumulate. Once the installed
// app verifies, the parked bundle is no longer the newest copy of anything and
// is finally safe to drop.
func TestCleanupStaleBackupRemovesAParkedBundleWhenTheAppVerifies(t *testing.T) {
	quietLog(t)
	root := t.TempDir()
	appPath := filepath.Join(root, "ElTerminalo.app")
	writeBundle(t, appPath, "installed")
	writeBundle(t, appPath+".backup", "previous")
	writeBundle(t, appPath+".broken", "rejected earlier")

	installRunnerFunc(t, func(name string, args ...string) ([]byte, error) {
		if strings.Join(append([]string{name}, args...), " ") == verifyCall(appPath) {
			return []byte(appPath + ": valid on disk\n"), nil
		}
		return nil, fmt.Errorf("test did not script: %s", name)
	})

	cleanupStaleBackup(appPath)

	if got := bundleMarker(t, appPath); got != "installed" {
		t.Fatalf("%s holds %q, want the installed app left alone", appPath, got)
	}
	for _, leftover := range []string{appPath + ".backup", appPath + ".broken"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Fatalf("%s survived a passing verification (stat err = %v)", leftover, err)
		}
	}
}

// A parked bundle is not cleaned up on a start that reached no verdict, only on
// one that verified: the two must not be collapsed into "the app is present".
func TestCleanupStaleBackupKeepsAParkedBundleWhenCodesignCannotRun(t *testing.T) {
	quietLog(t)
	root := t.TempDir()
	appPath := filepath.Join(root, "ElTerminalo.app")
	writeBundle(t, appPath, "installed")
	writeBundle(t, appPath+".backup", "previous")
	writeBundle(t, appPath+".broken", "rejected earlier")

	installRunnerFunc(t, func(name string, args ...string) ([]byte, error) {
		return nil, &exec.Error{Name: codesignPath, Err: errors.New("resource temporarily unavailable")}
	})

	cleanupStaleBackup(appPath)

	for path, want := range map[string]string{
		appPath:             "installed",
		appPath + ".backup": "previous",
		appPath + ".broken": "rejected earlier",
	} {
		if got := bundleMarker(t, path); got != want {
			t.Fatalf("%s holds %q, want %q", path, got, want)
		}
	}
}

// The interlock with ApplyUpdate: the cleanup runs from a goroutine at startup,
// and an install claiming the same latch means neither can be renaming the same
// three paths while the other is.
func TestCleanupStaleBackupSkipsWhileAnInstallIsInFlight(t *testing.T) {
	quietLog(t)
	root := t.TempDir()
	appPath := filepath.Join(root, "ElTerminalo.app")
	writeBundle(t, appPath, "installed")
	writeBundle(t, appPath+".backup", "previous")

	if !updateInFlight.CompareAndSwap(false, true) {
		t.Fatal("updateInFlight was already set before the test started")
	}
	t.Cleanup(func() { updateInFlight.Store(false) })

	var calls []string
	installRunnerFunc(t, func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name)
		return nil, nil
	})

	cleanupStaleBackupGuarded(appPath)

	if len(calls) != 0 {
		t.Fatalf("ran %v while an install holds the latch", calls)
	}
	if got := bundleMarker(t, appPath); got != "installed" {
		t.Fatalf("%s holds %q; the backup an install is in the middle of making must not be acted on", appPath, got)
	}
	if got := bundleMarker(t, appPath+".backup"); got != "previous" {
		t.Fatalf("%s holds %q", appPath+".backup", got)
	}
	// The skip must not release a latch it never took: ApplyUpdate's claim is
	// latched deliberately, and clearing it here would let a second install run.
	if !updateInFlight.Load() {
		t.Fatal("the skipped cleanup cleared the install latch on its way out")
	}
}

// And the claim it does take is always released — a cleanup that latched the
// flag would make every later ApplyUpdate return errUpdateInProgress forever.
func TestCleanupStaleBackupReleasesTheLatch(t *testing.T) {
	quietLog(t)
	root := t.TempDir()
	appPath := filepath.Join(root, "ElTerminalo.app")
	writeBundle(t, appPath, "installed")

	installRunnerFunc(t, func(name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("nothing should run: %s", name)
	})

	cleanupStaleBackupGuarded(appPath)

	if updateInFlight.Load() {
		updateInFlight.Store(false)
		t.Fatal("the cleanup held on to the install latch; every later update would be refused")
	}
}

func TestCleanupStaleBackupDeletesTheBackupWhenTheAppVerifies(t *testing.T) {
	quietLog(t)
	root := t.TempDir()
	appPath := filepath.Join(root, "ElTerminalo.app")
	writeBundle(t, appPath, "installed")
	writeBundle(t, appPath+".backup", "previous")

	installRunnerFunc(t, func(name string, args ...string) ([]byte, error) {
		if strings.Join(append([]string{name}, args...), " ") == verifyCall(appPath) {
			return []byte(appPath + ": valid on disk\n"), nil
		}
		return nil, fmt.Errorf("test did not script: %s", name)
	})

	cleanupStaleBackup(appPath)

	if got := bundleMarker(t, appPath); got != "installed" {
		t.Fatalf("%s holds %q, want the installed app left alone", appPath, got)
	}
	if _, err := os.Stat(appPath + ".backup"); !os.IsNotExist(err) {
		t.Fatalf("the stale backup survived a passing verification (stat err = %v)", err)
	}
}

func TestCleanupStaleBackupDoesNothingWithoutABackup(t *testing.T) {
	quietLog(t)
	root := t.TempDir()
	appPath := filepath.Join(root, "ElTerminalo.app")
	writeBundle(t, appPath, "installed")

	// The overwhelmingly common case, and it runs on every single startup: it
	// must not shell out to codesign at all.
	var calls []string
	installRunnerFunc(t, func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name)
		return nil, nil
	})

	cleanupStaleBackup(appPath)

	if len(calls) != 0 {
		t.Fatalf("ran %v with no backup present", calls)
	}
	if got := bundleMarker(t, appPath); got != "installed" {
		t.Fatalf("%s holds %q", appPath, got)
	}
}
