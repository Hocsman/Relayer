//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package platform

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
)

func TestNewShellCommandUsesUnixShellWithoutRewritingScript(t *testing.T) {
	script := `printf '%s\n' "space value"; printf '%s\n' "apostrophe's" "$HOME"`
	command, err := NewShellCommand(context.Background(), script)
	if err != nil {
		t.Fatalf("NewShellCommand returned an error: %v", err)
	}
	if command.Path != "/bin/sh" {
		t.Fatalf("shell path = %q, want /bin/sh", command.Path)
	}
	if len(command.Args) != 3 || command.Args[0] != "/bin/sh" || command.Args[1] != "-c" || command.Args[2] != script {
		t.Fatalf("shell args = %#v", command.Args)
	}
}

func TestIsPTYCloseErrorRecognizesWrappedEIO(t *testing.T) {
	if !IsPTYCloseError(fmt.Errorf("lecture PTY: %w", syscall.EIO)) {
		t.Fatal("wrapped EIO was not recognized as an expected PTY close error")
	}
	if IsPTYCloseError(errors.New("different error")) {
		t.Fatal("an unrelated error was recognized as a PTY close error")
	}
}
