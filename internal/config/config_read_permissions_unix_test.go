//go:build unix

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadExistingMarksUnreadableSelectedFileWithoutRenderingPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read files regardless of permission bits")
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "private-selected-config.yaml")
	writeConfigTestFile(t, path, []byte("version: 1\n"))
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	_, err := LoadExisting(path)
	if err == nil || !errors.Is(err, ErrExistingConfigRead) || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("LoadExisting unreadable error = %v", err)
	}
	var readError *ExistingConfigReadError
	if !errors.As(err, &readError) {
		t.Fatalf("LoadExisting unreadable error type = %T", err)
	}
	if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), filepath.Base(path)) {
		t.Fatalf("LoadExisting rendered the selected path: %q", err)
	}
}
