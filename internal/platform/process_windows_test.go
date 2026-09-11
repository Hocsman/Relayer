//go:build windows

package platform

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestNewShellCommandUsesWindowsCmdWithoutRewritingScript(t *testing.T) {
	script := `echo "space value" & echo apostrophe's`
	command, err := NewShellCommand(context.Background(), script)
	if err != nil {
		t.Fatalf("NewShellCommand returned an error: %v", err)
	}
	base := strings.ToLower(filepath.Base(command.Path))
	if base != "cmd.exe" && command.Path != "cmd.exe" {
		t.Fatalf("shell path = %q, want cmd.exe", command.Path)
	}
	if len(command.Args) != 3 || command.Args[0] != "cmd.exe" || command.Args[1] != "/c" || command.Args[2] != script {
		t.Fatalf("shell args = %#v", command.Args)
	}
}

func TestIsPTYCloseErrorRecognizesBrokenPipe(t *testing.T) {
	if !IsPTYCloseError(fmt.Errorf("read: %w", windows.ERROR_BROKEN_PIPE)) {
		t.Fatal("ERROR_BROKEN_PIPE was not recognized as an expected PTY close error")
	}
	if !IsPTYCloseError(fmt.Errorf("read: %w", windows.ERROR_PIPE_NOT_CONNECTED)) {
		t.Fatal("ERROR_PIPE_NOT_CONNECTED was not recognized as an expected PTY close error")
	}
	if IsPTYCloseError(errors.New("different error")) {
		t.Fatal("an unrelated error was recognized as a PTY close error")
	}
}

func TestProcessGroupExistsAndKill(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "ping -n 10 127.0.0.1 >nul")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start test process: %v", err)
	}
	defer KillProcessGroup(cmd)

	if !ProcessGroupExists(cmd) {
		t.Fatal("expected process to be reported as active")
	}

	KillProcessGroup(cmd)
	_ = cmd.Wait()

	// Wait briefly for the OS handle / exit state to be cleaned up
	time.Sleep(50 * time.Millisecond)

	if ProcessGroupExists(cmd) {
		t.Fatal("expected process to be reported as terminated")
	}
}
