package session

import "io"

// ptyDevice abstracts the underlying pseudo-terminal device across operating systems.
// On Unix platforms it is backed by creack/pty (*os.File).
// On Windows platforms it is backed by ConPTY (Windows Pseudo Console).
type ptyDevice interface {
	io.Reader
	io.Writer
	io.Closer
	Resize(columns, rows int) error
}
