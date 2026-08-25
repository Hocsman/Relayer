//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package platform

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

func TestIsPTYCloseErrorRecognizesWrappedEIO(t *testing.T) {
	if !IsPTYCloseError(fmt.Errorf("lecture PTY: %w", syscall.EIO)) {
		t.Fatal("wrapped EIO was not recognized as an expected PTY close error")
	}
	if IsPTYCloseError(errors.New("different error")) {
		t.Fatal("an unrelated error was recognized as a PTY close error")
	}
}
