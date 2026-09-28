package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Hocsman/Relayer/internal/version"
)

const (
	// latestReleaseURL answers with the newest release that is neither a
	// draft nor a prerelease.
	latestReleaseURL = "https://api.github.com/repos/Hocsman/Relayer/releases/latest"
	// releasePagePrefix is the only place an update link may lead: a
	// release of this repository, never a URL the response merely names.
	releasePagePrefix = "https://github.com/Hocsman/Relayer/releases/"
	// noUpdateCheckEnv turns the check off for every user of a machine, for
	// an administrator who does not want the application to reach GitHub.
	noUpdateCheckEnv   = "RELAYER_NO_UPDATE_CHECK"
	updateCheckTimeout = 10 * time.Second
	maxReleaseBody     = 1 << 20
)

// UpdateInfo tells the interface whether a newer release than the running one
// has been published. It carries no URL the interface opens itself: OpenUpdate
// opens the one the check validated.
type UpdateInfo struct {
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"`
	Available bool   `json:"available"`
	// Disabled is set when an administrator turned the check off with
	// RELAYER_NO_UPDATE_CHECK, or the build has no release version to compare.
	Disabled bool `json:"disabled"`
}

type updateChecker struct {
	client   *http.Client
	endpoint string
	current  func() string
	goos     string

	mu      sync.Mutex
	checked bool
	result  UpdateInfo
	openURL string
}

func newUpdateChecker() *updateChecker {
	return &updateChecker{
		client:   &http.Client{Timeout: updateCheckTimeout},
		endpoint: latestReleaseURL,
		current:  func() string { return version.Version },
		goos:     runtime.GOOS,
	}
}

// CheckForUpdate asks GitHub for the latest release once per launch. The
// interface calls it only while the user leaves the check on; it sends a
// plain request with no identifier beyond the running version in its
// User-Agent.
func (a *App) CheckForUpdate() (UpdateInfo, error) {
	return a.updates.check(context.Background())
}

// OpenUpdate opens, in the system browser, the download the last check found:
// the installer on Windows, the release page elsewhere.
func (a *App) OpenUpdate() error {
	target := a.updates.target()
	if target == "" {
		return errors.New("no update to open")
	}
	if a.ctx == nil || a.openURL == nil {
		return errors.New("the browser cannot be opened from here")
	}
	a.openURL(a.ctx, target)
	return nil
}

func (u *updateChecker) check(ctx context.Context) (UpdateInfo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.checked {
		return u.result, nil
	}
	current := strings.TrimPrefix(strings.TrimSpace(u.current()), "v")
	if disabled := os.Getenv(noUpdateCheckEnv); disabled != "" && disabled != "0" {
		return UpdateInfo{Current: current, Disabled: true}, nil
	}
	if _, ok := parseReleaseVersion(current); !ok {
		// A development build has no version to compare with a release.
		return UpdateInfo{Current: current, Disabled: true}, nil
	}

	release, err := u.fetch(ctx)
	if err != nil {
		return UpdateInfo{Current: current}, err
	}
	info, target := u.evaluate(current, release)
	u.checked, u.result, u.openURL = true, info, target
	return info, nil
}

func (u *updateChecker) target() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.result.Available {
		return ""
	}
	return u.openURL
}

type latestRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (u *updateChecker) fetch(ctx context.Context) (latestRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.endpoint, nil)
	if err != nil {
		return latestRelease{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "relayer-desktop/"+strings.TrimSpace(u.current()))
	response, err := u.client.Do(request)
	if err != nil {
		return latestRelease{}, fmt.Errorf("check for updates: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return latestRelease{}, fmt.Errorf("check for updates: GitHub answered %s", response.Status)
	}
	var release latestRelease
	if err := json.NewDecoder(io.LimitReader(response.Body, maxReleaseBody)).Decode(&release); err != nil {
		return latestRelease{}, fmt.Errorf("check for updates: %w", err)
	}
	return release, nil
}

// evaluate compares the release with the running version and picks what an
// update opens. A release whose tag is not a version, or whose links lead
// anywhere but this repository's releases, offers no update.
func (u *updateChecker) evaluate(current string, release latestRelease) (UpdateInfo, string) {
	info := UpdateInfo{Current: current}
	latest := strings.TrimPrefix(strings.TrimSpace(release.TagName), "v")
	latestVersion, ok := parseReleaseVersion(latest)
	if !ok || !strings.HasPrefix(release.HTMLURL, releasePagePrefix) {
		return info, ""
	}
	info.Latest = latest
	currentVersion, _ := parseReleaseVersion(current)
	if !newerVersion(latestVersion, currentVersion) {
		return info, ""
	}
	info.Available = true
	target := release.HTMLURL
	if u.goos == "windows" {
		installer := "relayer-desktop_" + latest + "_windows_amd64_setup.exe"
		for _, asset := range release.Assets {
			if asset.Name == installer && strings.HasPrefix(asset.URL, releasePagePrefix+"download/v"+latest+"/") {
				target = asset.URL
				break
			}
		}
	}
	return info, target
}

// parseReleaseVersion reads MAJOR.MINOR.PATCH. A prerelease or build suffix is
// accepted and ignored: the check compares published releases, which the
// latest-release endpoint never returns as prereleases.
func parseReleaseVersion(value string) ([3]int, bool) {
	var parsed [3]int
	core, _, _ := strings.Cut(value, "-")
	core, _, _ = strings.Cut(core, "+")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return parsed, false
	}
	for index, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 || part == "" {
			return parsed, false
		}
		parsed[index] = number
	}
	return parsed, true
}

func newerVersion(candidate, current [3]int) bool {
	for index := range candidate {
		if candidate[index] != current[index] {
			return candidate[index] > current[index]
		}
	}
	return false
}
