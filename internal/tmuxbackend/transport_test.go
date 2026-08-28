//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tmuxbackend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/agent"
)

func TestQuotePOSIXRoundTripsHostileArguments(t *testing.T) {
	arguments := []string{
		"spaces and tabs\t",
		"apostrophe's",
		`double "quotes"`,
		"semi;colon",
		"dollar$HOME",
		"back`tick`",
		"line one\nline two",
		"",
		"/tmp/path with spaces/file",
	}
	words := make([]string, len(arguments))
	for index, argument := range arguments {
		words[index] = quotePOSIX(argument)
	}
	script := `printf '%s\000' ` + strings.Join(words, " ")
	output, err := exec.Command("/bin/sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("shell round trip: %v", err)
	}
	encoded := bytes.Split(output, []byte{0})
	encoded = encoded[:len(encoded)-1]
	got := make([]string, len(encoded))
	for index := range encoded {
		got[index] = string(encoded[index])
	}
	if !reflect.DeepEqual(got, arguments) {
		t.Fatalf("quoted round trip = %#v, want %#v", got, arguments)
	}
}

func TestPipeCommandUsesAbsoluteCatAndQuotesPrivatePath(t *testing.T) {
	path := "/tmp/private output's;$(unsafe)"
	command := pipeCommand(path)
	if !strings.HasPrefix(command, "umask 077; exec /bin/cat > ") {
		t.Fatalf("pipe command depends on server PATH: %q", command)
	}
	if !strings.HasSuffix(command, quotePOSIX(path)) {
		t.Fatalf("pipe path is not encoded as one shell word: %q", command)
	}
}

func TestLaunchSpecIsPrivateExactAndCarriesFreshMergedEnvironment(t *testing.T) {
	runtimeDirectory := t.TempDir()
	if err := os.Chmod(runtimeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RELAYER_PARENT_SNAPSHOT_TOKEN", "parent-secret")
	t.Setenv("TMUX", "stale-server-value")
	t.Setenv("TMUX_PANE", "%99")
	t.Setenv("TERM", "host-term")
	spec := agent.Spec{
		Command: []string{"tool with spaces", "", "a'b", `"quoted"`, ";$`\n"},
		Cwd:     filepath.Join(runtimeDirectory, "cwd with spaces"),
		Env: map[string]string{
			"EXPLICIT_TOKEN": "configured-secret",
			"EMPTY":          "",
		},
	}
	files, err := createLaunchFiles(runtimeDirectory, "relayer-run-agent-hash", spec)
	if err != nil {
		t.Fatalf("createLaunchFiles: %v", err)
	}
	defer func() {
		files.close()
		files.remove()
	}()

	assertPermission(t, runtimeDirectory, 0o700)
	assertPermission(t, files.specPath, 0o600)
	assertPermission(t, files.gatePath, 0o600)
	assertPermission(t, files.outputPath, 0o600)

	payload, err := os.ReadFile(files.specPath)
	if err != nil {
		t.Fatalf("read private spec: %v", err)
	}
	var decoded launchSpec
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode private spec: %v", err)
	}
	if !reflect.DeepEqual(decoded.Command, spec.Command) || decoded.Cwd != spec.Cwd {
		t.Fatal("decoded launch command or working directory differs from the test specification")
	}
	if decoded.Env["RELAYER_PARENT_SNAPSHOT_TOKEN"] != "parent-secret" ||
		decoded.Env["EXPLICIT_TOKEN"] != "configured-secret" || decoded.Env["EMPTY"] != "" {
		t.Fatal("fresh merged environment missing expected values")
	}
	for _, dynamic := range []string{"TERM", "TMUX", "TMUX_PANE"} {
		if _, found := decoded.Env[dynamic]; found {
			t.Fatalf("tmux-owned environment %s was serialized", dynamic)
		}
	}

	command := helperCommand("/path with spaces/relayer", files.specPath, files.gatePath)
	for _, secret := range append(append([]string(nil), spec.Command...), "configured-secret") {
		if secret != "" && strings.Contains(command, secret) {
			t.Fatalf("user value %q leaked into tmux shell-command %q", secret, command)
		}
	}
}

func TestReadLaunchSpecRejectsLoosePermissionsSymlinksAndOversize(t *testing.T) {
	directory := t.TempDir()
	valid := filepath.Join(directory, "valid.json")
	if err := os.WriteFile(valid, []byte(`{"command":["true"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLaunchSpec(valid); err != nil {
		t.Fatalf("valid private spec rejected: %v", err)
	}

	loose := filepath.Join(directory, "loose.json")
	if err := os.WriteFile(loose, []byte(`{"command":["true"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readLaunchSpec(loose); err == nil {
		t.Fatal("world-readable spec was accepted")
	}

	link := filepath.Join(directory, "link.json")
	if err := os.Symlink(valid, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readLaunchSpec(link); err == nil {
		t.Fatal("symlink spec was accepted")
	}

	oversize := filepath.Join(directory, "oversize.json")
	if err := os.WriteFile(oversize, bytes.Repeat([]byte("x"), maxSpecSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLaunchSpec(oversize); err == nil {
		t.Fatal("oversized spec was accepted")
	}
}

func TestLaunchReleaseWaitsForConfirmedHelperHandoff(t *testing.T) {
	files, err := createLaunchFiles(t.TempDir(), "release-handshake", agent.Spec{
		Command: []string{"/usr/bin/true"},
	})
	if err != nil {
		t.Fatalf("create launch files: %v", err)
	}
	t.Cleanup(func() {
		files.close()
		files.remove()
	})
	if err := files.release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if files.gate == nil {
		t.Fatal("release closed the only gate writer before helper readiness")
	}

	reader, err := os.OpenFile(files.gatePath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open delayed helper reader: %v", err)
	}
	buffer := make([]byte, len("start\n"))
	read, readErr := reader.Read(buffer)
	closeErr := reader.Close()
	if readErr != nil {
		t.Fatalf("delayed helper read: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close delayed helper reader: %v", closeErr)
	}
	if got := string(buffer[:read]); got != "start\n" {
		t.Fatalf("delayed helper read %q, want start signal", got)
	}

	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer timeoutCancel()
	if err := files.waitForHandoff(timeoutCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("handoff before helper removed gate = %v, want deadline", err)
	}
	if files.gate == nil {
		t.Fatal("failed handoff confirmation closed the gate descriptor")
	}
	if err := os.Remove(files.gatePath); err != nil {
		t.Fatalf("simulate helper gate removal: %v", err)
	}
	if err := files.waitForHandoff(context.Background()); err != nil {
		t.Fatalf("confirm helper handoff: %v", err)
	}
	if files.gate != nil {
		t.Fatal("confirmed helper handoff retained the gate descriptor")
	}

	files.close()
	if files.gate != nil {
		t.Fatal("transport close retained the gate descriptor")
	}
}

func assertPermission(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("permissions %s = %#o, want %#o", path, got, want)
	}
}
