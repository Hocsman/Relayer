package platform

import "errors"

// ErrShellUnsupported reports that the current platform has no supported
// system shell launcher for PTY-backed sessions.
var ErrShellUnsupported = errors.New("exécution shell non prise en charge sur cette plateforme")
