//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package audit

import (
	"fmt"
	"os"
	"syscall"
)

func requireCurrentUserOwner(info os.FileInfo, path string) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("owner of audit path %s cannot be determined", path)
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("audit path %s is owned by another user", path)
	}
	return nil
}
