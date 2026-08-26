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
		return fmt.Errorf("propriétaire du chemin d'audit %s indéterminable", path)
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("chemin d'audit %s appartenant à un autre utilisateur", path)
	}
	return nil
}
