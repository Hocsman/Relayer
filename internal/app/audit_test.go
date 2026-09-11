package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/audit"
)

func createTestAuditJournal(t *testing.T, entries []audit.Entry) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	var buf bytes.Buffer
	for _, entry := range entries {
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal entry: %v", err)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}

	if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
		t.Fatalf("write audit file: %v", err)
	}
	return path
}

func sampleAuditEntries() []audit.Entry {
	now := time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)
	return []audit.Entry{
		{
			SchemaVersion: 1,
			Sequence:      1,
			Timestamp:     now,
			EntryID:       "ent-1",
			RunID:         "run-1",
			Kind:          audit.KindRunStarted,
			Outcome:       audit.OutcomeStarted,
		},
		{
			SchemaVersion: 1,
			Sequence:      2,
			Timestamp:     now.Add(time.Second),
			EntryID:       "ent-2",
			RunID:         "run-1",
			Kind:          audit.KindEventDetected,
			SessionID:     "sess-1",
			AgentID:       "claude",
			Outcome:       audit.OutcomeDetected,
		},
		{
			SchemaVersion: 1,
			Sequence:      3,
			Timestamp:     now.Add(2 * time.Second),
			EntryID:       "ent-3",
			RunID:         "run-1",
			Kind:          audit.KindDecision,
			SessionID:     "sess-1",
			AgentID:       "claude",
			Decision:      audit.DecisionAllow,
			DecisionBy:    audit.DecisionByPolicy,
			Outcome:       audit.OutcomeApplied,
		},
		{
			SchemaVersion: 1,
			Sequence:      1,
			Timestamp:     now.Add(10 * time.Second),
			EntryID:       "ent-4",
			RunID:         "run-2",
			Kind:          audit.KindDecision,
			SessionID:     "sess-2",
			AgentID:       "aider",
			Decision:      audit.DecisionDeny,
			DecisionBy:    audit.DecisionByHuman,
			Outcome:       audit.OutcomeFinished,
		},
	}
}

func TestAuditHelp(t *testing.T) {
	var out, diag bytes.Buffer
	err := runAudit([]string{"--help"}, &out, &diag)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("expected ErrHelp, got: %v", err)
	}
	if !strings.Contains(out.String(), "Usage: relayer audit") {
		t.Fatalf("expected usage text, got: %s", out.String())
	}
}

func TestAuditShow(t *testing.T) {
	path := createTestAuditJournal(t, sampleAuditEntries())

	// Default table output
	var out, diag bytes.Buffer
	err := runAudit([]string{"show", "--path", path}, &out, &diag)
	if err != nil {
		t.Fatalf("runAudit show: %v", err)
	}
	output := out.String()
	if !strings.Contains(output, "claude") || !strings.Contains(output, "aider") {
		t.Fatalf("expected agent names in output: %s", output)
	}
	if !strings.Contains(output, "TIMESTAMP") {
		t.Fatalf("expected header in output: %s", output)
	}

	// Filter by agent
	out.Reset()
	err = runAudit([]string{"show", "--path", path, "--agent", "aider"}, &out, &diag)
	if err != nil {
		t.Fatalf("runAudit show filter: %v", err)
	}
	output = out.String()
	if strings.Contains(output, "claude") || !strings.Contains(output, "aider") {
		t.Fatalf("expected only aider in output: %s", output)
	}

	// JSON format
	out.Reset()
	err = runAudit([]string{"show", "--path", path, "--json"}, &out, &diag)
	if err != nil {
		t.Fatalf("runAudit show json: %v", err)
	}
	var entries []audit.Entry
	if err := json.Unmarshal(out.Bytes(), &entries); err != nil {
		t.Fatalf("parse JSON output: %v, raw: %s", err, out.String())
	}
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4", len(entries))
	}
}

func TestAuditStats(t *testing.T) {
	path := createTestAuditJournal(t, sampleAuditEntries())

	var out, diag bytes.Buffer
	err := runAudit([]string{"stats", "--path", path}, &out, &diag)
	if err != nil {
		t.Fatalf("runAudit stats: %v", err)
	}
	output := out.String()
	if !strings.Contains(output, "Total Entries: 4") || !strings.Contains(output, "Runs:          2") {
		t.Fatalf("stats output unexpected: %s", output)
	}
	if !strings.Contains(output, "claude: 2") || !strings.Contains(output, "aider: 1") {
		t.Fatalf("stats agent counts unexpected: %s", output)
	}

	// JSON format
	out.Reset()
	err = runAudit([]string{"stats", "--path", path, "--json"}, &out, &diag)
	if err != nil {
		t.Fatalf("runAudit stats json: %v", err)
	}
	var summary audit.SummaryReport
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("parse stats JSON: %v", err)
	}
	if summary.TotalEntries != 4 || summary.RunsCount != 2 {
		t.Fatalf("summary JSON unexpected: %#v", summary)
	}
}

func TestAuditVerify(t *testing.T) {
	path := createTestAuditJournal(t, sampleAuditEntries())

	var out, diag bytes.Buffer
	err := runAudit([]string{"verify", "--path", path}, &out, &diag)
	if err != nil {
		t.Fatalf("runAudit verify valid: %v", err)
	}
	if !strings.Contains(out.String(), "PASSED") {
		t.Fatalf("expected PASSED, got: %s", out.String())
	}

	// Corrupted journal
	corrupted := filepath.Join(t.TempDir(), "corrupted.jsonl")
	badData := []byte("{\"schema_version\":1,\"sequence\":1,\"run_id\":\"r1\",\"entry_id\":\"e1\",\"kind\":\"run_started\",\"timestamp\":\"2026-09-10T18:00:00Z\"}\n{\"schema_version\":1,\"sequence\":3,\"run_id\":\"r1\",\"entry_id\":\"e2\",\"kind\":\"decision\",\"timestamp\":\"2026-09-10T18:00:01Z\"}\n")
	if err := os.WriteFile(corrupted, badData, 0600); err != nil {
		t.Fatalf("write bad data: %v", err)
	}

	out.Reset()
	err = runAudit([]string{"verify", "--path", corrupted}, &out, &diag)
	if !errors.Is(err, ErrAuditVerificationFailed) {
		t.Fatalf("expected ErrAuditVerificationFailed, got: %v", err)
	}
	if !strings.Contains(out.String(), "FAILED") || !strings.Contains(out.String(), "sequence gap") {
		t.Fatalf("expected FAILED with sequence gap: %s", out.String())
	}
}

func TestAuditNonExistentFile(t *testing.T) {
	missingPath := filepath.Join(t.TempDir(), "nonexistent.jsonl")

	// show returns friendly message and nil error
	var out, diag bytes.Buffer
	err := runAudit([]string{"show", "--path", missingPath}, &out, &diag)
	if err != nil {
		t.Fatalf("expected nil error for missing show: %v", err)
	}
	if !strings.Contains(out.String(), "No audit records found") {
		t.Fatalf("expected 'No audit records found', got: %s", out.String())
	}

	// verify returns error for missing file
	out.Reset()
	err = runAudit([]string{"verify", "--path", missingPath}, &out, &diag)
	if err == nil {
		t.Fatalf("expected error for missing verify file")
	}
}
