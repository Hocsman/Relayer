//go:build windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsConfigurationLockReportsBusyAndClosesEveryHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	unlockFirst, err := lockConfigurationFile(path)
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	firstHeld := true
	t.Cleanup(func() {
		if firstHeld {
			unlockFirst()
		}
	})

	started := time.Now()
	unlockContender, contenderErr := lockConfigurationFile(path)
	elapsed := time.Since(started)
	if unlockContender != nil {
		unlockContender()
		t.Fatal("contending Windows lock unexpectedly succeeded")
	}
	if contenderErr == nil || !strings.Contains(contenderErr.Error(), "occupée") {
		t.Fatalf("contending lock error = %v, want bounded busy classification", contenderErr)
	}
	if elapsed < configurationLockTimeout-100*time.Millisecond || elapsed > 2*configurationLockTimeout {
		t.Fatalf("busy lock returned after %s, expected approximately %s", elapsed, configurationLockTimeout)
	}

	unlockFirst()
	firstHeld = false
	// Removing an unlocked lock file proves both the successful holder and the
	// timed-out contender closed their Windows handles.
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatalf("remove released lock file: %v", err)
	}
	unlockAgain, err := lockConfigurationFile(path)
	if err != nil {
		t.Fatalf("reacquire after unlock: %v", err)
	}
	unlockAgain()
}

func TestWindowsConfigurationLockRejectsNonRegularPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.Mkdir(path+".lock", 0o700); err != nil {
		t.Fatalf("create non-regular lock fixture: %v", err)
	}
	unlock, err := lockConfigurationFile(path)
	if unlock != nil {
		unlock()
		t.Fatal("non-regular lock path returned an unlock function")
	}
	if err == nil || !strings.Contains(err.Error(), "fichier régulier") {
		t.Fatalf("non-regular lock error = %v", err)
	}
}
