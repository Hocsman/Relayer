package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
)

type memoryLineSink struct {
	mu         sync.Mutex
	lines      [][]byte
	writeErr   error
	closeErr   error
	writeCalls int
	closeCalls int
}

func (s *memoryLineSink) WriteLine(line []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeCalls++
	if s.writeErr != nil {
		return s.writeErr
	}
	s.lines = append(s.lines, append([]byte(nil), line...))
	return nil
}

func (s *memoryLineSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls++
	return s.closeErr
}

func (s *memoryLineSink) snapshot() ([][]byte, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lines := make([][]byte, len(s.lines))
	for index, line := range s.lines {
		lines[index] = append([]byte(nil), line...)
	}
	return lines, s.writeCalls, s.closeCalls
}

func TestDefaultConfigValidationAndDisabledRecorderAreSafe(t *testing.T) {
	defaults := DefaultConfig()
	if defaults != (Config{Enabled: true, Mode: ModeMetadata, MaxFileSizeMB: 10, MaxFiles: 5}) {
		t.Fatalf("DefaultConfig() = %#v", defaults)
	}
	if err := Validate(defaults); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "mode", mutate: func(config *Config) { config.Mode = "verbose" }},
		{name: "nul path", mutate: func(config *Config) { config.Path = "bad\x00path" }},
		{name: "size zero", mutate: func(config *Config) { config.MaxFileSizeMB = 0 }},
		{name: "size negative", mutate: func(config *Config) { config.MaxFileSizeMB = -1 }},
		{name: "files zero", mutate: func(config *Config) { config.MaxFiles = 0 }},
		{name: "files excessive", mutate: func(config *Config) { config.MaxFiles = maximumMaxFiles + 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := defaults
			test.mutate(&config)
			if err := Validate(config); err == nil {
				t.Fatalf("Validate accepted %#v", config)
			}
		})
	}

	for _, config := range []Config{
		{Enabled: false, Mode: ModeMetadata, MaxFileSizeMB: 1, MaxFiles: 1},
		{Enabled: true, Mode: ModeOff, MaxFileSizeMB: 1, MaxFiles: 1},
	} {
		idCalls := 0
		recorder, err := NewRecorder(config, nil, nil, func() (string, error) {
			idCalls++
			return "must-not-run", nil
		})
		if err != nil || recorder.Enabled() || recorder.RunID() != "" {
			t.Fatalf("disabled recorder = %#v, %v", recorder, err)
		}
		if err := recorder.Record(Entry{Summary: "ignored-secret"}); err != nil {
			t.Fatal(err)
		}
		if err := recorder.Close(); err != nil {
			t.Fatal(err)
		}
		if idCalls != 0 {
			t.Fatalf("disabled recorder generated %d identifiers", idCalls)
		}
	}
}

func TestRecorderWritesVersionedOrderedJSONLAndCopiesMetadata(t *testing.T) {
	sink := &memoryLineSink{}
	now := time.Date(2026, 8, 26, 12, 34, 56, 789, time.FixedZone("test", 2*60*60))
	var idCounter int
	recorder, err := NewRecorder(
		Config{Enabled: true, Mode: ModeDetailed, Path: "memory.jsonl", MaxFileSizeMB: 1, MaxFiles: 1},
		sink,
		func() time.Time { return now },
		func() (string, error) {
			idCounter++
			return fmt.Sprintf("audit-id-%d", idCounter), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if recorder.RunID() != "audit-id-1" || recorder.Path() != "memory.jsonl" || !recorder.Enabled() {
		t.Fatalf("recorder identity = run %q path %q enabled %t", recorder.RunID(), recorder.Path(), recorder.Enabled())
	}

	metadata := map[string]string{"automatic": "true", "mode": "enforce"}
	entry := Entry{
		SchemaVersion: 99,
		Sequence:      999,
		Timestamp:     time.Unix(1, 0),
		EntryID:       "caller-entry",
		RunID:         "caller-run",
		Kind:          KindPolicyEvaluated,
		SessionID:     "session-a",
		AgentID:       "agent-a",
		Backend:       "pty",
		Adapter:       "generic",
		EventID:       "evt-1",
		EventType:     adapters.EventConfirmation,
		Risk:          adapters.RiskLow,
		Rule:          "safe-rule",
		Decision:      DecisionAllow,
		DecisionBy:    DecisionByPolicy,
		Outcome:       OutcomeApplied,
		Reason:        "rule_match",
		Summary:       "quoted \"summary\"\nsecond line",
		Metadata:      metadata,
	}
	if err := recorder.Record(entry); err != nil {
		t.Fatal(err)
	}
	metadata["automatic"] = "mutated"
	metadata["new"] = "mutated"
	if err := recorder.Record(Entry{Kind: KindRunFinished, Outcome: OutcomeFinished}); err != nil {
		t.Fatal(err)
	}

	lines, _, _ := sink.snapshot()
	if len(lines) != 2 {
		t.Fatalf("JSONL lines = %d", len(lines))
	}
	for index, line := range lines {
		if len(line) == 0 || line[len(line)-1] != '\n' {
			t.Fatalf("line %d not newline terminated: %q", index, line)
		}
		var decoded Entry
		if err := json.Unmarshal(line, &decoded); err != nil {
			t.Fatalf("line %d invalid JSON: %v: %q", index, err, line)
		}
		if decoded.SchemaVersion != CurrentSchemaVersion || decoded.Sequence != uint64(index+1) ||
			decoded.RunID != "audit-id-1" || decoded.EntryID != fmt.Sprintf("audit-id-%d", index+2) ||
			!decoded.Timestamp.Equal(now.UTC()) || decoded.Timestamp.Location() != time.UTC {
			t.Fatalf("line %d identity/order = %#v", index, decoded)
		}
	}
	var first Entry
	if err := json.Unmarshal(lines[0], &first); err != nil {
		t.Fatal(err)
	}
	if first.Metadata["automatic"] != "true" || first.Metadata["mode"] != "enforce" || len(first.Metadata) != 2 {
		t.Fatalf("metadata was aliased or not sanitized: %#v", first.Metadata)
	}
	if first.Summary != `quoted "summary" second line` {
		t.Fatalf("summary = %q", first.Summary)
	}
}

func TestRecorderWriteFailureIsStickyAndCloseReturnsIt(t *testing.T) {
	writeErr := errors.New("simulated disk failure")
	closeErr := errors.New("simulated close failure")
	sink := &memoryLineSink{writeErr: writeErr, closeErr: closeErr}
	recorder, err := NewRecorder(
		Config{Enabled: true, Mode: ModeMetadata, MaxFileSizeMB: 1, MaxFiles: 1},
		sink, nil, sequentialIDGenerator(),
	)
	if err != nil {
		t.Fatal(err)
	}
	first := recorder.Record(Entry{Kind: KindBackendError})
	second := recorder.Record(Entry{Kind: KindBackendError})
	if !errors.Is(first, writeErr) || !errors.Is(second, writeErr) {
		t.Fatalf("sticky errors = first %v second %v", first, second)
	}
	_, writes, _ := sink.snapshot()
	if writes != 1 {
		t.Fatalf("failed sink called %d times", writes)
	}
	if err := recorder.Close(); !errors.Is(err, writeErr) || !errors.Is(err, closeErr) {
		t.Fatalf("Close error = %v", err)
	}
	if err := recorder.Close(); !errors.Is(err, writeErr) || !errors.Is(err, closeErr) {
		t.Fatalf("second Close error = %v", err)
	}
	_, _, closes := sink.snapshot()
	if closes != 1 {
		t.Fatalf("sink closed %d times", closes)
	}
}

func TestRecorderConcurrentAgentsAndCloseHaveOneTotalOrder(t *testing.T) {
	sink := &memoryLineSink{}
	var nextID atomic.Uint64
	recorder, err := NewRecorder(
		Config{Enabled: true, Mode: ModeDetailed, MaxFileSizeMB: 1, MaxFiles: 1},
		sink,
		func() time.Time { return time.Unix(100, 0) },
		func() (string, error) { return fmt.Sprintf("id-%d", nextID.Add(1)), nil },
	)
	if err != nil {
		t.Fatal(err)
	}

	const agents = 8
	const perAgent = 30
	start := make(chan struct{})
	var group sync.WaitGroup
	var accepted atomic.Int64
	var unexpected atomic.Value
	for agentIndex := 0; agentIndex < agents; agentIndex++ {
		agentIndex := agentIndex
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			for index := 0; index < perAgent; index++ {
				err := recorder.Record(Entry{
					Kind:      KindEventDetected,
					SessionID: fmt.Sprintf("session-%d", agentIndex),
					AgentID:   fmt.Sprintf("agent-%d", agentIndex),
					EventID:   "shared-event-id",
					Summary:   fmt.Sprintf("index-%d", index),
				})
				if err == nil {
					accepted.Add(1)
					continue
				}
				if !errors.Is(err, ErrClosed) {
					unexpected.Store(err)
				}
				return
			}
		}()
	}
	close(start)
	group.Wait()

	const closers = 32
	closeResults := make(chan error, closers)
	for index := 0; index < closers; index++ {
		go func() { closeResults <- recorder.Close() }()
	}
	for index := 0; index < closers; index++ {
		if err := <-closeResults; err != nil {
			t.Fatal(err)
		}
	}
	if value := unexpected.Load(); value != nil {
		t.Fatalf("unexpected Record error: %v", value)
	}
	if err := recorder.Record(Entry{Kind: KindRunFinished}); !errors.Is(err, ErrClosed) {
		t.Fatalf("post-close Record error = %v", err)
	}

	lines, _, closes := sink.snapshot()
	if int64(len(lines)) != accepted.Load() || closes != 1 {
		t.Fatalf("accepted=%d lines=%d closes=%d", accepted.Load(), len(lines), closes)
	}
	lastByAgent := make(map[string]int)
	for index, line := range lines {
		var entry Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Sequence != uint64(index+1) {
			t.Fatalf("line %d sequence = %d", index, entry.Sequence)
		}
		var agentIndex int
		if _, err := fmt.Sscanf(entry.Summary, "index-%d", &agentIndex); err != nil {
			t.Fatal(err)
		}
		if previous, exists := lastByAgent[entry.AgentID]; exists && agentIndex != previous+1 {
			t.Fatalf("per-agent order for %s: %d after %d", entry.AgentID, agentIndex, previous)
		}
		lastByAgent[entry.AgentID] = agentIndex
	}
}

func TestRecorderRecordRacingCloseIsLinearizable(t *testing.T) {
	sink := &memoryLineSink{}
	var nextID atomic.Uint64
	recorder, err := NewRecorder(
		Config{Enabled: true, Mode: ModeDetailed, MaxFileSizeMB: 1, MaxFiles: 1},
		sink,
		func() time.Time { return time.Unix(100, 0) },
		func() (string, error) { return fmt.Sprintf("race-id-%d", nextID.Add(1)), nil },
	)
	if err != nil {
		t.Fatal(err)
	}

	const writers = 128
	start := make(chan struct{})
	results := make(chan error, writers)
	var group sync.WaitGroup
	for index := 0; index < writers; index++ {
		index := index
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			results <- recorder.Record(Entry{
				Kind:      KindEventDetected,
				AgentID:   fmt.Sprintf("agent-%d", index%8),
				SessionID: fmt.Sprintf("session-%d", index%8),
				Metadata:  map[string]string{"writer": fmt.Sprint(index)},
			})
		}()
	}
	closeResult := make(chan error, 1)
	group.Add(1)
	go func() {
		defer group.Done()
		<-start
		closeResult <- recorder.Close()
	}()
	close(start)
	group.Wait()
	close(results)
	if err := <-closeResult; err != nil {
		t.Fatal(err)
	}

	accepted := 0
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrClosed):
		default:
			t.Fatalf("unexpected Record error: %v", err)
		}
	}
	lines, _, closes := sink.snapshot()
	if len(lines) != accepted || closes != 1 {
		t.Fatalf("accepted=%d lines=%d closes=%d", accepted, len(lines), closes)
	}
	for index, entry := range decodeEntries(t, lines) {
		if entry.Sequence != uint64(index+1) {
			t.Fatalf("line %d sequence = %d", index, entry.Sequence)
		}
	}
}

func TestRecorderRawJSONContainsNoForbiddenPayloadFieldsOrSentinels(t *testing.T) {
	sink := &memoryLineSink{}
	recorder, err := NewRecorder(
		Config{Enabled: true, Mode: ModeDetailed, MaxFileSizeMB: 1, MaxFiles: 1},
		sink, nil, sequentialIDGenerator(),
	)
	if err != nil {
		t.Fatal(err)
	}
	entry := Entry{
		Kind:       KindDecision,
		SessionID:  "session-a",
		AgentID:    "agent-a",
		EventID:    "event-a",
		EventType:  adapters.EventCredential,
		Decision:   DecisionAsk,
		DecisionBy: DecisionByHuman,
		Summary:    "manual-input-sentinel",
		Metadata: map[string]string{
			"match":         "prompt-match-sentinel",
			"input":         "manual-input-sentinel",
			"env":           "environment-sentinel",
			"error":         "raw-error-sentinel",
			"Authorization": "Bearer bearer-secret-sentinel",
		},
	}
	if err := recorder.Record(entry); err != nil {
		t.Fatal(err)
	}
	lines, _, _ := sink.snapshot()
	if len(lines) != 1 {
		t.Fatalf("lines = %d", len(lines))
	}
	raw := string(lines[0])
	for _, forbidden := range []string{
		`"match"`, `"input"`, `"env"`, `"error"`, `"event_id"`,
		"prompt-match-sentinel", "manual-input-sentinel", "environment-sentinel",
		"raw-error-sentinel", "bearer-secret-sentinel",
	} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("forbidden marker %q survived raw JSONL: %s", forbidden, raw)
		}
	}
}

func TestRecorderRejectsInvalidGeneratedIDsWithoutWriting(t *testing.T) {
	for _, generated := range []string{"", "id with spaces", "secret/token", "\x00"} {
		t.Run(fmt.Sprintf("%q", generated), func(t *testing.T) {
			sink := &memoryLineSink{}
			_, err := NewRecorder(
				Config{Enabled: true, Mode: ModeMetadata, MaxFileSizeMB: 1, MaxFiles: 1},
				sink, nil, func() (string, error) { return generated, nil },
			)
			if err == nil {
				t.Fatal("invalid run ID accepted")
			}
			if lines, writes, _ := sink.snapshot(); len(lines) != 0 || writes != 0 {
				t.Fatalf("invalid generator wrote %#v", lines)
			}
		})
	}
}

func sequentialIDGenerator() func() (string, error) {
	var counter int
	return func() (string, error) {
		counter++
		return fmt.Sprintf("id-%d", counter), nil
	}
}

func decodeEntries(t *testing.T, lines [][]byte) []Entry {
	t.Helper()
	entries := make([]Entry, len(lines))
	for index, line := range lines {
		if err := json.Unmarshal(line, &entries[index]); err != nil {
			t.Fatal(err)
		}
	}
	return entries
}

var _ LineSink = (*memoryLineSink)(nil)
