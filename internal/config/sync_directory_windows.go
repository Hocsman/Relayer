//go:build windows

package config

import (
	"errors"
	"os"
)

// Windows has no portable equivalent of fsync for directory metadata.
// os.File.Sync maps to FlushFileBuffers, which requires a writable file handle
// and consequently fails for directory handles opened by os.Open. The new file
// itself is synced before the same-directory atomic rename, so validate that
// the parent directory still exists and treat the completed rename as the
// publication boundary instead of reporting every successful save as
// post-commit uncertain.
func syncDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("le parent de la configuration n'est pas un dossier")
	}
	return nil
}
