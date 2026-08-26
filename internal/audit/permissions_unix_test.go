//go:build !windows

package audit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileSinkCreatesPrivateDirectoryAndEveryGeneration(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(directory, "audit.jsonl")
	sink, err := NewFileSink(path, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range [][]byte{[]byte("1\n"), []byte("2\n"), []byte("3\n")} {
		if err := sink.WriteLine(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(directory); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v, %v", modeOf(info), err)
	}
	for _, candidate := range []string{path, generationPath(path, 1), generationPath(path, 2)} {
		info, err := os.Stat(candidate)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, %v", candidate, modeOf(info), err)
		}
	}
}

func TestFileSinkRejectsSymlinksAndNonPrivateOrNonRegularTargets(t *testing.T) {
	t.Run("file symlink", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "private")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(directory, "target")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(directory, "audit.jsonl")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if sink, err := NewFileSink(link, 1024, 2); err == nil || sink != nil {
			t.Fatalf("file symlink accepted: %#v, %v", sink, err)
		}
	})

	t.Run("directory symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if sink, err := NewFileSink(filepath.Join(link, "audit.jsonl"), 1024, 2); err == nil || sink != nil {
			t.Fatalf("directory symlink accepted: %#v, %v", sink, err)
		}
	})

	t.Run("insecure directory", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "shared")
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if sink, err := NewFileSink(filepath.Join(directory, "audit.jsonl"), 1024, 2); err == nil || sink != nil {
			t.Fatalf("insecure directory accepted: %#v, %v", sink, err)
		}
	})

	t.Run("directory as file", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "private")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "audit.jsonl")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if sink, err := NewFileSink(path, 1024, 2); err == nil || sink != nil {
			t.Fatalf("directory target accepted: %#v, %v", sink, err)
		}
	})

	t.Run("existing file is tightened", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "private")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "audit.jsonl")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		sink, err := NewFileSink(path, 1024, 2)
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.Close(); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("tightened mode = %v, %v", modeOf(info), err)
		}
	})

	if _, err := ResolvePath("bad\x00path"); err == nil {
		t.Fatal("ResolvePath accepted NUL")
	}
	if _, err := os.Stat(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected Stat error: %v", err)
	}
}

func modeOf(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
}
