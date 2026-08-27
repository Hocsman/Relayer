//go:build darwin || linux

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/agent"
)

func TestReplaceAgentsReturnsBusyWithinBoundWhenAnotherProcessLockIsHeld(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load default config: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	unlock, err := lockConfigurationFile(path)
	if err != nil {
		t.Fatalf("hold configuration lock: %v", err)
	}
	defer unlock()

	started := time.Now()
	_, _, replaceErr := ReplaceAgents(path, loaded.Revision, []agent.Spec{{
		ID: "agent", Name: "Agent", Command: []string{"runner"}, Backend: agent.BackendPTY,
	}})
	elapsed := time.Since(started)
	if replaceErr == nil || !strings.Contains(replaceErr.Error(), "occupée") {
		t.Fatalf("ReplaceAgents error = %v, want bounded busy error", replaceErr)
	}
	if elapsed > 2*configurationLockTimeout {
		t.Fatalf("busy lock returned after %s, bound is %s", elapsed, 2*configurationLockTimeout)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("busy update mutated configuration")
	}
}
