package config

import (
	"errors"
	"os"
	"testing"
	"time"
)

// The loop is tested through retryRename rather than publishByRename so the
// timing behaviour runs on every platform, not only the one whose filesystem
// refuses a rename. The real classifier and the real os.Rename are covered by
// the Windows test beside this file.

func TestRetryRenameReturnsOnFirstSuccess(t *testing.T) {
	calls := 0
	err := retryRename(
		func() error { calls++; return nil },
		func(error) bool { t.Fatal("a successful rename must not be classified"); return false },
		time.Second,
	)
	if err != nil {
		t.Fatalf("retryRename: %v", err)
	}
	if calls != 1 {
		t.Fatalf("attempts = %d, want 1", calls)
	}
}

func TestRetryRenameSucceedsOnceInterferenceClears(t *testing.T) {
	transient := errors.New("held by another process")
	calls := 0

	start := time.Now()
	err := retryRename(
		func() error {
			calls++
			if calls < 3 {
				return transient
			}
			return nil
		},
		func(err error) bool { return errors.Is(err, transient) },
		time.Second,
	)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("retryRename: %v", err)
	}
	if calls != 3 {
		t.Fatalf("attempts = %d, want 3", calls)
	}
	// The old budget was five attempts ten milliseconds apart. This asserts the
	// loop actually waits between attempts rather than spinning.
	if elapsed < renameBackoffStart {
		t.Fatalf("elapsed = %s, want at least one backoff of %s", elapsed, renameBackoffStart)
	}
}

func TestRetryRenameDoesNotWaitOnAPermanentFailure(t *testing.T) {
	permanent := errors.New("no such file or directory")
	calls := 0

	start := time.Now()
	err := retryRename(
		func() error { calls++; return permanent },
		func(error) bool { return false },
		10*time.Second,
	)
	elapsed := time.Since(start)

	if !errors.Is(err, permanent) {
		t.Fatalf("retryRename error = %v, want the underlying failure", err)
	}
	if calls != 1 {
		t.Fatalf("attempts = %d, want 1: a missing source does not appear by waiting", calls)
	}
	// The caller needs this report now, not after the full budget.
	if elapsed > time.Second {
		t.Fatalf("elapsed = %s, want an immediate return", elapsed)
	}
}

func TestRetryRenameGivesUpAtTheDeadlineAndReportsTheRealError(t *testing.T) {
	transient := errors.New("still held")
	calls := 0

	start := time.Now()
	err := retryRename(
		func() error { calls++; return transient },
		func(error) bool { return true },
		80*time.Millisecond,
	)
	elapsed := time.Since(start)

	if !errors.Is(err, transient) {
		t.Fatalf("retryRename error = %v, want the last underlying failure", err)
	}
	if calls < 2 {
		t.Fatalf("attempts = %d, want more than one before the deadline", calls)
	}
	if elapsed < 80*time.Millisecond {
		t.Fatalf("elapsed = %s, want the budget to be spent", elapsed)
	}
	// A bounded budget: a permanently held file must not hang the caller.
	if elapsed > 3*time.Second {
		t.Fatalf("elapsed = %s, want the deadline to bound the wait", elapsed)
	}
}

func TestRetryRenameBackoffIsBounded(t *testing.T) {
	var gaps []time.Duration
	last := time.Now()

	_ = retryRename(
		func() error {
			now := time.Now()
			gaps = append(gaps, now.Sub(last))
			last = now
			return errors.New("held")
		},
		func(error) bool { return true },
		600*time.Millisecond,
	)

	if len(gaps) < 4 {
		t.Fatalf("attempts = %d, want several", len(gaps))
	}
	// Every wait stays under the cap, so a long hold never turns into one long
	// blind sleep. The allowance absorbs scheduler jitter on a loaded runner.
	for index, gap := range gaps[1:] {
		if gap > renameBackoffMax+150*time.Millisecond {
			t.Fatalf("gap %d = %s, want at most about %s", index, gap, renameBackoffMax)
		}
	}
}

func TestPublishByRenameMovesTheFile(t *testing.T) {
	directory := t.TempDir()
	source := directory + string(os.PathSeparator) + "source.tmp"
	target := directory + string(os.PathSeparator) + "config.yaml"

	if err := os.WriteFile(source, []byte("published"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := publishByRename(source, target); err != nil {
		t.Fatalf("publishByRename: %v", err)
	}

	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(content) != "published" {
		t.Fatalf("target content = %q, want the source content", content)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the source survived the publish")
	}
}

func TestPublishByRenameReportsAMissingSourceImmediately(t *testing.T) {
	directory := t.TempDir()

	start := time.Now()
	err := publishByRename(
		directory+string(os.PathSeparator)+"absent.tmp",
		directory+string(os.PathSeparator)+"config.yaml",
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("publishByRename accepted a missing source")
	}
	if elapsed > time.Second {
		t.Fatalf("elapsed = %s, want a missing source reported without waiting out the budget", elapsed)
	}
}

func TestRetryableRenameErrorRefusesNil(t *testing.T) {
	if retryableRenameError(nil) {
		t.Fatal("a nil error is not a transient rename failure")
	}
}
