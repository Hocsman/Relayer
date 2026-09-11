package app

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Hocsman/Relayer/internal/audit"
)

var ErrAuditVerificationFailed = errors.New("audit verification detected issues")

func runAudit(arguments []string, output io.Writer, diagnostics io.Writer) error {
	if output == nil {
		output = io.Discard
	}
	if diagnostics == nil {
		diagnostics = io.Discard
	}

	subcommand := "show"
	var restArgs []string

	if len(arguments) > 0 {
		switch arguments[0] {
		case "show", "stats", "summary", "verify":
			subcommand = arguments[0]
			restArgs = arguments[1:]
		case "--help", "-help", "-h", "help":
			printAuditUsage(output)
			return flag.ErrHelp
		default:
			if strings.HasPrefix(arguments[0], "-") {
				subcommand = "show"
				restArgs = arguments
			} else {
				subcommand = "show"
				restArgs = arguments
			}
		}
	}

	switch subcommand {
	case "show":
		return runAuditShow(restArgs, output, diagnostics)
	case "stats", "summary":
		return runAuditStats(restArgs, output, diagnostics)
	case "verify":
		return runAuditVerify(restArgs, output, diagnostics)
	default:
		printAuditUsage(output)
		return flag.ErrHelp
	}
}

func printAuditUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Usage: relayer audit [command] [options] [path]")
	_, _ = fmt.Fprintln(w, "")
	_, _ = fmt.Fprintln(w, "Commands:")
	_, _ = fmt.Fprintln(w, "  show     Display audit entries in tabular or JSON format (default)")
	_, _ = fmt.Fprintln(w, "  stats    Display summary statistics (runs, decisions, agents)")
	_, _ = fmt.Fprintln(w, "  verify   Validate schema, sequence continuity, and integrity")
	_, _ = fmt.Fprintln(w, "")
	_, _ = fmt.Fprintln(w, "Options (show):")
	_, _ = fmt.Fprintln(w, "  --path string    Path to audit.jsonl (default: user config dir)")
	_, _ = fmt.Fprintln(w, "  --agent string   Filter entries by agent ID")
	_, _ = fmt.Fprintln(w, "  --kind string    Filter entries by audit kind")
	_, _ = fmt.Fprintln(w, "  --limit int      Limit output to last N entries (default: 50, 0=all)")
	_, _ = fmt.Fprintln(w, "  --json           Output raw JSON instead of table")
	_, _ = fmt.Fprintln(w, "")
	_, _ = fmt.Fprintln(w, "Options (stats, verify):")
	_, _ = fmt.Fprintln(w, "  --path string    Path to audit.jsonl")
	_, _ = fmt.Fprintln(w, "  --json           Output results in JSON format")
}

func resolvePathFromFlags(fs *flag.FlagSet, explicitFlag string) (string, error) {
	path := strings.TrimSpace(explicitFlag)
	if path == "" && fs.NArg() > 0 {
		path = strings.TrimSpace(fs.Arg(0))
	}
	if path == "" {
		defaultPath, err := audit.DefaultPath()
		if err != nil {
			return "", err
		}
		path = defaultPath
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	return filepath.Clean(abs), nil
}

func runAuditShow(arguments []string, output io.Writer, diagnostics io.Writer) error {
	fs := flag.NewFlagSet("relayer audit show", flag.ContinueOnError)
	fs.SetOutput(diagnostics)
	pathFlag := fs.String("path", "", "path to audit.jsonl")
	agentFlag := fs.String("agent", "", "filter by agent ID")
	kindFlag := fs.String("kind", "", "filter by kind")
	limitFlag := fs.Int("limit", 50, "limit to last N entries (0 = all)")
	jsonFlag := fs.Bool("json", false, "output JSON format")

	if err := fs.Parse(arguments); err != nil {
		return err
	}

	targetPath, err := resolvePathFromFlags(fs, *pathFlag)
	if err != nil {
		return err
	}

	file, err := os.Open(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			if *jsonFlag {
				_, _ = fmt.Fprintln(output, "[]")
			} else {
				_, _ = fmt.Fprintf(output, "No audit records found at %s\n", targetPath)
			}
			return nil
		}
		return fmt.Errorf("open audit file: %w", err)
	}
	defer file.Close()

	filter := audit.Filter{
		AgentID: strings.TrimSpace(*agentFlag),
		Kind:    audit.Kind(strings.TrimSpace(*kindFlag)),
		Limit:   *limitFlag,
	}

	entries, err := audit.ReadEntries(file, filter)
	if err != nil {
		return err
	}

	if *jsonFlag {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(entries)
	}

	if len(entries) == 0 {
		_, _ = fmt.Fprintf(output, "No audit entries match criteria in %s\n", targetPath)
		return nil
	}

	w := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "TIMESTAMP (UTC)\tAGENT\tKIND\tDECISION\tACTOR\tOUTCOME\tREASON")
	for _, entry := range entries {
		ts := entry.Timestamp.Format("2006-01-02 15:04:05")
		agentID := entry.AgentID
		if agentID == "" {
			agentID = "-"
		}
		decision := string(entry.Decision)
		if decision == "" {
			decision = "-"
		}
		actor := string(entry.DecisionBy)
		if actor == "" {
			actor = "-"
		}
		outcome := string(entry.Outcome)
		if outcome == "" {
			outcome = "-"
		}
		reason := entry.Reason
		if reason == "" {
			reason = "-"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", ts, agentID, entry.Kind, decision, actor, outcome, reason)
	}
	return w.Flush()
}

func runAuditStats(arguments []string, output io.Writer, diagnostics io.Writer) error {
	fs := flag.NewFlagSet("relayer audit stats", flag.ContinueOnError)
	fs.SetOutput(diagnostics)
	pathFlag := fs.String("path", "", "path to audit.jsonl")
	jsonFlag := fs.Bool("json", false, "output JSON format")

	if err := fs.Parse(arguments); err != nil {
		return err
	}

	targetPath, err := resolvePathFromFlags(fs, *pathFlag)
	if err != nil {
		return err
	}

	file, err := os.Open(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			_, _ = fmt.Fprintf(output, "No audit records found at %s\n", targetPath)
			return nil
		}
		return fmt.Errorf("open audit file: %w", err)
	}
	defer file.Close()

	summary, err := audit.SummarizeJournal(file)
	if err != nil {
		return err
	}
	summary.Path = targetPath

	if *jsonFlag {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}

	_, _ = fmt.Fprintf(output, "Relayer Audit Summary\n")
	_, _ = fmt.Fprintf(output, "  Journal:       %s\n", summary.Path)
	_, _ = fmt.Fprintf(output, "  Total Entries: %d\n", summary.TotalEntries)
	_, _ = fmt.Fprintf(output, "  Runs:          %d\n", summary.RunsCount)
	_, _ = fmt.Fprintf(output, "  Sessions:      %d\n", summary.SessionsCount)

	if !summary.FirstTimestamp.IsZero() && !summary.LastTimestamp.IsZero() {
		duration := summary.LastTimestamp.Sub(summary.FirstTimestamp).Round(time.Second)
		_, _ = fmt.Fprintf(output, "  Time Span:     %s to %s (%s)\n",
			summary.FirstTimestamp.Format("2006-01-02 15:04:05"),
			summary.LastTimestamp.Format("2006-01-02 15:04:05"),
			duration,
		)
	}

	if len(summary.AgentCounts) > 0 {
		_, _ = fmt.Fprintf(output, "\nAgents:\n")
		var agents []string
		for a := range summary.AgentCounts {
			agents = append(agents, a)
		}
		sort.Strings(agents)
		for _, a := range agents {
			_, _ = fmt.Fprintf(output, "  - %s: %d entries\n", a, summary.AgentCounts[a])
		}
	}

	if len(summary.DecisionsCount) > 0 {
		_, _ = fmt.Fprintf(output, "\nDecisions:\n")
		for _, d := range []audit.Decision{audit.DecisionAllow, audit.DecisionAsk, audit.DecisionDeny} {
			if count := summary.DecisionsCount[d]; count > 0 {
				_, _ = fmt.Fprintf(output, "  - %s: %d\n", d, count)
			}
		}
	}

	if len(summary.ActorsCount) > 0 {
		totalDecisions := 0
		for _, count := range summary.ActorsCount {
			totalDecisions += count
		}
		_, _ = fmt.Fprintf(output, "\nDecision Actors:\n")
		for _, actor := range []audit.DecisionBy{audit.DecisionByPolicy, audit.DecisionByHuman, audit.DecisionBySystem} {
			if count := summary.ActorsCount[actor]; count > 0 {
				pct := float64(count) / float64(totalDecisions) * 100
				_, _ = fmt.Fprintf(output, "  - %s: %d (%.1f%%)\n", actor, count, pct)
			}
		}
	}

	if summary.SensitiveCount > 0 {
		_, _ = fmt.Fprintf(output, "\nSecurity:\n")
		_, _ = fmt.Fprintf(output, "  - Sensitive events: %d\n", summary.SensitiveCount)
	}

	return nil
}

func runAuditVerify(arguments []string, output io.Writer, diagnostics io.Writer) error {
	fs := flag.NewFlagSet("relayer audit verify", flag.ContinueOnError)
	fs.SetOutput(diagnostics)
	pathFlag := fs.String("path", "", "path to audit.jsonl")
	jsonFlag := fs.Bool("json", false, "output JSON format")

	if err := fs.Parse(arguments); err != nil {
		return err
	}

	targetPath, err := resolvePathFromFlags(fs, *pathFlag)
	if err != nil {
		return err
	}

	file, err := os.Open(targetPath)
	if err != nil {
		return fmt.Errorf("open audit file: %w", err)
	}
	defer file.Close()

	report, err := audit.VerifyJournal(file)
	if err != nil {
		return err
	}
	report.Path = targetPath

	if *jsonFlag {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return err
		}
		if !report.Passed {
			return ErrAuditVerificationFailed
		}
		return nil
	}

	_, _ = fmt.Fprintf(output, "Relayer Audit Verification\n")
	_, _ = fmt.Fprintf(output, "  Journal:      %s\n", report.Path)
	_, _ = fmt.Fprintf(output, "  Total Lines:  %d\n", report.TotalLines)
	_, _ = fmt.Fprintf(output, "  Valid Lines:  %d\n", report.ValidLines)
	_, _ = fmt.Fprintf(output, "  Runs Checked: %d\n", report.TotalRuns)

	if report.Passed {
		_, _ = fmt.Fprintf(output, "  Status:       PASSED (0 issues)\n")
		return nil
	}

	_, _ = fmt.Fprintf(output, "  Status:       FAILED (%d issue(s) detected)\n\nIssues:\n", len(report.Issues))
	for _, issue := range report.Issues {
		entryInfo := ""
		if issue.EntryID != "" {
			entryInfo = fmt.Sprintf(" [%s]", issue.EntryID)
		}
		_, _ = fmt.Fprintf(output, "  - Line %d%s: %s\n", issue.Line, entryInfo, issue.Message)
	}

	return ErrAuditVerificationFailed
}
