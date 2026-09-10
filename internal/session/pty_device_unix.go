//go:build !windows

package session

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

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

func waitCommand(session *processSession) error {
	return session.cmd.Wait()
}
