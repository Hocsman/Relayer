//go:build windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsSyncDirectoryAcceptsExistingDirectory(t *testing.T) {
	if err := syncDirectory(t.TempDir()); err != nil {
		t.Fatalf("sync existing directory: %v", err)
	}
}

func TestWindowsSyncDirectoryRejectsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := syncDirectory(path); err == nil {
		t.Fatal("syncDirectory accepted a regular file")
	}
}
