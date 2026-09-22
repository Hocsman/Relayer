//go:build !windows

package session

import (
	"context"
	"errors"
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// closeConsoleToStop is false on Unix: SIGTERM to the process group is the
// graceful stop, and closing the master first would send SIGHUP before the
// agent had its grace period.
const closeConsoleToStop = false

// processRef holds nothing on Unix. A process-group ID stays reserved while
// the group has a member, and the session stops signalling the group once
// waitSession has cleaned it up.
type processRef struct{}

func (*processRef) release() {}

type unixPTYDevice struct {
	file *os.File
}

func (u *unixPTYDevice) Read(p []byte) (int, error) {
	return u.file.Read(p)
}

func (u *unixPTYDevice) Write(p []byte) (int, error) {
	return u.file.Write(p)
}

func (u *unixPTYDevice) Close() error {
	return u.file.Close()
}

func (u *unixPTYDevice) Resize(columns, rows int) error {
	return pty.Setsize(u.file, &pty.Winsize{
		Rows: uint16(clamp(rows, 1, 65535)),
		Cols: uint16(clamp(columns, 1, 65535)),
	})
}

func startPTY(session *processSession, cmd *exec.Cmd, columns, rows int) (ptyDevice, error) {
	file, err := pty.StartWithSize(cmd, &pty.Winsize{
		Rows: uint16(clamp(rows, 1, 65535)),
		Cols: uint16(clamp(columns, 1, 65535)),
	})
	if err != nil {
		return nil, err
	}
	return &unixPTYDevice{file: file}, nil
}

// waitCommand reaps the leader. os/exec records the state in cmd.ProcessState
// on this goroutine; the Unix platform helpers never read that field, so there
// is nothing for it to race with.
//
// When the session's context was cancelled, os/exec reports the context's
// error from Wait even if the agent then shut down cleanly on its SIGTERM. That
// error says why the agent was asked to stop, not that it failed, so an agent
// that exited on its own terms keeps its real exit status.
func waitCommand(session *processSession) (*os.ProcessState, error) {
	err := session.cmd.Wait()
	state := session.cmd.ProcessState
	if err != nil && state != nil && state.Exited() &&
		(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		if state.Success() {
			return state, nil
		}
		return state, &exec.ExitError{ProcessState: state}
	}
	return state, err
}
