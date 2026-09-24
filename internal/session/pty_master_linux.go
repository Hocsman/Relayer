//go:build linux

package session

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// pollableMaster returns the master side of a pseudo-terminal as a file the Go
// runtime polls, in place of file, which it closes.
//
// creack/pty opens the master and sizes it through file.Fd(), which puts the
// descriptor in blocking mode for good. A write to a blocking master blocks in
// the kernel once the agent stops reading and its terminal's input buffer is
// full, about 20 KiB of keystrokes: no context, no deadline and no Close ends
// it, since Close defers the descriptor's release until the write returns,
// and the write outlived even the agent's exit. The gateway's keystrokes held
// the session's write slot, and the run's end waited on them, for good.
//
// A non-blocking descriptor is written through the runtime's poller instead: a
// write deadline ends a write that would block, and Close ends one in
// progress. The descriptor is duplicated so that the new file owns one no
// other file will switch back to blocking mode; the non-blocking flag belongs
// to the open file, which both share until the original is closed here.
func pollableMaster(file *os.File) (*os.File, error) {
	raw, err := file.SyscallConn()
	if err != nil {
		return nil, err
	}
	duplicate := -1
	var dupErr error
	if err := raw.Control(func(fd uintptr) {
		duplicate, dupErr = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 0)
	}); err != nil {
		return nil, err
	}
	if dupErr != nil {
		return nil, dupErr
	}
	if err := syscall.SetNonblock(duplicate, true); err != nil {
		_ = unix.Close(duplicate)
		return nil, err
	}
	master := os.NewFile(uintptr(duplicate), file.Name())
	if master == nil {
		_ = unix.Close(duplicate)
		return nil, errors.New("the pseudo-terminal master could not be wrapped")
	}
	_ = file.Close()
	return master, nil
}

// setWindowSize sizes the pseudo-terminal through the file's raw connection.
// creack/pty's Setsize goes through file.Fd(), which would put the master back
// in blocking mode.
func setWindowSize(file *os.File, columns, rows int) error {
	raw, err := file.SyscallConn()
	if err != nil {
		return err
	}
	size := &unix.Winsize{Row: uint16(clamp(rows, 1, 65535)), Col: uint16(clamp(columns, 1, 65535))}
	var ioctlErr error
	if err := raw.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, size)
	}); err != nil {
		return err
	}
	return ioctlErr
}
