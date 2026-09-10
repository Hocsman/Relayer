//go:build windows

package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/charmbracelet/x/conpty"
	"golang.org/x/sys/windows"
)

type windowsConPTYDevice struct {
	c             *conpty.ConPty
	processHandle windows.Handle
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

	p, err := os.FindProcess(pid)
	if err == nil {
		cmd.Process = p
	}

	dev := &windowsConPTYDevice{
		c:             c,
		processHandle: windows.Handle(handle),
	}
	return dev, nil
}

func waitCommand(session *processSession) error {
	dev, ok := session.device.(*windowsConPTYDevice)
	if ok && dev.processHandle != 0 {
		defer func() {
			_ = windows.CloseHandle(dev.processHandle)
			dev.processHandle = 0
		}()
	}

	if session.cmd != nil && session.cmd.Process != nil {
		state, err := session.cmd.Process.Wait()
		if state != nil {
			session.cmd.ProcessState = state
		}
		return err
	}

	if dev != nil && dev.processHandle != 0 {
		event, err := windows.WaitForSingleObject(dev.processHandle, windows.INFINITE)
		if err != nil {
			return fmt.Errorf("wait for process: %w", err)
		}
		if event != windows.WAIT_OBJECT_0 {
			return fmt.Errorf("unexpected wait event: %d", event)
		}
		var exitCode uint32
		if err := windows.GetExitCodeProcess(dev.processHandle, &exitCode); err != nil {
			return fmt.Errorf("get exit code: %w", err)
		}
		if exitCode != 0 {
			return fmt.Errorf("process exited with status %d", exitCode)
		}
	}
	return nil
}
