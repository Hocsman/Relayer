package server

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/config"
)

// startAuditedController boots a real run with its audit journal in detailed
// mode, the only mode that keeps metadata, so a test can read the other party
// of each hand-over back out of the journal.
func startAuditedController(t *testing.T) *Controller {
	t.Helper()

	tempDir := t.TempDir()
	t.Setenv("APPDATA", tempDir)
	t.Setenv("HOME", tempDir)
	t.Setenv("USERPROFILE", tempDir)
	t.Setenv("XDG_CONFIG_HOME", tempDir)

	configPath := filepath.Join(tempDir, "config.yaml")
	if _, err := config.LoadOrCreate(configPath); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	detailed := strings.Replace(string(raw), "mode: metadata", "mode: detailed", 1)
	if detailed == string(raw) {
		t.Fatal("the default configuration no longer names an audit mode to switch")
	}
	if err := os.WriteFile(configPath, []byte(detailed), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctrl, err := NewController(configPath, io.Discard)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := ctrl.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start controller: %v", err)
	}
	// Registered after t.TempDir, so it runs first: Windows cannot remove a
	// journal the run still holds open.
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = ctrl.Close(shutdownCtx)
		cancel()
	})
	return ctrl
}

// handRecord is the part of a journaled hand-over this test pins down.
type handRecord struct {
	kind, operator, outcome, reason, target string
}

func journaledHandovers(t *testing.T, ctrl *Controller, runID string) []handRecord {
	t.Helper()

	entries, err := ctrl.GetAuditEntries(AuditFilterInput{RunID: runID, Limit: 500})
	if err != nil {
		t.Fatalf("GetAuditEntries: %v", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Sequence < entries[j].Sequence })

	var records []handRecord
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Kind, "control_") && !strings.HasPrefix(entry.Kind, "attach_") {
			continue
		}
		if entry.Summary != "" {
			t.Errorf("%s carries a summary %q", entry.Kind, entry.Summary)
		}
		records = append(records, handRecord{
			kind:     entry.Kind,
			operator: entry.Operator,
			outcome:  entry.Outcome,
			reason:   entry.Reason,
			target:   entry.Metadata["target_operator"],
		})
	}
	return records
}

// TestEveryHandoverIsJournaled is the reason the control verbs audit at all.
// docs/sharing.md has promised since v0.7.0 that every transfer is journaled,
// and that "every use is journaled as control_forced", while no code emitted a
// single control_* record: a forced takeover left no trace.
func TestEveryHandoverIsJournaled(t *testing.T) {
	ctrl := startAuditedController(t)

	state := ctrl.GetState()
	if len(state.Agents) == 0 {
		t.Skip("the default configuration started no agent on this platform")
	}
	session := state.Agents[0].SessionID

	ctrl.RegisterPresence("conn-a", "alice", string(RoleOperator))
	ctrl.RegisterPresence("conn-b", "bob", string(RoleOperator))
	ctrl.RegisterPresence("conn-c", "carol", string(RoleOperator))

	must := func(step string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	must("alice attaches", ctrl.SetInteractiveSession(state.RunID, session, true, "alice", "conn-a"))
	_, err := ctrl.RequestControl(ctrl.GetState().RunID, session, "conn-b", "bob")
	must("bob asks", err)
	_, err = ctrl.RequestControl(ctrl.GetState().RunID, session, "conn-b", "bob")
	must("bob asks again", err)
	_, err = ctrl.DeclineControl(ctrl.GetState().RunID, session, "conn-a", "alice", "conn-b")
	must("alice declines", err)
	_, err = ctrl.RequestControl(ctrl.GetState().RunID, session, "conn-b", "bob")
	must("bob asks once more", err)
	_, err = ctrl.GrantControl(ctrl.GetState().RunID, session, "conn-a", "alice", "conn-b")
	must("alice grants", err)
	_, err = ctrl.ReleaseControl(ctrl.GetState().RunID, session, "conn-b", "bob")
	must("bob releases", err)
	_, err = ctrl.ReleaseControl(ctrl.GetState().RunID, session, "conn-b", "bob")
	must("bob releases a free terminal", err)
	_, err = ctrl.RequestControl(ctrl.GetState().RunID, session, "conn-c", "carol")
	must("carol takes the free terminal", err)

	if _, err := ctrl.ForceTakeControl(ctrl.GetState().RunID, session, "conn-a", "alice"); !errors.Is(err, ErrForceDisabled) {
		t.Fatalf("force while disabled: %v, want ErrForceDisabled", err)
	}
	ctrl.mu.Lock()
	ctrl.allowForceTakeover = true
	ctrl.mu.Unlock()
	_, err = ctrl.ForceTakeControl(ctrl.GetState().RunID, session, "conn-a", "alice")
	must("alice forces", err)

	// Detaching journals attach_finished and nothing else: one action, one
	// record.
	must("alice detaches", ctrl.SetInteractiveSession(state.RunID, session, false, "alice", "conn-a"))

	_, err = ctrl.RequestControl(ctrl.GetState().RunID, session, "conn-b", "bob")
	must("bob takes the free terminal", err)
	ctrl.ReleasePresence("conn-b")

	// A session the run never started is refused and names nothing.
	if _, err := ctrl.RequestControl(ctrl.GetState().RunID, "no-such-session", "conn-c", "carol"); !errors.Is(err, errUnknownSession) {
		t.Fatalf("request on an unknown session: %v, want errUnknownSession", err)
	}

	want := []handRecord{
		{"attach_started", "alice", "applied", "operator_interactive_attached", ""},
		{"control_requested", "bob", "pending", "control_requested", "alice"},
		{"control_declined", "alice", "applied", "control_declined", "bob"},
		{"control_requested", "bob", "pending", "control_requested", "alice"},
		{"control_granted", "alice", "applied", "control_granted", "bob"},
		{"control_released", "bob", "applied", "control_released", ""},
		{"control_requested", "carol", "applied", "control_taken_free", ""},
		{"control_forced", "alice", "failed", "control_force_disabled", "carol"},
		{"control_forced", "alice", "applied", "control_forced", "carol"},
		{"attach_finished", "alice", "applied", "operator_interactive_detached", ""},
		{"control_requested", "bob", "applied", "control_taken_free", ""},
		{"control_released", "bob", "applied", "control_released_disconnect", ""},
	}
	got := journaledHandovers(t, ctrl, state.RunID)
	if len(got) != len(want) {
		t.Fatalf("journaled %d hand-overs, want %d:\n got  %+v\n want %+v", len(got), len(want), got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("hand-over %d = %+v, want %+v", index, got[index], want[index])
		}
	}

	// The journal must still verify: the verifier refuses free-form text on
	// these kinds, and these are the first records of them ever written.
	report, err := ctrl.VerifyAuditJournal()
	if err != nil {
		t.Fatalf("VerifyAuditJournal: %v", err)
	}
	if !report.Passed {
		t.Fatalf("the journal no longer verifies: %+v", report.Issues)
	}
}
