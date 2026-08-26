//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tmuxbackend

import (
	"os"
	"syscall"
)

func ensurePlatformSupport() error { return nil }

func makeFIFO(path string, mode uint32) error {
	return syscall.Mkfifo(path, mode)
}

func openFIFO(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR, 0o600)
}
