//go:build windows

package platform

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// holdExclusively opens a file the way a scanner or a backup agent does, with
// no sharing, which is what makes Windows refuse a rename touching it. The
// returned function releases the handle.
func holdExclusively(t *testing.T, path string) func() {
	t.Helper()

	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16PtrFromString: %v", err)
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.GENERIC_READ,
		0, // no sharing at all
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("CreateFile %s: %v", path, err)
	}

	var once bool
	return func() {
		if once {
			return
		}
		once = true
		_ = windows.CloseHandle(handle)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// TestTransientFileErrorClassifiesRealWindowsRefusals drives the classifier
// with errors the kernel actually produced rather than with constructed ones.
func TestTransientFileErrorClassifiesRealWindowsRefusals(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.tmp")
	target := filepath.Join(directory, "config.yaml")

	t.Run("a handle on the destination", func(t *testing.T) {
		writeFile(t, source, "new")
		writeFile(t, target, "old")
		release := holdExclusively(t, target)
		defer release()

		err := os.Rename(source, target)
		if err == nil {
			t.Fatal("Windows accepted a rename over a file held exclusively")
		}
		if !TransientFileError(err) {
			t.Fatalf("error %v was not classified as transient", err)
		}
	})

	t.Run("a handle on the source", func(t *testing.T) {
		writeFile(t, source, "new")
		writeFile(t, target, "old")
		release := holdExclusively(t, source)
		defer release()

		err := os.Rename(source, target)
		if err == nil {
			t.Fatal("Windows accepted a rename of a file held exclusively")
		}
		if !TransientFileError(err) {
			t.Fatalf("error %v was not classified as transient", err)
		}
	})

	t.Run("an ordinary reader of the destination", func(t *testing.T) {
		// Go opens without FILE_SHARE_DELETE, so even a plain reader refuses
		// the publish. This is why the flake did not need antivirus to appear.
		writeFile(t, source, "new")
		writeFile(t, target, "old")
		reader, err := os.Open(target)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer reader.Close()

		renameErr := os.Rename(source, target)
		if renameErr == nil {
			t.Skip("this Windows build allows a rename over an open file")
		}
		if !TransientFileError(renameErr) {
			t.Fatalf("error %v was not classified as transient", renameErr)
		}
	})

	t.Run("a missing source is permanent", func(t *testing.T) {
		err := os.Rename(filepath.Join(directory, "absent.tmp"), target)
		if err == nil {
			t.Fatal("renaming a missing file succeeded")
		}
		if TransientFileError(err) {
			t.Fatalf("error %v was classified as transient; waiting will not create the file", err)
		}
	})
}

// TestPublishByRenameOutlastsATransientHold is the regression test for the
// flake itself: a hold longer than the old fifty-millisecond budget, released
// while the publish is still trying.
func TestPublishByRenameOutlastsATransientHold(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.tmp")
	target := filepath.Join(directory, "config.yaml")
	writeFile(t, source, "published")
	writeFile(t, target, "old")

	release := holdExclusively(t, target)
	defer release()

	// 250ms: five times the budget this used to have, and in the range a
	// scanner actually takes.
	const hold = 250 * time.Millisecond
	go func() {
		time.Sleep(hold)
		release()
	}()

	start := time.Now()
	if err := PublishByRename(source, target); err != nil {
		t.Fatalf("publishByRename gave up on a hold that cleared: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed < hold {
		t.Fatalf("elapsed = %s, want the publish to have waited out the %s hold", elapsed, hold)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(content) != "published" {
		t.Fatalf("target content = %q, want the source content", content)
	}
}

// TestPublishByRenameGivesUpOnAPermanentHold pins the other side: a file nobody
// ever releases must fail, bounded, rather than hang the caller.
func TestPublishByRenameGivesUpOnAPermanentHold(t *testing.T) {
	previous := ReplaceTimeout
	ReplaceTimeout = 150 * time.Millisecond
	t.Cleanup(func() { ReplaceTimeout = previous })

	directory := t.TempDir()
	source := filepath.Join(directory, "source.tmp")
	target := filepath.Join(directory, "config.yaml")
	writeFile(t, source, "published")
	writeFile(t, target, "old")

	release := holdExclusively(t, target)
	defer release()

	start := time.Now()
	err := PublishByRename(source, target)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("publishByRename succeeded against a file held for its whole budget")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("elapsed = %s, want the budget to bound the wait", elapsed)
	}
	// The caller wraps this in its own bounded message; what matters here is
	// that the underlying error survives for a maintainer reading a log.
	if _, ok := err.(*os.LinkError); !ok {
		t.Fatalf("error %v (%T), want the underlying LinkError", err, err)
	}
}

// TestTransientFileErrorRejectsUnrelatedErrno guards the classifier against
// widening into codes that do not clear on their own.
func TestTransientFileErrorRejectsUnrelatedErrno(t *testing.T) {
	for _, errno := range []syscall.Errno{
		syscall.Errno(2),  // ERROR_FILE_NOT_FOUND
		syscall.Errno(3),  // ERROR_PATH_NOT_FOUND
		syscall.Errno(87), // ERROR_INVALID_PARAMETER
	} {
		err := &os.LinkError{Op: "rename", Old: "a", New: "b", Err: errno}
		if TransientFileError(err) {
			t.Errorf("errno %d was classified as transient", uintptr(errno))
		}
	}
}
