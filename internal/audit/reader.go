package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const maxAuditLineBuffer = 1024 * 1024 // 1 MB max line length

// Filter specifies criteria to restrict which audit entries are returned.
type Filter struct {
	AgentID   string
	SessionID string
	RunID     string
	Kind      Kind
	Limit     int
	Since     time.Time
}

// Matches reports whether an Entry satisfies all criteria in Filter.
func (f Filter) Matches(entry Entry) bool {
	if f.AgentID != "" && !strings.EqualFold(entry.AgentID, f.AgentID) {
		return false
	}
	if f.SessionID != "" && entry.SessionID != f.SessionID {
		return false
	}
	if f.RunID != "" && entry.RunID != f.RunID {
		return false
	}
	if f.Kind != "" && entry.Kind != f.Kind {
		return false
	}
	if !f.Since.IsZero() && entry.Timestamp.Before(f.Since) {
		return false
	}
	return true
}

// ReadEntries reads and parses audit records from r applying filter.
// If Limit is specified, only the last Limit matching records are returned.
func ReadEntries(r io.Reader, filter Filter) ([]Entry, error) {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, maxAuditLineBuffer)

	var matches []Entry
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if len(line) == 0 {
			continue
		}

		var entry Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("line %d: parse audit entry: %w", lineNumber, err)
		}

		if filter.Matches(entry) {
			matches = append(matches, entry)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read audit stream: %w", err)
	}

	if filter.Limit > 0 && len(matches) > filter.Limit {
		matches = matches[len(matches)-filter.Limit:]
	}

	return matches, nil
}

// ReadEntriesFromFile reads audit entries from path.
func ReadEntriesFromFile(path string, filter Filter) ([]Entry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return ReadEntries(file, filter)
}

// SummaryReport provides aggregate statistics over an audit journal.
type SummaryReport struct {
	Path           string             `json:"path,omitempty"`
	TotalEntries   int                `json:"total_entries"`
	RunsCount      int                `json:"runs_count"`
	SessionsCount  int                `json:"sessions_count"`
	AgentCounts    map[string]int     `json:"agent_counts"`
	KindCounts     map[Kind]int       `json:"kind_counts"`
	DecisionsCount map[Decision]int   `json:"decisions_count"`
	ActorsCount    map[DecisionBy]int `json:"actors_count"`
	OutcomesCount  map[Outcome]int    `json:"outcomes_count"`
	SensitiveCount int                `json:"sensitive_count"`
	FirstTimestamp time.Time          `json:"first_timestamp,omitempty"`
	LastTimestamp  time.Time          `json:"last_timestamp,omitempty"`
}

// SummarizeJournal computes aggregate statistics across all records in r.
func SummarizeJournal(r io.Reader) (SummaryReport, error) {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, maxAuditLineBuffer)

	report := SummaryReport{
		AgentCounts:    make(map[string]int),
		KindCounts:     make(map[Kind]int),
		DecisionsCount: make(map[Decision]int),
		ActorsCount:    make(map[DecisionBy]int),
		OutcomesCount:  make(map[Outcome]int),
	}

	distinctRuns := make(map[string]struct{})
	distinctSessions := make(map[string]struct{})
	lineNumber := 0

	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if len(line) == 0 {
			continue
		}

		var entry Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return report, fmt.Errorf("line %d: parse audit entry: %w", lineNumber, err)
		}

		report.TotalEntries++
		if entry.RunID != "" {
			distinctRuns[entry.RunID] = struct{}{}
		}
		if entry.SessionID != "" {
			distinctSessions[entry.SessionID] = struct{}{}
		}
		if entry.AgentID != "" {
			report.AgentCounts[entry.AgentID]++
		}
		if entry.Kind != "" {
			report.KindCounts[entry.Kind]++
		}
		if entry.Decision != "" {
			report.DecisionsCount[entry.Decision]++
		}
		if entry.DecisionBy != "" {
			report.ActorsCount[entry.DecisionBy]++
		}
		if entry.Outcome != "" {
			report.OutcomesCount[entry.Outcome]++
		}
		if entry.Sensitive {
			report.SensitiveCount++
		}

		if !entry.Timestamp.IsZero() {
			if report.FirstTimestamp.IsZero() || entry.Timestamp.Before(report.FirstTimestamp) {
				report.FirstTimestamp = entry.Timestamp
			}
			if report.LastTimestamp.IsZero() || entry.Timestamp.After(report.LastTimestamp) {
				report.LastTimestamp = entry.Timestamp
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return report, fmt.Errorf("read audit stream: %w", err)
	}

	report.RunsCount = len(distinctRuns)
	report.SessionsCount = len(distinctSessions)
	return report, nil
}

// VerificationIssue describes a single structural or integrity failure.
type VerificationIssue struct {
	Line    int    `json:"line"`
	EntryID string `json:"entry_id,omitempty"`
	Message string `json:"message"`
}

// VerificationReport details the integrity status of an audit journal.
type VerificationReport struct {
	Path       string              `json:"path,omitempty"`
	TotalLines int                 `json:"total_lines"`
	TotalRuns  int                 `json:"total_runs"`
	ValidLines int                 `json:"valid_lines"`
	Issues     []VerificationIssue `json:"issues"`
	Passed     bool                `json:"passed"`
}

// VerifyJournal verifies sequence continuity, schema compliance and security rules.
func VerifyJournal(r io.Reader) (VerificationReport, error) {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, maxAuditLineBuffer)

	report := VerificationReport{}
	runsSeen := make(map[string]struct{})
	expectedSeqByRun := make(map[string]uint64)
	lastTimeByRun := make(map[string]time.Time)

	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		report.TotalLines++
		line := strings.TrimSpace(scanner.Text())
		if len(line) == 0 {
			continue
		}

		var entry Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			report.Issues = append(report.Issues, VerificationIssue{
				Line:    lineNumber,
				Message: fmt.Sprintf("malformed JSON: %v", err),
			})
			continue
		}

		hasError := false

		// Check schema version
		if entry.SchemaVersion != CurrentSchemaVersion {
			report.Issues = append(report.Issues, VerificationIssue{
				Line:    lineNumber,
				EntryID: entry.EntryID,
				Message: fmt.Sprintf("unsupported schema_version %d (expected %d)", entry.SchemaVersion, CurrentSchemaVersion),
			})
			hasError = true
		}

		// Check mandatory identity fields
		if strings.TrimSpace(entry.EntryID) == "" {
			report.Issues = append(report.Issues, VerificationIssue{
				Line:    lineNumber,
				Message: "missing entry_id",
			})
			hasError = true
		}
		if strings.TrimSpace(entry.RunID) == "" {
			report.Issues = append(report.Issues, VerificationIssue{
				Line:    lineNumber,
				EntryID: entry.EntryID,
				Message: "missing run_id",
			})
			hasError = true
		}
		if entry.Kind == "" || safeKind(entry.Kind) != entry.Kind || entry.Kind == KindUnknown {
			report.Issues = append(report.Issues, VerificationIssue{
				Line:    lineNumber,
				EntryID: entry.EntryID,
				Message: fmt.Sprintf("invalid kind %q", entry.Kind),
			})
			hasError = true
		}
		if entry.Timestamp.IsZero() {
			report.Issues = append(report.Issues, VerificationIssue{
				Line:    lineNumber,
				EntryID: entry.EntryID,
				Message: "missing or zero timestamp",
			})
			hasError = true
		}

		// Check sequence monotonicity per run
		if entry.RunID != "" {
			runsSeen[entry.RunID] = struct{}{}
			expected, exists := expectedSeqByRun[entry.RunID]
			if !exists {
				if entry.Sequence != 1 {
					report.Issues = append(report.Issues, VerificationIssue{
						Line:    lineNumber,
						EntryID: entry.EntryID,
						Message: fmt.Sprintf("run %s: first sequence must be 1, got %d", entry.RunID, entry.Sequence),
					})
					hasError = true
				}
				expectedSeqByRun[entry.RunID] = entry.Sequence + 1
			} else {
				if entry.Sequence != expected {
					report.Issues = append(report.Issues, VerificationIssue{
						Line:    lineNumber,
						EntryID: entry.EntryID,
						Message: fmt.Sprintf("run %s: sequence gap or reorder (expected %d, got %d)", entry.RunID, expected, entry.Sequence),
					})
					hasError = true
				}
				expectedSeqByRun[entry.RunID] = entry.Sequence + 1
			}

			// Check timestamp monotonicity per run
			lastTime, timeExists := lastTimeByRun[entry.RunID]
			if timeExists && entry.Timestamp.Before(lastTime) {
				report.Issues = append(report.Issues, VerificationIssue{
					Line:    lineNumber,
					EntryID: entry.EntryID,
					Message: fmt.Sprintf("run %s: timestamp regressed from %s to %s", entry.RunID, lastTime.Format(time.RFC3339), entry.Timestamp.Format(time.RFC3339)),
				})
				hasError = true
			}
			lastTimeByRun[entry.RunID] = entry.Timestamp
		}

		// Security invariants: human decision entries must have empty summary & metadata
		if entry.DecisionBy == DecisionByHuman {
			if entry.Summary != "" {
				report.Issues = append(report.Issues, VerificationIssue{
					Line:    lineNumber,
					EntryID: entry.EntryID,
					Message: "security violation: human decision entry contains non-empty summary",
				})
				hasError = true
			}
			if len(entry.Metadata) > 0 {
				report.Issues = append(report.Issues, VerificationIssue{
					Line:    lineNumber,
					EntryID: entry.EntryID,
					Message: "security violation: human decision entry contains non-empty metadata",
				})
				hasError = true
			}
		}

		if !hasError {
			report.ValidLines++
		}
	}

	if err := scanner.Err(); err != nil {
		return report, fmt.Errorf("read audit stream: %w", err)
	}

	report.TotalRuns = len(runsSeen)
	report.Passed = len(report.Issues) == 0
	return report, nil
}
