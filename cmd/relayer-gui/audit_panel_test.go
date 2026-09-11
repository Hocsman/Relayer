package main

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
)

func TestAuditPanel_NonExistentFile(t *testing.T) {
	tempDir := t.TempDir()
	missingPath := filepath.Join(tempDir, "does-not-exist.jsonl")

	app := NewApp()
	app.state.Audit.Path = missingPath

	// GetAuditSummary
	summary, err := app.GetAuditSummary()
	if err != nil {
		t.Fatalf("GetAuditSummary failed on missing file: %v", err)
	}
	if summary.TotalEntries != 0 {
		t.Errorf("expected 0 entries, got %d", summary.TotalEntries)
	}
	if summary.Path != missingPath {
		t.Errorf("expected path %q, got %q", missingPath, summary.Path)
	}

	// GetAuditEntries
	entries, err := app.GetAuditEntries(AuditFilterInput{})
	if err != nil {
		t.Fatalf("GetAuditEntries failed on missing file: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}

	// VerifyAuditJournal
	verification, err := app.VerifyAuditJournal()
	if err != nil {
		t.Fatalf("VerifyAuditJournal failed on missing file: %v", err)
	}
	if !verification.Passed {
		t.Errorf("expected verification to pass on missing file, got false")
	}
	if verification.TotalLines != 0 {
		t.Errorf("expected 0 total lines, got %d", verification.TotalLines)
	}

	// ExportAuditReport JSON
	jsonExport, err := app.ExportAuditReport("json")
	if err != nil {
		t.Fatalf("ExportAuditReport(json) failed: %v", err)
	}
	if strings.TrimSpace(jsonExport) != "[]" {
		t.Errorf("expected '[]', got %q", jsonExport)
	}

	// ExportAuditReport CSV
	csvExport, err := app.ExportAuditReport("csv")
	if err != nil {
		t.Fatalf("ExportAuditReport(csv) failed: %v", err)
	}
	r := csv.NewReader(strings.NewReader(csvExport))
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("failed to read csv export: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("expected 1 line (header), got %d", len(records))
	}
	if records[0][0] != "sequence" {
		t.Errorf("expected sequence header, got %q", records[0][0])
	}

	// Unsupported format
	_, err = app.ExportAuditReport("xml")
	if err != errUnsupportedExportFormat {
		t.Errorf("expected errUnsupportedExportFormat, got %v", err)
	}
}

func TestAuditPanel_ValidJournal(t *testing.T) {
	tempDir := t.TempDir()
	journalPath := filepath.Join(tempDir, "audit.jsonl")

	baseTime := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	testEntries := []audit.Entry{
		{
			SchemaVersion: audit.CurrentSchemaVersion,
			Sequence:      1,
			Timestamp:     baseTime,
			EntryID:       "ent-1",
			RunID:         "run-1",
			Kind:          audit.KindRunStarted,
			DecisionBy:    audit.DecisionBySystem,
			Outcome:       audit.OutcomeStarted,
		},
		{
			SchemaVersion: audit.CurrentSchemaVersion,
			Sequence:      2,
			Timestamp:     baseTime.Add(1 * time.Second),
			EntryID:       "ent-2",
			RunID:         "run-1",
			SessionID:     "sess-1",
			AgentID:       "claude-code",
			Backend:       "pty",
			Adapter:       "claude",
			Kind:          audit.KindPolicyEvaluated,
			EventType:     adapters.EventPermission,
			Risk:          adapters.RiskHigh,
			Rule:          "deny_curl",
			Decision:      audit.DecisionDeny,
			DecisionBy:    audit.DecisionByPolicy,
			Outcome:       audit.OutcomeInFlight,
			Reason:        "curl command matched restricted pattern",
			Sensitive:     true,
			Metadata:      map[string]string{"cmd": "curl"},
		},
		{
			SchemaVersion: audit.CurrentSchemaVersion,
			Sequence:      3,
			Timestamp:     baseTime.Add(2 * time.Second),
			EntryID:       "ent-3",
			RunID:         "run-1",
			SessionID:     "sess-1",
			AgentID:       "claude-code",
			Backend:       "pty",
			Adapter:       "claude",
			Kind:          audit.KindDecision,
			EventType:     adapters.EventPermission,
			Decision:      audit.DecisionAllow,
			DecisionBy:    audit.DecisionByHuman,
			Outcome:       audit.OutcomeApplied,
			Reason:        "decision_selected",
		},
	}

	file, err := os.Create(journalPath)
	if err != nil {
		t.Fatalf("failed to create temp journal: %v", err)
	}
	for _, entry := range testEntries {
		line, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("failed to marshal entry: %v", err)
		}
		if _, err := file.Write(append(line, '\n')); err != nil {
			t.Fatalf("failed to write entry: %v", err)
		}
	}
	file.Close()

	app := NewApp()
	app.state.Audit.Path = journalPath

	// 1. GetAuditSummary
	summary, err := app.GetAuditSummary()
	if err != nil {
		t.Fatalf("GetAuditSummary failed: %v", err)
	}
	if summary.TotalEntries != 3 {
		t.Errorf("expected 3 total entries, got %d", summary.TotalEntries)
	}
	if summary.RunsCount != 1 {
		t.Errorf("expected 1 run, got %d", summary.RunsCount)
	}
	if summary.SessionsCount != 1 {
		t.Errorf("expected 1 session, got %d", summary.SessionsCount)
	}
	if summary.AgentCounts["claude-code"] != 2 {
		t.Errorf("expected 2 claude-code entries, got %d", summary.AgentCounts["claude-code"])
	}
	if summary.ActorsCount[string(audit.DecisionByHuman)] != 1 {
		t.Errorf("expected 1 human decision, got %d", summary.ActorsCount[string(audit.DecisionByHuman)])
	}
	if summary.ActorsCount[string(audit.DecisionByPolicy)] != 1 {
		t.Errorf("expected 1 policy decision, got %d", summary.ActorsCount[string(audit.DecisionByPolicy)])
	}
	if summary.SensitiveCount != 1 {
		t.Errorf("expected 1 sensitive entry, got %d", summary.SensitiveCount)
	}

	// 2. GetAuditEntries (All)
	allEntries, err := app.GetAuditEntries(AuditFilterInput{})
	if err != nil {
		t.Fatalf("GetAuditEntries failed: %v", err)
	}
	if len(allEntries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(allEntries))
	}
	if allEntries[0].EntryID != "ent-1" || allEntries[0].Sequence != 1 {
		t.Errorf("unexpected first entry: %+v", allEntries[0])
	}
	if allEntries[1].Sensitive != true || allEntries[1].Rule != "deny_curl" {
		t.Errorf("unexpected second entry: %+v", allEntries[1])
	}
	if allEntries[2].DecisionBy != string(audit.DecisionByHuman) || allEntries[2].Decision != string(audit.DecisionAllow) {
		t.Errorf("unexpected third entry: %+v", allEntries[2])
	}

	// 3. GetAuditEntries with filters
	filteredEntries, err := app.GetAuditEntries(AuditFilterInput{
		AgentID: "claude-code",
		Limit:   1,
	})
	if err != nil {
		t.Fatalf("GetAuditEntries with filter failed: %v", err)
	}
	if len(filteredEntries) != 1 {
		t.Fatalf("expected 1 filtered entry, got %d", len(filteredEntries))
	}
	if filteredEntries[0].EntryID != "ent-3" {
		t.Errorf("expected ent-3 (latest limit=1), got %q", filteredEntries[0].EntryID)
	}

	// 4. VerifyAuditJournal
	verification, err := app.VerifyAuditJournal()
	if err != nil {
		t.Fatalf("VerifyAuditJournal failed: %v", err)
	}
	if !verification.Passed {
		t.Fatalf("expected verification to pass, but failed with issues: %+v", verification.Issues)
	}
	if verification.ValidLines != 3 {
		t.Errorf("expected 3 valid lines, got %d", verification.ValidLines)
	}

	// 5. Export JSON
	jsonExport, err := app.ExportAuditReport("json")
	if err != nil {
		t.Fatalf("ExportAuditReport(json) failed: %v", err)
	}
	var exportedViews []AuditEntryView
	if err := json.Unmarshal([]byte(jsonExport), &exportedViews); err != nil {
		t.Fatalf("failed to unmarshal exported json: %v", err)
	}
	if len(exportedViews) != 3 {
		t.Errorf("expected 3 views in json, got %d", len(exportedViews))
	}

	// 6. Export CSV
	csvExport, err := app.ExportAuditReport("csv")
	if err != nil {
		t.Fatalf("ExportAuditReport(csv) failed: %v", err)
	}
	csvRecords, err := csv.NewReader(strings.NewReader(csvExport)).ReadAll()
	if err != nil {
		t.Fatalf("failed to read exported csv: %v", err)
	}
	if len(csvRecords) != 4 { // Header + 3 entries
		t.Fatalf("expected 4 csv rows, got %d", len(csvRecords))
	}
	// Verify row 2 (ent-2) has correct fields
	row2 := csvRecords[2]
	if row2[0] != "2" || row2[2] != "ent-2" || row2[4] != "claude-code" || row2[12] != "true" || row2[13] != "deny_curl" {
		t.Errorf("unexpected csv row 2 content: %v", row2)
	}
}

func TestAuditPanel_CorruptedJournal(t *testing.T) {
	tempDir := t.TempDir()
	journalPath := filepath.Join(tempDir, "corrupted.jsonl")

	// Sequence gap: sequence 1 then sequence 3
	lines := []string{
		`{"schema_version":1,"sequence":1,"timestamp":"2026-09-11T12:00:00Z","entry_id":"ent-1","run_id":"run-1","kind":"run_started"}`,
		`{"schema_version":1,"sequence":3,"timestamp":"2026-09-11T12:00:01Z","entry_id":"ent-2","run_id":"run-1","kind":"session_started"}`,
	}

	if err := os.WriteFile(journalPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("failed to write corrupted journal: %v", err)
	}

	app := NewApp()
	app.state.Audit.Path = journalPath

	verification, err := app.VerifyAuditJournal()
	if err != nil {
		t.Fatalf("VerifyAuditJournal returned error: %v", err)
	}
	if verification.Passed {
		t.Errorf("expected verification to fail on sequence gap, but passed")
	}
	if len(verification.Issues) == 0 {
		t.Errorf("expected at least one issue, got 0")
	}
}
