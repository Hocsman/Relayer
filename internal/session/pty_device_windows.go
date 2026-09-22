//go:build windows

package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/charmbracelet/x/conpty"
	"golang.org/x/sys/windows"
)

// closeConsoleToStop is true on Windows: a console program under ConPTY has no
// window for taskkill's polite request to reach, so closing the pseudo console
// — which sends CTRL_CLOSE_EVENT to every process attached to it — is the
// graceful stop. Waiting out a grace period first only delayed every Stop.
const closeConsoleToStop = true

// processRef pins the leader's process object for as long as the Manager owns
// the session. Windows never hands a PID to a new process while a handle to
// the old one is open, so holding it is what keeps every later use of the
// number — taskkill's tree walk, OpenProcess — about this session's process
// rather than whatever started after it exited. It is released only when the
// session itself is: by Remove, or at the end of Manager.Close.
type processRef struct {
	mu     sync.Mutex
	handle windows.Handle
}

func (r *processRef) set(handle windows.Handle) {
	r.mu.Lock()
	r.handle = handle
	r.mu.Unlock()
}

func (r *processRef) waitable() windows.Handle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.handle
}

func (r *processRef) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handle != 0 {
		_ = windows.CloseHandle(r.handle)
		r.handle = 0
	}
}

type windowsConPTYDevice struct {
	c *conpty.ConPty
}

func (w *windowsConPTYDevice) Read(p []byte) (int, error) {
	return w.c.Read(p)
}

func (w *windowsConPTYDevice) Write(p []byte) (int, error) {
	return w.c.Write(p)
}

func (w *windowsConPTYDevice) Close() error {
	var errs []error
	if err := w.c.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (w *windowsConPTYDevice) Resize(columns, rows int) error {
	return w.c.Resize(clamp(columns, 1, 65535), clamp(rows, 1, 65535))
}

func startPTY(session *processSession, cmd *exec.Cmd, columns, rows int) (ptyDevice, error) {
	c, err := conpty.New(clamp(columns, 1, 65535), clamp(rows, 1, 65535), 0)
	if err != nil {
		return nil, fmt.Errorf("initialize conpty: %w", err)
	}

	name := cmd.Path
	if name == "" && len(cmd.Args) > 0 {
		name = cmd.Args[0]
	}

	attr := &syscall.ProcAttr{
		Dir: cmd.Dir,
		Env: cmd.Env,
	}
	if cmd.SysProcAttr != nil {
		attr.Sys = cmd.SysProcAttr
	}

	pid, handle, err := c.Spawn(name, cmd.Args, attr)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("spawn in conpty: %w", err)
	}
	session.proc.set(windows.Handle(handle))

	p, err := os.FindProcess(pid)
	if err == nil {
		cmd.Process = p
	}

	return &windowsConPTYDevice{c: c}, nil
}

// waitCommand waits for the leader and returns its exit state. It deliberately
// leaves cmd.ProcessState unset: the platform helpers read that field from the
// stop path's goroutine, and writing it here raced with them. The session
// keeps the state under its own lock instead.
//
// The spawn handle is not closed here. cmd.Process.Wait releases the handle
// os.FindProcess opened, which is fine, because processRef still holds the
// spawn handle and with it the PID.
func waitCommand(session *processSession) (*os.ProcessState, error) {
	if session.cmd != nil && session.cmd.Process != nil {
		return session.cmd.Process.Wait()
	}

	handle := session.proc.waitable()
	if handle == 0 {
		return nil, errors.New("no process to wait for")
	}
	event, err := windows.WaitForSingleObject(handle, windows.INFINITE)
	if err != nil {
		return nil, fmt.Errorf("wait for process: %w", err)
	}
	if event != windows.WAIT_OBJECT_0 {
		return nil, fmt.Errorf("unexpected wait event: %d", event)
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return nil, fmt.Errorf("get exit code: %w", err)
	}
	if exitCode != 0 {
		return nil, fmt.Errorf("process exited with status %d", exitCode)
	}
	return nil, nil
}
