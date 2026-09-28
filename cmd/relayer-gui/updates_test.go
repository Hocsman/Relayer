package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const releaseJSON = `{
  "tag_name": "v0.9.2",
  "html_url": "https://github.com/Hocsman/Relayer/releases/tag/v0.9.2",
  "assets": [
    {"name": "relayer-desktop_0.9.2_windows_amd64.zip",
     "browser_download_url": "https://github.com/Hocsman/Relayer/releases/download/v0.9.2/relayer-desktop_0.9.2_windows_amd64.zip"},
    {"name": "relayer-desktop_0.9.2_windows_amd64_setup.exe",
     "browser_download_url": "https://github.com/Hocsman/Relayer/releases/download/v0.9.2/relayer-desktop_0.9.2_windows_amd64_setup.exe"}
  ]
}`

func fakeReleaseServer(t *testing.T, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.Header.Get("User-Agent"); !strings.HasPrefix(got, "relayer-desktop/") {
			t.Errorf("User-Agent = %q", got)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func checkerFor(server *httptest.Server, current, goos string) *updateChecker {
	checker := newUpdateChecker()
	checker.client = server.Client()
	checker.endpoint = server.URL
	checker.current = func() string { return current }
	checker.goos = goos
	return checker
}

func TestANewerReleaseIsOfferedWithItsInstallerOnWindows(t *testing.T) {
	t.Setenv(noUpdateCheckEnv, "")
	server, _ := fakeReleaseServer(t, releaseJSON)
	checker := checkerFor(server, "0.8.14", "windows")

	info, err := checker.check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !info.Available || info.Current != "0.8.14" || info.Latest != "0.9.2" {
		t.Fatalf("info = %+v, want 0.9.2 offered over 0.8.14", info)
	}
	want := "https://github.com/Hocsman/Relayer/releases/download/v0.9.2/relayer-desktop_0.9.2_windows_amd64_setup.exe"
	if got := checker.target(); got != want {
		t.Fatalf("target = %q, want the installer %q", got, want)
	}
}

func TestElsewhereTheUpdateOpensTheReleasePage(t *testing.T) {
	t.Setenv(noUpdateCheckEnv, "")
	server, _ := fakeReleaseServer(t, releaseJSON)
	checker := checkerFor(server, "0.8.14", "darwin")
	if _, err := checker.check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	if got := checker.target(); got != "https://github.com/Hocsman/Relayer/releases/tag/v0.9.2" {
		t.Fatalf("target = %q, want the release page", got)
	}
}

func TestTheSameOrAnOlderReleaseOffersNothing(t *testing.T) {
	t.Setenv(noUpdateCheckEnv, "")
	for _, current := range []string{"0.9.2", "v0.9.2", "0.10.0", "1.0.0"} {
		server, _ := fakeReleaseServer(t, releaseJSON)
		checker := checkerFor(server, current, "windows")
		info, err := checker.check(context.Background())
		if err != nil {
			t.Fatalf("%s: check: %v", current, err)
		}
		if info.Available || checker.target() != "" {
			t.Errorf("%s: info = %+v, want no update", current, info)
		}
	}
}

// The check asks GitHub once per launch, however often the interface asks.
func TestTheCheckAsksGitHubOncePerLaunch(t *testing.T) {
	t.Setenv(noUpdateCheckEnv, "")
	server, requests := fakeReleaseServer(t, releaseJSON)
	checker := checkerFor(server, "0.8.14", "linux")
	for range 3 {
		if _, err := checker.check(context.Background()); err != nil {
			t.Fatalf("check: %v", err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

// Neither a development build nor an administrator's opt-out reaches GitHub.
func TestADevelopmentBuildOrTheOptOutSendsNothing(t *testing.T) {
	server, requests := fakeReleaseServer(t, releaseJSON)

	t.Setenv(noUpdateCheckEnv, "")
	info, err := checkerFor(server, "dev", "linux").check(context.Background())
	if err != nil || !info.Disabled || info.Available {
		t.Fatalf("development build: info = %+v, err = %v", info, err)
	}

	t.Setenv(noUpdateCheckEnv, "1")
	info, err = checkerFor(server, "0.8.14", "linux").check(context.Background())
	if err != nil || !info.Disabled || info.Available {
		t.Fatalf("opted out: info = %+v, err = %v", info, err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("requests = %d, want none", got)
	}
}

// Whatever the response names, an update only ever opens a release of this
// repository.
func TestAnUpdateOpensNothingOutsideTheRepositorysReleases(t *testing.T) {
	t.Setenv(noUpdateCheckEnv, "")
	for name, body := range map[string]string{
		"foreign release page":      `{"tag_name": "v0.9.2", "html_url": "https://example.com/releases/tag/v0.9.2"}`,
		"tag that is not a version": `{"tag_name": "latest", "html_url": "https://github.com/Hocsman/Relayer/releases/tag/latest"}`,
	} {
		server, _ := fakeReleaseServer(t, body)
		checker := checkerFor(server, "0.8.14", "linux")
		info, err := checker.check(context.Background())
		if err != nil {
			t.Fatalf("%s: check: %v", name, err)
		}
		if info.Available || checker.target() != "" {
			t.Errorf("%s: info = %+v, target = %q, want no update", name, info, checker.target())
		}
	}

	server, _ := fakeReleaseServer(t, strings.ReplaceAll(releaseJSON,
		"https://github.com/Hocsman/Relayer/releases/download/v0.9.2/relayer-desktop_0.9.2_windows_amd64_setup.exe",
		"https://example.com/relayer-desktop_0.9.2_windows_amd64_setup.exe"))
	checker := checkerFor(server, "0.8.14", "windows")
	if _, err := checker.check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	if got := checker.target(); got != "https://github.com/Hocsman/Relayer/releases/tag/v0.9.2" {
		t.Fatalf("a foreign installer URL: target = %q, want the release page instead", got)
	}
}

func TestAFailedCheckReportsItsErrorAndOffersNothing(t *testing.T) {
	t.Setenv(noUpdateCheckEnv, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	checker := checkerFor(server, "0.8.14", "linux")
	info, err := checker.check(context.Background())
	if err == nil || info.Available {
		t.Fatalf("info = %+v, err = %v; want an error and no update", info, err)
	}
}

func TestOpenUpdateOpensOnlyWhatTheCheckFound(t *testing.T) {
	t.Setenv(noUpdateCheckEnv, "")
	server, _ := fakeReleaseServer(t, releaseJSON)
	application := NewApp()
	application.updates = checkerFor(server, "0.8.14", "linux")
	application.ctx = context.Background()
	var opened []string
	application.openURL = func(_ context.Context, url string) { opened = append(opened, url) }

	if err := application.OpenUpdate(); err == nil {
		t.Fatal("OpenUpdate before any check opened something")
	}
	if _, err := application.CheckForUpdate(); err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if err := application.OpenUpdate(); err != nil {
		t.Fatalf("OpenUpdate: %v", err)
	}
	if len(opened) != 1 || opened[0] != "https://github.com/Hocsman/Relayer/releases/tag/v0.9.2" {
		t.Fatalf("opened = %v", opened)
	}
}
