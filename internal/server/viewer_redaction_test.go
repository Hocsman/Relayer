package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const viewerProbeSecret = "sk-VIEWER-PROBE-SECRET-4242"

// startGatewayWithSecretInArgv boots a gateway whose only agent carries a
// credential in its argument vector, the way an --api-key flag does.
func startGatewayWithSecretInArgv(t *testing.T) string {
	t.Helper()

	command := `["sh", "-c", "sleep 30", "relayer", "--api-key", "` + viewerProbeSecret + `"]`
	if runtime.GOOS == "windows" {
		command = `["cmd.exe", "/c", "ping -n 30 127.0.0.1 >nul", "--api-key", "` + viewerProbeSecret + `"]`
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	yaml := "version: 1\nbackend: pty\nagents:\n  - id: keyed\n    name: Keyed agent\n    command: " + command +
		"\nintercept_patterns:\n  - pattern: '(?i)continue'\n    description: continue prompt\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	readyCh := make(chan string, 1)
	serverErrCh := startServeForTest(t, ctx, cancel, Options{
		Bind:        "127.0.0.1",
		Port:        0,
		Token:       "alice:opAlice",
		ViewerToken: "dave:viewDave",
		ConfigPath:  configPath,
		Diagnostics: io.Discard,
		OnReady:     func(url, _ string) { readyCh <- url },
	})
	select {
	case url := <-readyCh:
		return url
	case err := <-serverErrCh:
		t.Fatalf("Serve failed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("server startup timed out")
	}
	return ""
}

func rawReply(t *testing.T, client *sharedGatewayClient, method string) string {
	t.Helper()
	var raw json.RawMessage
	client.mustCall(method, map[string]any{}, &raw)
	return string(raw)
}

// TestViewerNeverReceivesAnAgentsArguments pins the leak v0.8.4's changelog
// said was closed: a viewer token received every agent's full command line,
// credentials included, through getAgentProfiles and getState.
func TestViewerNeverReceivesAnAgentsArguments(t *testing.T) {
	baseURL := startGatewayWithSecretInArgv(t)
	viewer := dialSharedGateway(t, baseURL, "viewDave")
	operator := dialSharedGateway(t, baseURL, "opAlice")

	for _, method := range []string{"getAgentProfiles", "getState"} {
		if reply := rawReply(t, viewer, method); strings.Contains(reply, viewerProbeSecret) {
			t.Errorf("a viewer's %s carries the agent's credential:\n%s", method, reply)
		}
	}

	request, _ := http.NewRequest(http.MethodGet, baseURL+"/api/state", nil)
	request.Header.Set("Authorization", "Bearer viewDave")
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET /api/state: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if strings.Contains(string(body), viewerProbeSecret) {
		t.Errorf("a viewer's /api/state carries the agent's credential:\n%s", body)
	}

	// A viewer still sees which agent it is watching.
	var profiles AgentProfilesView
	viewer.mustCall("getAgentProfiles", map[string]any{}, &profiles)
	if len(profiles.Profiles) != 1 || profiles.Profiles[0].ExecutableLabel == "" || profiles.ConfigPath != "" {
		t.Errorf("viewer profiles = %+v, want the agent's label and no configuration path", profiles)
	}

	// An operator, who can rewrite and restart the agent anyway, is unchanged.
	if reply := rawReply(t, operator, "getAgentProfiles"); !strings.Contains(reply, viewerProbeSecret) {
		t.Error("the operator's profiles lost the argument vector it edits")
	}
}

func TestExecutableOnlyKeepsNothingAfterTheExecutable(t *testing.T) {
	// Built the way the session manager builds a display command.
	quoted := func(argv ...string) string {
		parts := make([]string, len(argv))
		for index, argument := range argv {
			parts[index] = strconv.Quote(argument)
		}
		return strings.Join(parts, " ")
	}
	cases := map[string]string{
		quoted("/usr/local/bin/claude", "--api-key", "sk-secret"): "claude",
		"[explicit shell]":             "[explicit shell]",
		"":                             "",
		"claude --api-key sk-unquoted": "",
	}
	if runtime.GOOS == "windows" {
		// filepath.Base splits on backslashes only on Windows.
		cases[quoted(`C:\Tools\aider.exe`, "--openai-api-key", "sk")] = "aider.exe"
	}
	for display, want := range cases {
		if got := executableOnly(display); got != want {
			t.Errorf("executableOnly(%q) = %q, want %q", display, got, want)
		}
	}
}
