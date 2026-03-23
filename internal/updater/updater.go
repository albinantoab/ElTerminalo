package updater

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	githubRepo    = "albinantoab/ElTerminalo"
	apiURL        = "https://api.github.com/repos/" + githubRepo + "/releases/latest"
	checkTimeout  = 5 * time.Second
	downloadTimeout = 5 * time.Minute
)

// UpdateInfo contains information about an available update.
type UpdateInfo struct {
	Available  bool   `json:"available"`
	CurrentVer string `json:"currentVersion"`
	LatestVer  string `json:"latestVersion"`
	URL        string `json:"url"`
}

// CleanupStaleBackup removes any leftover .app.backup from a prior update.
// Safe to call on every startup.
func CleanupStaleBackup() {
	appPath, err := currentAppPath()
	if err != nil {
		return
	}
	backupPath := appPath + ".backup"
	os.RemoveAll(backupPath)
}

// Check queries GitHub for the latest release and compares with the current version.
func Check(currentVersion string) UpdateInfo {
	info := UpdateInfo{CurrentVer: currentVersion}

	client := &http.Client{Timeout: checkTimeout}
	resp, err := client.Get(apiURL)
	if err != nil {
		return info
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return info
	}

	var release ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return info
	}

	latest := strings.TrimPrefix(release.TagName, "v")
	info.LatestVer = latest
	info.URL = release.HTMLURL

	if latest != "" && latest != currentVersion && isNewer(latest, currentVersion) {
		info.Available = true
	}

	return info
}

// ApplyUpdate downloads the latest release zip, extracts the .app,
// replaces the currently running app bundle, and relaunches.
func ApplyUpdate() error {
	// 1. Find the current .app bundle path
	appPath, err := currentAppPath()
	if err != nil {
		return fmt.Errorf("cannot locate app bundle: %w", err)
	}

	// 2. Fetch the latest release metadata to find the zip asset URL
	client := &http.Client{Timeout: checkTimeout}
	resp, err := client.Get(apiURL)
	if err != nil {
		return fmt.Errorf("failed to check releases: %w", err)
	}
	defer resp.Body.Close()

	var release ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return fmt.Errorf("failed to parse release: %w", err)
	}

	// Find the zip asset for our architecture
	zipURL := findZipAsset(release.Assets)
	if zipURL == "" {
		return fmt.Errorf("no compatible zip asset found in release")
	}

	// 3. Download the zip to a temp file
	tmpDir, err := os.MkdirTemp("", "elterminalo-update-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	zipPath := filepath.Join(tmpDir, "update.zip")
	if err := downloadFile(zipURL, zipPath); err != nil {
		return fmt.Errorf("download failed: %w", err)
	}

	// 4. Extract the zip
	extractDir := filepath.Join(tmpDir, "extracted")
	if err := unzip(zipPath, extractDir); err != nil {
		return fmt.Errorf("extraction failed: %w", err)
	}

	// 5. Find the .app in the extracted contents
	newAppPath, err := findApp(extractDir)
	if err != nil {
		return fmt.Errorf("no .app found in update: %w", err)
	}

	// 6. Replace the current app
	backupPath := appPath + ".backup"
	os.RemoveAll(backupPath)

	if err := os.Rename(appPath, backupPath); err != nil {
		return fmt.Errorf("failed to backup current app: %w", err)
	}

	if err := copyDir(newAppPath, appPath); err != nil {
		// Restore backup on failure
		os.RemoveAll(appPath)
		os.Rename(backupPath, appPath)
		return fmt.Errorf("failed to install update: %w", err)
	}

	os.RemoveAll(backupPath)

	// 7. Relaunch
	cmd := exec.Command("open", "-n", appPath)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to relaunch: %w", err)
	}

	// Clean up temp directory explicitly (defer won't run after os.Exit)
	os.RemoveAll(tmpDir)

	// Exit current process
	os.Exit(0)
	return nil
}

// --- helpers ---

type ghRelease struct {
	TagName string    `json:"tag_name"`
	HTMLURL string    `json:"html_url"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func findZipAsset(assets []ghAsset) string {
	arch := runtime.GOARCH
	for _, a := range assets {
		name := strings.ToLower(a.Name)
		if strings.HasSuffix(name, ".zip") && strings.Contains(name, "macos") {
			if strings.Contains(name, arch) || strings.Contains(name, "universal") {
				return a.BrowserDownloadURL
			}
		}
	}
	// Fallback: any zip with "macos" in the name
	for _, a := range assets {
		name := strings.ToLower(a.Name)
		if strings.HasSuffix(name, ".zip") && strings.Contains(name, "macos") {
			return a.BrowserDownloadURL
		}
	}
	return ""
}

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

func downloadFile(url, dest string) error {
	client := &http.Client{Timeout: downloadTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

func unzip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		target := filepath.Join(dest, f.Name)

		// Prevent zip slip
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(dest)+string(os.PathSeparator)) {
			continue
		}

		if f.FileInfo().IsDir() {
			os.MkdirAll(target, f.Mode())
			continue
		}

		os.MkdirAll(filepath.Dir(target), 0755)

		outFile, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return err
		}

		_, err = io.Copy(outFile, rc)
		rc.Close()
		outFile.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func findApp(dir string) (string, error) {
	var appPath string
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
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

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		// Use Lstat to detect symlinks (Walk dereferences them)
		linfo, err := os.Lstat(path)
		if err != nil {
			return err
		}

		if linfo.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
			// Skip descending into symlinked directories
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if linfo.IsDir() {
			return os.MkdirAll(target, linfo.Mode())
		}

		return copyFile(path, target, linfo.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
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
