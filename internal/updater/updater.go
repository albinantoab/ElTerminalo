package updater

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	githubRepo = "albinanto/ElTerminalo"
	checkURL   = "https://api.github.com/repos/" + githubRepo + "/releases/latest"
	timeout    = 5 * time.Second
)

// UpdateInfo contains information about an available update.
type UpdateInfo struct {
	Available  bool   `json:"available"`
	CurrentVer string `json:"currentVersion"`
	LatestVer  string `json:"latestVersion"`
	URL        string `json:"url"`
}

// Check queries GitHub for the latest release and compares with the current version.
// Returns quickly and never blocks — errors are silently ignored.
func Check(currentVersion string) UpdateInfo {
	info := UpdateInfo{
		CurrentVer: currentVersion,
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(checkURL)
	if err != nil {
		return info
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return info
	}

	var release struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
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

// isNewer returns true if latest is a higher semver than current.
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
