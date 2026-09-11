package audit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func makeTestEntry(runID string, seq uint64, t time.Time) Entry {
	return Entry{
		SchemaVersion: CurrentSchemaVersion,
		Sequence:      seq,
		Timestamp:     t,
		EntryID:       "ent-" + runID + "-" + string(rune('0'+seq)),
		RunID:         runID,
		Kind:          KindEventDetected,
		SessionID:     "sess-1",
		AgentID:       "claude",
		Decision:      DecisionAsk,
		DecisionBy:    DecisionBySystem,
		Outcome:       OutcomeDetected,
	}
}

func entriesToJSONL(t *testing.T, entries []Entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, entry := range entries {
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal test entry: %v", err)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func TestReadEntriesFilterAndLimit(t *testing.T) {
	now := time.Now().UTC()
	entries := []Entry{
		{
			SchemaVersion: CurrentSchemaVersion,
			Sequence:      1,
			Timestamp:     now.Add(-3 * time.Minute),
			EntryID:       "e1",
			RunID:         "run-1",
			Kind:          KindEventDetected,
			AgentID:       "claude",
		},
		{
			SchemaVersion: CurrentSchemaVersion,
			Sequence:      2,
			Timestamp:     now.Add(-2 * time.Minute),
			EntryID:       "e2",
			RunID:         "run-1",
			Kind:          KindPolicyEvaluated,
			AgentID:       "claude",
			Decision:      DecisionAllow,
		},
		{
			SchemaVersion: CurrentSchemaVersion,
			Sequence:      1,
			Timestamp:     now.Add(-1 * time.Minute),
			EntryID:       "e3",
			RunID:         "run-2",
			Kind:          KindEventDetected,
			AgentID:       "aider",
		},
	}

	data := entriesToJSONL(t, entries)

	// No filter
	read, err := ReadEntries(bytes.NewReader(data), Filter{})
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(read) != 3 {
		t.Fatalf("got %d entries, want 3", len(read))
	}

	// Filter by agent
	readAgent, err := ReadEntries(bytes.NewReader(data), Filter{AgentID: "aider"})
	if err != nil {
		t.Fatalf("ReadEntries by agent: %v", err)
	}
	if len(readAgent) != 1 || readAgent[0].EntryID != "e3" {
		t.Fatalf("got %#v, want e3", readAgent)
	}

	// Filter by kind
	readKind, err := ReadEntries(bytes.NewReader(data), Filter{Kind: KindPolicyEvaluated})
	if err != nil {
		t.Fatalf("ReadEntries by kind: %v", err)
	}
	if len(readKind) != 1 || readKind[0].EntryID != "e2" {
		t.Fatalf("got %#v, want e2", readKind)
	}

	// Filter by Limit (tail 2)
	readLimit, err := ReadEntries(bytes.NewReader(data), Filter{Limit: 2})
	if err != nil {
		t.Fatalf("ReadEntries limit: %v", err)
	}
	if len(readLimit) != 2 || readLimit[0].EntryID != "e2" || readLimit[1].EntryID != "e3" {
		t.Fatalf("got %#v, want e2 and e3", readLimit)
	}
}

func TestSummarizeJournal(t *testing.T) {
	now := time.Now().UTC()
	entries := []Entry{
		{
			SchemaVersion: CurrentSchemaVersion,
			Sequence:      1,
			Timestamp:     now.Add(-10 * time.Second),
			EntryID:       "e1",
			RunID:         "run-1",
			SessionID:     "sess-1",
			Kind:          KindEventDetected,
			AgentID:       "claude",
			Sensitive:     true,
		},
		{
			SchemaVersion: CurrentSchemaVersion,
			Sequence:      2,
			Timestamp:     now,
			EntryID:       "e2",
			RunID:         "run-1",
			SessionID:     "sess-1",
			Kind:          KindDecision,
			AgentID:       "claude",
			Decision:      DecisionAllow,
			DecisionBy:    DecisionByPolicy,
			Outcome:       OutcomeApplied,
		},
		{
			SchemaVersion: CurrentSchemaVersion,
			Sequence:      1,
			Timestamp:     now.Add(5 * time.Second),
			EntryID:       "e3",
			RunID:         "run-2",
			SessionID:     "sess-2",
			Kind:          KindDecision,
			AgentID:       "aider",
			Decision:      DecisionDeny,
			DecisionBy:    DecisionByHuman,
			Outcome:       OutcomeFinished,
		},
	}

	summary, err := SummarizeJournal(bytes.NewReader(entriesToJSONL(t, entries)))
	if err != nil {
		t.Fatalf("SummarizeJournal: %v", err)
	}

	if summary.TotalEntries != 3 {
		t.Errorf("TotalEntries = %d, want 3", summary.TotalEntries)
	}
	if summary.RunsCount != 2 {
		t.Errorf("RunsCount = %d, want 2", summary.RunsCount)
	}
	if summary.SessionsCount != 2 {
		t.Errorf("SessionsCount = %d, want 2", summary.SessionsCount)
	}
	if summary.AgentCounts["claude"] != 2 || summary.AgentCounts["aider"] != 1 {
		t.Errorf("AgentCounts = %#v", summary.AgentCounts)
	}
	if summary.DecisionsCount[DecisionAllow] != 1 || summary.DecisionsCount[DecisionDeny] != 1 {
		t.Errorf("DecisionsCount = %#v", summary.DecisionsCount)
	}
	if summary.ActorsCount[DecisionByPolicy] != 1 || summary.ActorsCount[DecisionByHuman] != 1 {
		t.Errorf("ActorsCount = %#v", summary.ActorsCount)
	}
	if summary.SensitiveCount != 1 {
		t.Errorf("SensitiveCount = %d, want 1", summary.SensitiveCount)
	}
}

func TestVerifyJournalValidAndCorrupted(t *testing.T) {
	now := time.Now().UTC()

	// 1. Valid journal
	validEntries := []Entry{
		makeTestEntry("run-1", 1, now),
		makeTestEntry("run-1", 2, now.Add(time.Second)),
		makeTestEntry("run-2", 1, now.Add(2*time.Second)),
	}
	report, err := VerifyJournal(bytes.NewReader(entriesToJSONL(t, validEntries)))
	if err != nil {
		t.Fatalf("VerifyJournal valid: %v", err)
	}
	if !report.Passed || len(report.Issues) != 0 || report.ValidLines != 3 {
		t.Fatalf("expected pass, got %#v", report)
	}

	// 2. Corrupted sequence (gap: 1 then 3)
	gapEntries := []Entry{
		makeTestEntry("run-1", 1, now),
		makeTestEntry("run-1", 3, now.Add(time.Second)),
	}
	gapReport, err := VerifyJournal(bytes.NewReader(entriesToJSONL(t, gapEntries)))
	if err != nil {
		t.Fatalf("VerifyJournal gap: %v", err)
	}
	if gapReport.Passed || len(gapReport.Issues) != 1 || !strings.Contains(gapReport.Issues[0].Message, "sequence gap") {
		t.Fatalf("expected sequence gap issue, got %#v", gapReport)
	}

	// 3. Schema version mismatch
	schemaEntry := makeTestEntry("run-1", 1, now)
	schemaEntry.SchemaVersion = 99
	schemaReport, err := VerifyJournal(bytes.NewReader(entriesToJSONL(t, []Entry{schemaEntry})))
	if err != nil {
		t.Fatalf("VerifyJournal schema: %v", err)
	}
	if schemaReport.Passed || len(schemaReport.Issues) != 1 || !strings.Contains(schemaReport.Issues[0].Message, "unsupported schema_version") {
		t.Fatalf("expected schema version issue, got %#v", schemaReport)
	}

	// 4. Malformed JSON line
	malformed := []byte("{\"schema_version\":1}\nnot valid json\n")
	malReport, err := VerifyJournal(bytes.NewReader(malformed))
	if err != nil {
		t.Fatalf("VerifyJournal malformed: %v", err)
	}
	if malReport.Passed || len(malReport.Issues) == 0 {
		t.Fatalf("expected failure, got %#v", malReport)
	}

	// 5. Security violation: human entry leaking summary
	humanLeak := makeTestEntry("run-1", 1, now)
	humanLeak.DecisionBy = DecisionByHuman
	humanLeak.Summary = "leaked summary text"
	leakReport, err := VerifyJournal(bytes.NewReader(entriesToJSONL(t, []Entry{humanLeak})))
	if err != nil {
		t.Fatalf("VerifyJournal leak: %v", err)
	}
	if leakReport.Passed || len(leakReport.Issues) != 1 || !strings.Contains(leakReport.Issues[0].Message, "security violation") {
		t.Fatalf("expected security violation issue, got %#v", leakReport)
	}
}
