//go:build windows

package session

import (
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procPeekNamedPipe = kernel32.NewProc("PeekNamedPipe")
)

func checkPipeEmpty(t *testing.T, reader *os.File) {
	t.Helper()
	var bytesAvail uint32
	r1, _, err := procPeekNamedPipe.Call(
		reader.Fd(),
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&bytesAvail)),
		0,
	)
	if r1 == 0 {
		t.Fatalf("PeekNamedPipe failed: %v", err)
	}
	if bytesAvail != 0 {
		t.Fatalf("expected 0 bytes in pipe, got %d", bytesAvail)
	}
}
