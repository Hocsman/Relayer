package record

import (
	"bytes"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/terminal"
)

func newTestMultiplexer(t *testing.T, config Config, queueSize int) (*Multiplexer, *Store) {
	t.Helper()
	store := openTestStore(t, config)
	multiplexer := NewMultiplexer(MultiplexerOptions{
		Store:     store,
		Config:    config,
		RunID:     "run-1",
		QueueSize: queueSize,
	})
	if multiplexer == nil {
		t.Fatal("multiplexer is nil for an enabled configuration")
	}
	t.Cleanup(func() { _ = multiplexer.Close() })
	return multiplexer, store
}

func testInfo(id string) terminal.Info {
	return terminal.Info{
		ID:             id,
		Name:           "agent",
		DisplayCommand: "claude --chat",
		Backend:        "pty",
		Adapter:        "claude",
	}
}

func onlyRecording(t *testing.T, store *Store) Metadata {
	t.Helper()
	listed, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d recordings, want 1", len(listed))
	}
	return listed[0]
}

func TestNewMultiplexerIsNilWhenRecordingIsDisabled(t *testing.T) {
	config := testConfig(t)
	store := openTestStore(t, config)
	config.Enabled = false
	if NewMultiplexer(MultiplexerOptions{Store: store, Config: config}) != nil {
		t.Fatal("a disabled configuration produced a multiplexer")
	}
	config.Enabled = true
	if NewMultiplexer(MultiplexerOptions{Config: config}) != nil {
		t.Fatal("a missing store produced a multiplexer")
	}
}

func TestNilMultiplexerIsInert(t *testing.T) {
	var multiplexer *Multiplexer
	multiplexer.StartSession(testInfo("a"), terminal.Size{Columns: 80, Rows: 24}, testBase)
	multiplexer.RecordOutput("a", testBase, []byte("x"))
	multiplexer.RecordInput("a", testBase, []byte("x"))
	multiplexer.RecordResize("a", testBase, terminal.Size{Columns: 80, Rows: 24})
	multiplexer.FinishSession("a", testBase, nil)
	if err := multiplexer.Close(); err != nil {
		t.Fatalf("close a nil multiplexer: %v", err)
	}
}

func TestMultiplexerIgnoresAnUnknownSession(t *testing.T) {
	multiplexer, store := newTestMultiplexer(t, testConfig(t), 0)
	multiplexer.RecordOutput("never-started", testBase, []byte("x"))
	multiplexer.RecordInput("never-started", testBase, []byte("x"))
	multiplexer.RecordResize("never-started", testBase, terminal.Size{Columns: 80, Rows: 24})
	multiplexer.FinishSession("never-started", testBase, nil)
	if err := multiplexer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	listed, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("listed %d recordings, want 0", len(listed))
	}
}

func TestMultiplexerRecordsAWholeSession(t *testing.T) {
	multiplexer, store := newTestMultiplexer(t, testConfig(t), 0)
	multiplexer.StartSession(testInfo("session-a"), terminal.Size{Columns: 100, Rows: 30}, testBase)
	// A duplicate start must not open a second transcript.
	multiplexer.StartSession(testInfo("session-a"), terminal.Size{Columns: 100, Rows: 30}, testBase)
	for index := 0; index < 10; index++ {
		multiplexer.RecordOutput("session-a", testBase.Add(time.Duration(index)*time.Millisecond), []byte("line\r\n"))
	}
	multiplexer.RecordResize("session-a", testBase.Add(time.Second), terminal.Size{Columns: 120, Rows: 40})
	multiplexer.FinishSession("session-a", testBase.Add(2*time.Second), intPointer(3))
	if err := multiplexer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	metadata := onlyRecording(t, store)
	if metadata.Active || metadata.EndedAt.IsZero() || metadata.Truncated {
		t.Fatalf("metadata = %+v", metadata)
	}
	if metadata.ExitCode == nil || *metadata.ExitCode != 3 {
		t.Fatalf("exit code = %v", metadata.ExitCode)
	}
	if metadata.Frames != 11 {
		t.Fatalf("frames = %d, want 11", metadata.Frames)
	}
	if metadata.DroppedFrames != 0 {
		t.Fatalf("dropped = %d, want 0", metadata.DroppedFrames)
	}
	if err := ValidateFile(metadata.Path); err != nil {
		t.Fatalf("transcript is invalid: %v", err)
	}
	header, frames, _, _, err := ReadRange(metadata.Path, 0, 100)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if header.Width != 100 || header.Height != 30 || header.Title != "agent" {
		t.Fatalf("header = %+v", header)
	}
	if frames[len(frames)-1].Kind != KindResize || frames[len(frames)-1].Data != "120x40" {
		t.Fatalf("last frame = %+v", frames[len(frames)-1])
	}
}

func TestMultiplexerHonoursTheInputPolicy(t *testing.T) {
	cases := []struct {
		name        string
		recordInput bool
		redact      bool
		want        []string
	}{
		{name: "input is not recorded", recordInput: false, redact: true, want: nil},
		{name: "input is redacted", recordInput: true, redact: true, want: []string{"******\r"}},
		{name: "input is kept", recordInput: true, redact: false, want: []string{"secret\r"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := testConfig(t)
			config.RecordInput = testCase.recordInput
			config.Redact = testCase.redact
			multiplexer, store := newTestMultiplexer(t, config, 0)
			multiplexer.StartSession(testInfo("session-i"), terminal.Size{Columns: 80, Rows: 24}, testBase)
			multiplexer.RecordInput("session-i", testBase, []byte("secret\r"))
			multiplexer.FinishSession("session-i", testBase.Add(time.Second), nil)
			if err := multiplexer.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			metadata := onlyRecording(t, store)
			if metadata.InputRecorded != testCase.recordInput || metadata.Redacted != testCase.redact {
				t.Fatalf("metadata policy = %+v", metadata)
			}
			_, frames, _, _, err := ReadRange(metadata.Path, 0, 100)
			if err != nil {
				t.Fatalf("read transcript: %v", err)
			}
			var recorded []string
			for _, frame := range frames {
				if frame.Kind == KindInput {
					recorded = append(recorded, frame.Data)
				}
			}
			if len(recorded) != len(testCase.want) {
				t.Fatalf("input frames = %q, want %q", recorded, testCase.want)
			}
			for index, data := range recorded {
				if data != testCase.want[index] {
					t.Fatalf("input frame %d = %q, want %q", index, data, testCase.want[index])
				}
			}
		})
	}
}

func TestSessionQueueNeverBlocksWhenFull(t *testing.T) {
	// No consumer at all: the queue models a writer stalled on a slow disk.
	session := &recordingSession{events: make(chan recordEvent, 2)}
	const offered = 500
	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := 0; index < offered; index++ {
			session.offer(recordEvent{at: testBase, kind: KindOutput, data: []byte("x")})
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("offering to a full queue blocked the caller")
	}
	if dropped := session.takeDropped(); dropped != offered-2 {
		t.Fatalf("dropped = %d, want %d", dropped, offered-2)
	}
	if second := session.takeDropped(); second != 0 {
		t.Fatalf("dropped frames were reported twice: %d", second)
	}
	session.finish(testBase, nil)
	// A closed queue silently refuses later frames instead of panicking.
	session.offer(recordEvent{at: testBase, kind: KindOutput, data: []byte("x")})
	if dropped := session.takeDropped(); dropped != 0 {
		t.Fatalf("a closed queue counted %d drops", dropped)
	}
}

func TestMultiplexerAccountsForEveryDroppedFrame(t *testing.T) {
	var diagnostics bytes.Buffer
	config := testConfig(t)
	store := openTestStore(t, config)
	multiplexer := NewMultiplexer(MultiplexerOptions{
		Store:       store,
		Config:      config,
		RunID:       "run-1",
		Diagnostics: &diagnostics,
		QueueSize:   1,
	})
	if multiplexer == nil {
		t.Fatal("multiplexer is nil")
	}
	multiplexer.StartSession(testInfo("session-burst"), terminal.Size{Columns: 80, Rows: 24}, testBase)
	const burst = 3000
	for index := 0; index < burst; index++ {
		multiplexer.RecordOutput("session-burst", testBase.Add(time.Duration(index)*time.Microsecond), []byte("burst"))
	}
	multiplexer.FinishSession("session-burst", testBase.Add(time.Second), nil)
	if err := multiplexer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	metadata := onlyRecording(t, store)
	if metadata.Truncated {
		t.Fatalf("transcript was truncated: %+v", metadata)
	}
	_, frames, _, complete, err := ReadRange(metadata.Path, 0, burst+64)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if !complete {
		t.Fatal("transcript was not read completely")
	}
	outputs := 0
	markers := 0
	for _, frame := range frames {
		switch frame.Kind {
		case KindOutput:
			outputs++
		case KindMarker:
			markers++
			if !strings.HasPrefix(frame.Data, droppedMarkerPrefix) {
				t.Fatalf("unexpected marker %q", frame.Data)
			}
		default:
			t.Fatalf("unexpected frame %+v", frame)
		}
	}
	if outputs+metadata.DroppedFrames != burst {
		t.Fatalf("%d written + %d dropped != %d offered", outputs, metadata.DroppedFrames, burst)
	}
	if metadata.DroppedFrames > 0 && markers == 0 {
		t.Fatal("dropped frames left no marker in the transcript")
	}
	if err := ValidateFile(metadata.Path); err != nil {
		t.Fatalf("transcript is invalid: %v", err)
	}
}

func TestMultiplexerCloseJoinsEverySessionGoroutine(t *testing.T) {
	multiplexer, store := newTestMultiplexer(t, testConfig(t), 0)
	const sessions = 8
	for index := 0; index < sessions; index++ {
		id := terminal.SessionID("session-" + string(rune('a'+index)))
		multiplexer.StartSession(testInfo(string(id)), terminal.Size{Columns: 80, Rows: 24}, testBase.Add(time.Duration(index)*time.Second))
		for step := 0; step < 20; step++ {
			multiplexer.RecordOutput(id, testBase.Add(time.Duration(step)*time.Millisecond), []byte("work"))
		}
	}
	// Close without a single FinishSession: the goroutines still have to
	// drain, close their transcripts and rewrite their sidecars.
	if err := multiplexer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := multiplexer.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}

	listed, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != sessions {
		t.Fatalf("listed %d recordings, want %d", len(listed), sessions)
	}
	for _, metadata := range listed {
		if metadata.Active {
			t.Fatalf("recording %s is still open after Close", metadata.ID)
		}
		if metadata.EndedAt.IsZero() {
			t.Fatalf("recording %s was never finalized", metadata.ID)
		}
		if metadata.Frames != 20 {
			t.Fatalf("recording %s wrote %d frames, want 20", metadata.ID, metadata.Frames)
		}
		if err := ValidateFile(metadata.Path); err != nil {
			t.Fatalf("transcript %s is invalid: %v", metadata.ID, err)
		}
	}
}

func TestMultiplexerStopsRecordingAfterClose(t *testing.T) {
	multiplexer, store := newTestMultiplexer(t, testConfig(t), 0)
	if err := multiplexer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	multiplexer.StartSession(testInfo("late"), terminal.Size{Columns: 80, Rows: 24}, testBase)
	multiplexer.RecordOutput("late", testBase, []byte("x"))
	listed, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("listed %d recordings after close, want 0", len(listed))
	}
}

func TestMultiplexerCopiesTheCallerBuffer(t *testing.T) {
	config := testConfig(t)
	config.RecordInput = true
	config.Redact = false
	multiplexer, store := newTestMultiplexer(t, config, 0)
	multiplexer.StartSession(testInfo("session-alias"), terminal.Size{Columns: 80, Rows: 24}, testBase)
	// The PTY read loop reuses its buffer the instant the call returns, so a
	// retained slice would rewrite frames that are already queued.
	output := []byte("original output")
	multiplexer.RecordOutput("session-alias", testBase, output)
	for index := range output {
		output[index] = 'Z'
	}
	input := []byte("typed line")
	multiplexer.RecordInput("session-alias", testBase.Add(time.Millisecond), input)
	for index := range input {
		input[index] = 'Z'
	}
	multiplexer.FinishSession("session-alias", testBase.Add(time.Second), nil)
	if err := multiplexer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	metadata := onlyRecording(t, store)
	_, frames := readFrames(t, metadata.Path)
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	if frames[0].Kind != KindOutput || frames[0].Data != "original output" {
		t.Fatalf("output frame = %+v, want the bytes as supplied", frames[0])
	}
	if frames[1].Kind != KindInput || frames[1].Data != "typed line" {
		t.Fatalf("input frame = %+v, want the bytes as supplied", frames[1])
	}
}

func TestMultiplexerKeepsOffsetsReplayableUnderAClockStep(t *testing.T) {
	multiplexer, store := newTestMultiplexer(t, testConfig(t), 0)
	multiplexer.StartSession(testInfo("session-clock"), terminal.Size{Columns: 80, Rows: 24}, testBase)
	// A suspended host and an NTP correction both deliver instants before the
	// session base and out of order; no player can replay either.
	for _, offset := range []time.Duration{-10 * time.Second, 5 * time.Second, -100 * time.Second, time.Second} {
		multiplexer.RecordOutput("session-clock", testBase.Add(offset), []byte("step"))
	}
	multiplexer.RecordResize("session-clock", testBase.Add(-time.Hour), terminal.Size{Columns: 120, Rows: 40})
	multiplexer.FinishSession("session-clock", testBase.Add(-time.Hour), nil)
	if err := multiplexer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	metadata := onlyRecording(t, store)
	if err := ValidateFile(metadata.Path); err != nil {
		t.Fatalf("transcript is invalid: %v", err)
	}
	_, frames := readFrames(t, metadata.Path)
	previous := 0.0
	for index, frame := range frames {
		if frame.Offset < 0 {
			t.Fatalf("frame %d has a negative offset %v", index, frame.Offset)
		}
		if frame.Offset < previous {
			t.Fatalf("frame %d goes back in time: %v after %v", index, frame.Offset, previous)
		}
		previous = frame.Offset
	}
	if metadata.EndedAt.Before(metadata.StartedAt) {
		t.Fatalf("ended_at %v precedes started_at %v", metadata.EndedAt, metadata.StartedAt)
	}
}

func TestMultiplexerCloseRacesWithEveryRecordPath(t *testing.T) {
	config := testConfig(t)
	config.RecordInput = true
	multiplexer, store := newTestMultiplexer(t, config, 4)
	const sessions = 8
	const steps = 300
	var group sync.WaitGroup
	// Two shutdowns run while sessions are still being opened and fed. A queue
	// closed under a producer, a goroutine Close never joins, or a WaitGroup
	// counter raised while Close already waits on it would surface here.
	group.Add(2)
	for closer := 0; closer < 2; closer++ {
		go func() {
			defer group.Done()
			if err := multiplexer.Close(); err != nil {
				t.Errorf("concurrent close: %v", err)
			}
		}()
	}
	for index := 0; index < sessions; index++ {
		id := terminal.SessionID("session-race-" + strconv.Itoa(index))
		group.Add(1)
		go func() {
			defer group.Done()
			multiplexer.StartSession(testInfo(id), terminal.Size{Columns: 80, Rows: 24}, testBase)
			for step := 0; step < steps; step++ {
				at := testBase.Add(time.Duration(step) * time.Millisecond)
				multiplexer.RecordOutput(id, at, []byte("output"))
				multiplexer.RecordInput(id, at, []byte("in"))
				multiplexer.RecordResize(id, at, terminal.Size{Columns: 80 + step%3, Rows: 24})
			}
			multiplexer.FinishSession(id, testBase.Add(time.Second), nil)
		}()
	}
	group.Wait()
	if err := multiplexer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	listed, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, metadata := range listed {
		if metadata.Active {
			t.Fatalf("recording %s is still open after close", metadata.ID)
		}
		if metadata.EndedAt.IsZero() {
			t.Fatalf("recording %s was never finalized", metadata.ID)
		}
		if err := ValidateFile(metadata.Path); err != nil {
			t.Fatalf("transcript %s is invalid: %v", metadata.ID, err)
		}
	}
}

func TestMultiplexerLeavesNoGoroutineBehind(t *testing.T) {
	before := runtime.NumGoroutine()
	multiplexer, _ := newTestMultiplexer(t, testConfig(t), 8)
	for index := 0; index < 8; index++ {
		id := terminal.SessionID("session-leak-" + strconv.Itoa(index))
		multiplexer.StartSession(testInfo(id), terminal.Size{Columns: 80, Rows: 24}, testBase)
		for step := 0; step < 50; step++ {
			multiplexer.RecordOutput(id, testBase.Add(time.Duration(step)*time.Millisecond), []byte("work"))
		}
	}
	if err := multiplexer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Close joins every session goroutine, so the count returns on its own; the
	// retry only absorbs the scheduler's delay in reaping a finished goroutine.
	after := runtime.NumGoroutine()
	for attempt := 0; attempt < 100 && after > before; attempt++ {
		time.Sleep(10 * time.Millisecond)
		after = runtime.NumGoroutine()
	}
	if after > before {
		t.Fatalf("goroutines = %d after close, want at most %d", after, before)
	}
}

// TestStartSessionKeepsTheCommandOutOfTheHeader pins that a transcript header
// never carries the agent's argument vector. Every asciicast player displays
// the title, and the file is meant to be shareable for review; the command can
// hold a whole shell script, which the audit model excludes from a record for
// the same reason.
func TestStartSessionKeepsTheCommandOutOfTheHeader(t *testing.T) {
	store := openTestStore(t, testConfig(t))
	multiplexer := NewMultiplexer(MultiplexerOptions{
		Store:  store,
		Config: testConfig(t),
		RunID:  "run-title",
	})
	t.Cleanup(func() { _ = multiplexer.Close() })

	const secret = "SUPER_SECRET_TOKEN_IN_ARGV"
	multiplexer.StartSession(terminal.Info{
		ID:             "alpha",
		Name:           "Agent Alpha",
		DisplayCommand: `"bash" "-c" "curl -H 'Authorization: ` + secret + `'"`,
		Backend:        "pty",
		Adapter:        "generic",
	}, terminal.Size{Columns: 80, Rows: 24}, time.Unix(1700000000, 0))
	multiplexer.RecordOutput("alpha", time.Unix(1700000001, 0), []byte("hello"))
	multiplexer.FinishSession("alpha", time.Unix(1700000002, 0), nil)

	if err := multiplexer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	entries, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("recordings = %d, want 1", len(entries))
	}

	content, err := os.ReadFile(entries[0].Path)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if strings.Contains(string(content), secret) {
		t.Fatal("the transcript carries the agent's command line")
	}

	header, err := ReadHeader(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if header.Title != "Agent Alpha" {
		t.Fatalf("header title = %q, want the agent name", header.Title)
	}
}

// lifecycleLog collects every LifecycleEvent a multiplexer reports.
type lifecycleLog struct {
	mu     sync.Mutex
	events []LifecycleEvent
}

func (l *lifecycleLog) observe(event LifecycleEvent) {
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
}

func (l *lifecycleLog) snapshot() []LifecycleEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]LifecycleEvent(nil), l.events...)
}

// TestMultiplexerReportsEveryTranscriptOpeningAndClosing is what the
// recording_started and recording_finished audit records rest on: until the
// multiplexer reported its lifecycle, nothing emitted either kind.
func TestMultiplexerReportsEveryTranscriptOpeningAndClosing(t *testing.T) {
	config := testConfig(t)
	store := openTestStore(t, config)
	var log lifecycleLog
	multiplexer := NewMultiplexer(MultiplexerOptions{
		Store:     store,
		Config:    config,
		RunID:     "run-lifecycle",
		Lifecycle: log.observe,
	})
	t.Cleanup(func() { _ = multiplexer.Close() })

	info := testInfo("session-a")
	info.Name = "Agent A"
	multiplexer.StartSession(info, terminal.Size{Columns: 80, Rows: 24}, testBase)
	multiplexer.RecordOutput("session-a", testBase.Add(time.Millisecond), []byte("hello\r\n"))
	multiplexer.FinishSession("session-a", testBase.Add(time.Second), intPointer(0))

	// Close joins the session goroutine, so every report is in by now. The run
	// relies on that to journal the last one before its audit journal closes.
	if err := multiplexer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	events := log.snapshot()
	if len(events) != 2 {
		t.Fatalf("lifecycle events = %d (%+v), want an opening and a closing", len(events), events)
	}
	opened, closed := events[0], events[1]
	if opened.Finished || opened.Err != nil {
		t.Fatalf("first event = %+v, want a successful opening", opened)
	}
	if !closed.Finished || closed.Err != nil {
		t.Fatalf("second event = %+v, want a successful closing", closed)
	}
	if opened.Metadata.ID == "" || opened.Metadata.ID != closed.Metadata.ID {
		t.Fatalf("recording ids = %q then %q, want one transcript", opened.Metadata.ID, closed.Metadata.ID)
	}
	if closed.Metadata.Frames != 1 || closed.Metadata.Bytes == 0 || closed.Metadata.Active {
		t.Fatalf("closing metadata = %+v, want the final counters", closed.Metadata)
	}

	// Every other audit record names an agent by its identifier. The display
	// name used to land in AgentID, so a journal filtered by agent missed the
	// transcript's records.
	if opened.Metadata.AgentID != "session-a" || opened.Metadata.Name != "Agent A" {
		t.Fatalf("agent id = %q, name = %q, want the identifier and the display name",
			opened.Metadata.AgentID, opened.Metadata.Name)
	}
	if stored := onlyRecording(t, store); stored.AgentID != "session-a" {
		t.Fatalf("stored agent id = %q, want the identifier", stored.AgentID)
	}
}

func TestMultiplexerReportsATranscriptThatNeverOpened(t *testing.T) {
	config := testConfig(t)
	store := openTestStore(t, config)
	var log lifecycleLog
	multiplexer := NewMultiplexer(MultiplexerOptions{
		Store:     store,
		Config:    config,
		RunID:     "run-refused",
		Lifecycle: log.observe,
	})
	t.Cleanup(func() { _ = multiplexer.Close() })

	// A closed store refuses to create anything.
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	multiplexer.StartSession(testInfo("session-b"), terminal.Size{Columns: 80, Rows: 24}, testBase)
	multiplexer.FinishSession("session-b", testBase.Add(time.Second), nil)
	if err := multiplexer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	events := log.snapshot()
	if len(events) != 1 {
		t.Fatalf("lifecycle events = %+v, want exactly the refused opening", events)
	}
	if events[0].Finished || events[0].Err == nil {
		t.Fatalf("event = %+v, want a failed opening", events[0])
	}
	// No file exists, so nothing may read as though one does.
	if events[0].Metadata.SessionID != "session-b" || events[0].Metadata.ID != "" {
		t.Fatalf("metadata = %+v, want the session identity and no recording id", events[0].Metadata)
	}
}
