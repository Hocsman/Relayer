//go:build darwin || linux

package config

import (
	"errors"
	"os"
	"syscall"
	"time"
)

const configurationLockTimeout = 2 * time.Second

// lockConfigurationFile serializes cooperating Relayer processes across the
// read-validate-publish transaction. The lock file is intentionally retained:
// deleting it could let a third process lock a different inode mid-transaction.
func lockConfigurationFile(path string) (func(), error) {
	lockPath := path + ".lock"
	info, err := os.Lstat(lockPath)
	if err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return nil, errors.New("configuration lock must be a regular non-symlink file")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("could not inspect configuration lock")
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("could not open configuration lock")
	}
	deadline := time.Now().Add(configurationLockTimeout)
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, errors.New("could not lock configuration")
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, errors.New("configuration is in use by another Relayer instance")
		}
		time.Sleep(25 * time.Millisecond)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
