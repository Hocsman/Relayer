//go:build windows

package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// holdExclusively opens a file with no sharing, the way a scanner or a backup
// agent does. Windows then refuses both a remove and a rename touching it.
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

	released := false
	return func() {
		if released {
			return
		}
		released = true
		_ = windows.CloseHandle(handle)
	}
}

// TestRotationOutlastsATransientHoldOnAGeneration is the reason rotation goes
// through the platform helpers.
//
// Rotation removes the oldest generation and renames the rest down. Windows
// refuses both while another handle is open on the path, and Go does not pass
// FILE_SHARE_DELETE when it opens a file, so a reader of an older generation is
// enough. The sink is fail-closed: a refusal that would have cleared on its own
// used to become a sticky rotate error and, from there, a lost record.
func TestRotationOutlastsATransientHoldOnAGeneration(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(directory, "audit.jsonl")

	line := func(sequence int) []byte {
		return []byte(fmt.Sprintf(`{"sequence":%d,"pad":"xxxxxxxxxxxxxxxx"}`+"\n", sequence))
	}
	// Two lines per generation, so the third write rotates.
	sink, err := NewFileSink(path, int64(len(line(1))*2), 3)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	for sequence := 1; sequence <= 3; sequence++ {
		if err := sink.WriteLine(line(sequence)); err != nil {
			t.Fatalf("WriteLine %d: %v", sequence, err)
		}
	}

	rotated := path + ".1"
	if _, err := os.Stat(rotated); err != nil {
		t.Fatalf("expected a rotated generation at %s: %v", rotated, err)
	}

	// Hold the rotated generation for longer than the old fifty-millisecond
	// budget, and release it while rotation is still trying.
	release := holdExclusively(t, rotated)
	defer release()
	const hold = 250 * time.Millisecond
	go func() {
		time.Sleep(hold)
		release()
	}()

	start := time.Now()
	for sequence := 4; sequence <= 5; sequence++ {
		if err := sink.WriteLine(line(sequence)); err != nil {
			t.Fatalf("WriteLine %d gave up on a hold that cleared: %v", sequence, err)
		}
	}
	if elapsed := time.Since(start); elapsed < hold {
		t.Fatalf("elapsed = %s, want rotation to have waited out the %s hold", elapsed, hold)
	}

	// The sink must still be writable: a sticky rotate error would have made
	// every later record fail.
	if err := sink.WriteLine(line(6)); err != nil {
		t.Fatalf("the sink is stuck after a transient hold: %v", err)
	}
}
