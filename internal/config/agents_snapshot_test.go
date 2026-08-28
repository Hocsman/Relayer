package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Hocsman/Relayer/internal/agent"
)

func TestFileSnapshotRestoresExactConfigurationAndMode(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	original := []byte("# retained comment\nversion: 1\nbackend: pty\nagents:\n  - id: first\n    name: First\n    command: [fixture]\n    cwd: .\nintercept_patterns:\n  - pattern: '(?i)confirm'\n    description: confirmation\n")
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := CaptureFileSnapshot(path)
	if err != nil {
		t.Fatalf("CaptureFileSnapshot: %v", err)
	}
	defer snapshot.Discard()

	current, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	updated, candidateRevision, err := ReplaceAgents(path, current.Revision, []agent.Spec{{
		ID: "second", Name: "Second", Command: []string{"fixture"}, Cwd: directory, Backend: agent.BackendPTY,
	}})
	if err != nil {
		t.Fatalf("ReplaceAgents: %v", err)
	}
	if updated.Revision != candidateRevision || candidateRevision == snapshot.Revision() {
		t.Fatalf("candidate revisions = %q / %q, snapshot = %q", updated.Revision, candidateRevision, snapshot.Revision())
	}

	restored, revision, err := snapshot.Restore(candidateRevision)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if revision != snapshot.Revision() || restored.Revision != snapshot.Revision() {
		t.Fatalf("restored revisions = %q / %q, want %q", revision, restored.Revision, snapshot.Revision())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("restored bytes differ:\n%s", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if gotMode := info.Mode().Perm(); gotMode != originalInfo.Mode().Perm() {
		t.Fatalf("restored mode = %04o, want preserved mode %04o", gotMode, originalInfo.Mode().Perm())
	}
}

func TestFileSnapshotRestoreRejectsNewerEditAndDiscard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("version: 1\nbackend: pty\nagents: []\nintercept_patterns:\n  - pattern: '(?i)confirm'\n    description: confirmation\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := CaptureFileSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	newer := append([]byte("# newer edit\n"), original...)
	if err := os.WriteFile(path, newer, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := snapshot.Restore(snapshot.Revision()); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("Restore stale error = %v, want ErrRevisionMismatch", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newer) {
		t.Fatal("stale restore mutated the newer edit")
	}
	snapshot.Discard()
	if snapshot.Revision() != "" {
		t.Fatal("Discard retained a revision")
	}
	if _, _, err := snapshot.Restore(contentRevision(newer)); err == nil {
		t.Fatal("Restore succeeded after Discard")
	}
}
