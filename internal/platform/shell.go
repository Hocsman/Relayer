package platform

import "errors"

// ErrShellUnsupported reports that the current platform has no supported
// system shell launcher for PTY-backed sessions.
var ErrShellUnsupported = errors.New("shell execution not supported on this platform")
