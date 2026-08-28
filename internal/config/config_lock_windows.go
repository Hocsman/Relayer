//go:build windows

package config

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

const configurationLockTimeout = 2 * time.Second

// lockConfigurationFile serializes cooperating Relayer processes on Windows
// across the same read-validate-publish transaction used on Unix.
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

	var overlapped windows.Overlapped
	deadline := time.Now().Add(configurationLockTimeout)
	for {
		err = windows.LockFileEx(
			windows.Handle(file.Fd()),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0,
			1,
			0,
			&overlapped,
		)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
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
		_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
		_ = file.Close()
	}, nil
}
