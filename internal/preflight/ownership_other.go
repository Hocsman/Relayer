//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package preflight

import "io/fs"

func currentOwnerStatus(fs.FileInfo) OwnerStatus { return OwnerUnknown }
