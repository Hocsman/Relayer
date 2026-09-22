package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/record"
)

const viewerProbeSecret = "sk-VIEWER-PROBE-SECRET-4242"

// startGatewayWithSecretInArgv boots a gateway whose only agent carries a
// credential in its argument vector, the way an --api-key flag does.
func startGatewayWithSecretInArgv(t *testing.T) (string, string) {
	t.Helper()

	command := `["sh", "-c", "sleep 30", "relayer", "--api-key", "` + viewerProbeSecret + `"]`
	if runtime.GOOS == "windows" {
		command = `["cmd.exe", "/c", "ping -n 30 127.0.0.1 >nul", "--api-key", "` + viewerProbeSecret + `"]`
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	// The journal lives in the test's directory, so a leaked path is detectable.
	// The journal refuses a directory other users can read.
	journalDir := filepath.Join(dir, "audit")
	if err := os.Mkdir(journalDir, 0o700); err != nil {
		t.Fatalf("mkdir audit: %v", err)
	}
	journal := filepath.Join(journalDir, "audit.jsonl")
	// So is the agent's working directory.
	workdir := filepath.Join(dir, "work")
	if err := os.Mkdir(workdir, 0o700); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	yaml := "version: 1\nbackend: pty\naudit:\n  enabled: true\n  mode: metadata\n  path: '" + journal + "'\n" +
		"agents:\n  - id: keyed\n    name: Keyed agent\n    cwd: '" + workdir + "'\n    command: " + command +
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
		return url, configPath
	case err := <-serverErrCh:
		t.Fatalf("Serve failed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("server startup timed out")
	}
	return "", ""
}

func rawReply(t *testing.T, client *sharedGatewayClient, method string) string {
	t.Helper()
	var raw json.RawMessage
	client.mustCall(method, map[string]any{}, &raw)
	return string(raw)
}

// TestViewerNeverReceivesAnAgentsArguments pins the leak v0.8.4's changelog
// said was closed: a viewer token received every agent's full command line,
// credentials included, through getAgentProfiles; and getState named the
// configuration and journal files in its startup notices. getAuditSummary and
// verifyAuditJournal also named the journal's path.
func TestViewerNeverReceivesAnAgentsArguments(t *testing.T) {
	baseURL, configPath := startGatewayWithSecretInArgv(t)
	// A token unique to this test's directory, independent of how JSON escapes
	// a Windows path.
	pathToken := filepath.Base(filepath.Dir(filepath.Dir(configPath)))
	viewer := dialSharedGateway(t, baseURL, "viewDave")
	operator := dialSharedGateway(t, baseURL, "opAlice")

	for _, method := range []string{"getAgentProfiles", "getState", "getAuditSummary", "verifyAuditJournal"} {
		reply := rawReply(t, viewer, method)
		if strings.Contains(reply, viewerProbeSecret) {
			t.Errorf("a viewer's %s carries the agent's credential:\n%s", method, reply)
		}
		if strings.Contains(reply, pathToken) {
			t.Errorf("a viewer's %s names the configuration's directory:\n%s", method, reply)
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
	if reply := rawReply(t, operator, "getAuditSummary"); !strings.Contains(reply, pathToken) {
		t.Errorf("the operator's audit summary lost the journal's path:\n%s", reply)
	}
}

// TestViewerStillSeesWhichAgentItWatches: the display command is already only
// the executable's name. Masking it again left a viewer's cards nameless.
func TestViewerStillSeesWhichAgentItWatches(t *testing.T) {
	baseURL, _ := startGatewayWithSecretInArgv(t)
	viewer := dialSharedGateway(t, baseURL, "viewDave")
	operator := dialSharedGateway(t, baseURL, "opAlice")

	var viewerState, operatorState AppState
	viewer.mustCall("getState", map[string]any{}, &viewerState)
	operator.mustCall("getState", map[string]any{}, &operatorState)
	if len(viewerState.Agents) != 1 || len(operatorState.Agents) != 1 {
		t.Fatalf("agents = %d for the viewer and %d for the operator, want 1 each", len(viewerState.Agents), len(operatorState.Agents))
	}
	if viewerState.Agents[0].DisplayCommand == "" || viewerState.Agents[0].DisplayCommand != operatorState.Agents[0].DisplayCommand {
		t.Fatalf("viewer display command = %q, operator's = %q, want the same executable name",
			viewerState.Agents[0].DisplayCommand, operatorState.Agents[0].DisplayCommand)
	}
}

// TestViewerRecordingErrorsNameNoPath: a transcript deleted by hand, or a
// recording directory that went away, made the store's error — which quotes the
// directory and the file — reach a viewer verbatim.
func TestViewerRecordingErrorsNameNoPath(t *testing.T) {
	secretPath := filepath.Join("srv", "SECRETREC", "keyed-1.cast")
	leaky := fmt.Errorf("inspect the recording file %s: %w", secretPath, os.ErrNotExist)

	if got := recordingErrorForRole(leaky, RoleViewer); strings.Contains(got.Error(), "SECRETREC") {
		t.Fatalf("a viewer's recording error = %q, want no path", got)
	}
	if got := recordingErrorForRole(leaky, RoleOperator); got != leaky {
		t.Fatalf("an operator's recording error = %v, want it unchanged", got)
	}
	wrapped := fmt.Errorf("%w: %s", record.ErrRecordingNotFound, secretPath)
	if got := recordingErrorForRole(wrapped, RoleViewer); got != record.ErrRecordingNotFound {
		t.Fatalf("a viewer's not-found error = %q, want the bare sentinel", got)
	}
}
