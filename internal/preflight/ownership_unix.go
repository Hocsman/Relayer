//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package preflight

import (
	"io/fs"
	"os"
	"syscall"
)

func currentOwnerStatus(info fs.FileInfo) OwnerStatus {
	if info == nil {
		return OwnerUnknown
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return OwnerUnknown
	}
	if int(stat.Uid) != os.Geteuid() {
		return OwnerOther
	}
	return OwnerCurrent
}
