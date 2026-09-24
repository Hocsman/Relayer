//go:build !windows && !linux

package session

import (
	"os"

	"github.com/creack/pty"
)

// pollableMaster leaves the master as creack/pty opened it: in blocking mode.
// The runtime's poller is not relied on for pseudo-terminals outside Linux, so
// a write to an agent that reads nothing may still block until it reads again
// or exits; the run's end bounds its wait for one.
func pollableMaster(file *os.File) (*os.File, error) {
	return file, nil
}

// setWindowSize sizes the pseudo-terminal as creack/pty does.
func setWindowSize(file *os.File, columns, rows int) error {
	return pty.Setsize(file, &pty.Winsize{
		Rows: uint16(clamp(rows, 1, 65535)),
		Cols: uint16(clamp(columns, 1, 65535)),
	})
}
