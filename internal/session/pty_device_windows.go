//go:build windows

package session

import (
	"errors"
	"fmt"
	"io"
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

// ownedHandle is a pipe handle the device duplicated for itself. conpty's Close
// closes its own handles at once, and a Read blocked in ReadFile on one of them
// then woke up — or the next Read started — on a number Windows had freed: every
// Stop reported "invalid handle" as a stream failure, and once the number had
// been given to another pipe, the reader read that pipe's bytes into the
// agent's output and never ended. This copy is closed only when no Read or
// Write is using it, and an operation that starts after Close never touches it.
type ownedHandle struct {
	mu       sync.Mutex
	handle   windows.Handle
	inFlight int
	closed   bool
}

func duplicateHandle(source windows.Handle) (*ownedHandle, error) {
	process := windows.CurrentProcess()
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(process, source, process, &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	return &ownedHandle{handle: duplicate}, nil
}

// acquire returns the handle for one operation, or false once it is closed.
// Every true answer must be followed by done.
func (h *ownedHandle) acquire() (windows.Handle, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, false
	}
	h.inFlight++
	return h.handle, true
}

func (h *ownedHandle) done() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.inFlight--
	h.closeIfIdleLocked()
}

func (h *ownedHandle) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	h.closeIfIdleLocked()
}

func (h *ownedHandle) closeIfIdleLocked() {
	if h.closed && h.inFlight == 0 && h.handle != 0 {
		_ = windows.CloseHandle(h.handle)
		h.handle = 0
	}
}

// windowsConPTYDevice reads and writes through its own copies of the pipe
// handles. Closing the pseudo console makes conhost exit, which closes the
// other end of both pipes: a Read blocked on the output copy then returns
// ERROR_BROKEN_PIPE, once the output conhost had already written is drained,
// and a blocked Write fails the same way. Only then is each copy closed.
type windowsConPTYDevice struct {
	c      *conpty.ConPty
	output *ownedHandle
	input  *ownedHandle
}

func newWindowsConPTYDevice(c *conpty.ConPty) (*windowsConPTYDevice, error) {
	output, err := duplicateHandle(windows.Handle(c.OutPipeReadFd()))
	if err != nil {
		return nil, fmt.Errorf("duplicate the pseudo console's output: %w", err)
	}
	input, err := duplicateHandle(windows.Handle(c.InPipeWriteFd()))
	if err != nil {
		output.close()
		return nil, fmt.Errorf("duplicate the pseudo console's input: %w", err)
	}
	return &windowsConPTYDevice{c: c, output: output, input: input}, nil
}

func (w *windowsConPTYDevice) Read(p []byte) (int, error) {
	handle, ok := w.output.acquire()
	if !ok {
		return 0, io.EOF
	}
	defer w.output.done()
	var read uint32
	err := windows.ReadFile(handle, p, &read, nil)
	return int(read), err
}

func (w *windowsConPTYDevice) Write(p []byte) (int, error) {
	handle, ok := w.input.acquire()
	if !ok {
		return 0, ErrClosed
	}
	defer w.input.done()
	var written uint32
	err := windows.WriteFile(handle, p, &written, nil)
	return int(written), err
}

func (w *windowsConPTYDevice) Close() error {
	err := w.c.Close()
	w.input.close()
	w.output.close()
	return err
}

func (w *windowsConPTYDevice) Resize(columns, rows int) error {
	return w.c.Resize(clamp(columns, 1, 65535), clamp(rows, 1, 65535))
}

func startPTY(session *processSession, cmd *exec.Cmd, columns, rows int) (ptyDevice, error) {
	c, err := conpty.New(clamp(columns, 1, 65535), clamp(rows, 1, 65535), 0)
	if err != nil {
		return nil, fmt.Errorf("initialize conpty: %w", err)
	}
	device, err := newWindowsConPTYDevice(c)
	if err != nil {
		_ = c.Close()
		return nil, err
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
		_ = device.Close()
		return nil, fmt.Errorf("spawn in conpty: %w", err)
	}
	session.proc.set(windows.Handle(handle))

	p, err := os.FindProcess(pid)
	if err == nil {
		cmd.Process = p
	}

	return device, nil
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
