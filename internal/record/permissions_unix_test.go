//go:build unix

package record

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenStoreRefusesAWorldReadableDirectory pins the contract a caller has to
// satisfy: Relayer creates its transcript directory privately, but it will not
// silently loosen or tighten one somebody else already created. A transcript
// holds raw terminal output, so a directory other local users can read is
// refused rather than used.
//
// This is the same posture as internal/audit, and it is why a configured
// `recording.path` must either not exist yet or already be private.
func TestOpenStoreRefusesAWorldReadableDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	config := DefaultConfig()
	config.Enabled = true
	config.Path = directory

	store, err := OpenStore(config)
	if err == nil {
		_ = store.Close()
		t.Fatal("OpenStore accepted a directory other users can read")
	}
	if info, statErr := os.Stat(directory); statErr == nil && info.Mode().Perm() != 0o755 {
		t.Fatalf("OpenStore changed the directory's permissions to %04o", info.Mode().Perm())
	}
}

// TestOpenStoreCreatesAPrivateDirectory is the other half: a path that does not
// exist yet is created at 0700, so the documented default just works.
func TestOpenStoreCreatesAPrivateDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "nested", "recordings")

	config := DefaultConfig()
	config.Enabled = true
	config.Path = directory

	store, err := OpenStore(config)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	info, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("recording directory permissions = %04o, want no group or other access", info.Mode().Perm())
	}
}
