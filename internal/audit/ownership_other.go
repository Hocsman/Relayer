//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package audit

import "os"

// Platforms without a Unix UID model still enforce private modes where the
// operating system exposes them and reject symlinks/non-regular paths.
func requireCurrentUserOwner(_ os.FileInfo, _ string) error { return nil }
