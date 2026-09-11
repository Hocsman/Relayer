package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Hocsman/Relayer/internal/audit"
)

var (
	errAuditJournalUnavailable = errors.New("audit journal is currently unavailable")
	errUnsupportedExportFormat = errors.New("unsupported export format: expected json or csv")
)

func (a *App) auditJournalPath() (string, error) {
	a.mu.RLock()
	configuredPath := strings.TrimSpace(a.state.Audit.Path)
	a.mu.RUnlock()

	if configuredPath != "" {
		abs, err := filepath.Abs(configuredPath)
		if err != nil {
			return "", fmt.Errorf("resolve configured audit path: %w", err)
		}
		return filepath.Clean(abs), nil
	}

	defaultPath, err := audit.DefaultPath()
	if err != nil {
		return "", fmt.Errorf("resolve default audit path: %w", err)
	}
	return filepath.Clean(defaultPath), nil
}

// GetAuditSummary computes aggregate statistics across the audit journal.
// If the journal file does not yet exist, it returns an empty report with TotalEntries=0.
func (a *App) GetAuditSummary() (AuditSummaryView, error) {
	path, err := a.auditJournalPath()
	if err != nil {
		return AuditSummaryView{}, errAuditJournalUnavailable
	}

	emptySummary := AuditSummaryView{
		Path:           path,
		TotalEntries:   0,
		AgentCounts:    make(map[string]int),
		KindCounts:     make(map[string]int),
		DecisionsCount: make(map[string]int),
		ActorsCount:    make(map[string]int),
		OutcomesCount:  make(map[string]int),
	}

	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return emptySummary, nil
		}
		return AuditSummaryView{}, errAuditJournalUnavailable
	}
	defer file.Close()

	report, err := audit.SummarizeJournal(file)
	if err != nil {
		return AuditSummaryView{}, errAuditJournalUnavailable
	}

	summary := AuditSummaryView{
		Path:           path,
		TotalEntries:   report.TotalEntries,
		RunsCount:      report.RunsCount,
		SessionsCount:  report.SessionsCount,
		AgentCounts:    make(map[string]int, len(report.AgentCounts)),
		KindCounts:     make(map[string]int, len(report.KindCounts)),
		DecisionsCount: make(map[string]int, len(report.DecisionsCount)),
		ActorsCount:    make(map[string]int, len(report.ActorsCount)),
		OutcomesCount:  make(map[string]int, len(report.OutcomesCount)),
		SensitiveCount: report.SensitiveCount,
	}

	for k, v := range report.AgentCounts {
		summary.AgentCounts[k] = v
	}
	for k, v := range report.KindCounts {
		summary.KindCounts[string(k)] = v
	}
	for k, v := range report.DecisionsCount {
		summary.DecisionsCount[string(k)] = v
	}
	for k, v := range report.ActorsCount {
		summary.ActorsCount[string(k)] = v
	}
	for k, v := range report.OutcomesCount {
		summary.OutcomesCount[string(k)] = v
	}
	if !report.FirstTimestamp.IsZero() {
		summary.FirstTimestamp = report.FirstTimestamp.UTC().Format(time.RFC3339)
	}
	if !report.LastTimestamp.IsZero() {
		summary.LastTimestamp = report.LastTimestamp.UTC().Format(time.RFC3339)
	}

	return summary, nil
}

// GetAuditEntries reads filtered audit records and maps them to display-safe DTOs.
func (a *App) GetAuditEntries(filter AuditFilterInput) ([]AuditEntryView, error) {
	path, err := a.auditJournalPath()
	if err != nil {
		return nil, errAuditJournalUnavailable
	}

	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []AuditEntryView{}, nil
		}
		return nil, errAuditJournalUnavailable
	}
	defer file.Close()

	auditFilter := audit.Filter{
		AgentID:   strings.TrimSpace(filter.AgentID),
		SessionID: strings.TrimSpace(filter.SessionID),
		RunID:     strings.TrimSpace(filter.RunID),
		Kind:      audit.Kind(strings.TrimSpace(filter.Kind)),
		Limit:     filter.Limit,
	}

	entries, err := audit.ReadEntries(file, auditFilter)
	if err != nil {
		return nil, errAuditJournalUnavailable
	}

	views := make([]AuditEntryView, len(entries))
	for index, entry := range entries {
		var formattedTime string
		if !entry.Timestamp.IsZero() {
			formattedTime = entry.Timestamp.UTC().Format(time.RFC3339)
		}

		views[index] = AuditEntryView{
			Sequence:   entry.Sequence,
			Timestamp:  formattedTime,
			EntryID:    entry.EntryID,
			RunID:      entry.RunID,
			Kind:       string(entry.Kind),
			SessionID:  entry.SessionID,
			AgentID:    entry.AgentID,
			Backend:    entry.Backend,
			Adapter:    entry.Adapter,
			EventType:  string(entry.EventType),
			Risk:       string(entry.Risk),
			Rule:       entry.Rule,
			Decision:   string(entry.Decision),
			DecisionBy: string(entry.DecisionBy),
			Outcome:    string(entry.Outcome),
			Reason:     entry.Reason,
			Summary:    entry.Summary,
			Sensitive:  entry.Sensitive,
			Metadata:   entry.Metadata,
		}
	}

	return views, nil
}

// VerifyAuditJournal inspects schema compliance, sequence continuity, and
// security redaction rules across the journal.
func (a *App) VerifyAuditJournal() (AuditVerificationView, error) {
	path, err := a.auditJournalPath()
	if err != nil {
		return AuditVerificationView{}, errAuditJournalUnavailable
	}

	emptyVerification := AuditVerificationView{
		Path:       path,
		TotalLines: 0,
		TotalRuns:  0,
		ValidLines: 0,
		Passed:     true,
		Issues:     []AuditVerificationIssueView{},
	}

	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return emptyVerification, nil
		}
		return AuditVerificationView{}, errAuditJournalUnavailable
	}
	defer file.Close()

	report, err := audit.VerifyJournal(file)
	if err != nil {
		return AuditVerificationView{}, errAuditJournalUnavailable
	}

	issues := make([]AuditVerificationIssueView, len(report.Issues))
	for index, issue := range report.Issues {
		issues[index] = AuditVerificationIssueView{
			Line:    issue.Line,
			EntryID: issue.EntryID,
			Message: issue.Message,
		}
	}

	return AuditVerificationView{
		Path:       path,
		TotalLines: report.TotalLines,
		TotalRuns:  report.TotalRuns,
		ValidLines: report.ValidLines,
		Passed:     report.Passed,
		Issues:     issues,
	}, nil
}

// ExportAuditReport generates an indented JSON or standard CSV representation
// of the entire audit journal.
func (a *App) ExportAuditReport(format string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(format))
	if normalized != "json" && normalized != "csv" {
		return "", errUnsupportedExportFormat
	}

	entries, err := a.GetAuditEntries(AuditFilterInput{Limit: 0})
	if err != nil {
		return "", err
	}

	switch normalized {
	case "json":
		data, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			return "", fmt.Errorf("marshal audit export: %w", err)
		}
		return string(data), nil

	case "csv":
		var buf bytes.Buffer
		writer := csv.NewWriter(&buf)

		header := []string{
			"sequence",
			"timestamp",
			"entry_id",
			"run_id",
			"agent_id",
			"session_id",
			"kind",
			"event_type",
			"decision",
			"decision_by",
			"outcome",
			"risk",
			"sensitive",
			"rule",
			"reason",
			"summary",
		}
		if err := writer.Write(header); err != nil {
			return "", fmt.Errorf("write csv header: %w", err)
		}

		for _, entry := range entries {
			record := []string{
				strconv.FormatUint(entry.Sequence, 10),
				entry.Timestamp,
				entry.EntryID,
				entry.RunID,
				entry.AgentID,
				entry.SessionID,
				entry.Kind,
				entry.EventType,
				entry.Decision,
				entry.DecisionBy,
				entry.Outcome,
				entry.Risk,
				strconv.FormatBool(entry.Sensitive),
				entry.Rule,
				entry.Reason,
				entry.Summary,
			}
			if err := writer.Write(record); err != nil {
				return "", fmt.Errorf("write csv record: %w", err)
			}
		}

		writer.Flush()
		if err := writer.Error(); err != nil {
			return "", fmt.Errorf("flush csv writer: %w", err)
		}

		return buf.String(), nil
	default:
		return "", errUnsupportedExportFormat
	}
}
