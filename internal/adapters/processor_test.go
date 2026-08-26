package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type snapshotReplayFixture struct {
	OldLog        string `json:"old_log"`
	Prompt        string `json:"prompt"`
	Cleared       string `json:"cleared"`
	SameNewPrompt string `json:"same_new_prompt"`
}

func TestProcessorReconcilesSnapshotIdempotentlyAcrossAckAndNewOccurrence(t *testing.T) {
	fixture := loadSnapshotReplayFixture(t)
	processor := newGenericTestProcessor(t, 4096, Hooks{})

	if event, changed, err := processor.ReconcileSnapshot([]byte(fixture.OldLog)); err != nil || event != nil || changed {
		t.Fatalf("historical baseline = event %#v changed %t error %v", event, changed, err)
	}
	first, changed, err := processor.ReconcileSnapshot([]byte(fixture.Prompt))
	if err != nil || first == nil || !changed {
		t.Fatalf("first prompt snapshot = event %#v changed %t error %v", first, changed, err)
	}
	if first.Sequence != 1 || first.ID == "" || first.Signature == "" {
		t.Fatalf("first occurrence identity = %#v", first)
	}
	firstID := first.ID
	firstSignature := first.Signature
	first.Metadata["pattern"] = "caller mutation"
	if pending := processor.Pending(); pending == nil || pending.ID != firstID || pending.Metadata["pattern"] != "overwrite" {
		t.Fatalf("snapshot result aliases pending state: %#v", pending)
	}

	repeated, changed, err := processor.ReconcileSnapshot([]byte("\x1b[31m" + fixture.Prompt + "\x1b[0m"))
	if err != nil || repeated == nil || repeated.ID != firstID || changed {
		t.Fatalf("same ANSI-decorated snapshot = event %#v changed %t error %v", repeated, changed, err)
	}
	if err := processor.Acknowledge("another-event"); !errors.Is(err, ErrEventMismatch) {
		t.Fatalf("mismatched acknowledgement error = %v", err)
	}
	if pending := processor.Pending(); pending == nil || pending.ID != firstID {
		t.Fatalf("mismatched acknowledgement cleared pending event: %#v", pending)
	}
	if err := processor.Acknowledge(firstID); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if processor.Pending() != nil || processor.IsBlocked() {
		t.Fatalf("acknowledged occurrence remains pending: %#v", processor.Pending())
	}

	// The identical retained snapshot is old evidence and must not resurrect
	// the occurrence that was just acknowledged.
	if event, changed, err := processor.ReconcileSnapshot([]byte(fixture.Prompt)); err != nil || event != nil || changed {
		t.Fatalf("same snapshot after ack = event %#v changed %t error %v", event, changed, err)
	}
	if event, changed, err := processor.ReconcileSnapshot([]byte(fixture.Cleared)); err != nil || event != nil || changed {
		t.Fatalf("cleared snapshot = event %#v changed %t error %v", event, changed, err)
	}

	second, changed, err := processor.ReconcileSnapshot([]byte(fixture.SameNewPrompt))
	if err != nil || second == nil || !changed {
		t.Fatalf("new identical occurrence = event %#v changed %t error %v", second, changed, err)
	}
	if second.Signature != firstSignature || second.ID == firstID || second.Sequence != 2 {
		t.Fatalf("new identical occurrence identity = first %#v second %#v", first, second)
	}
	if event, changed, err := processor.ReconcileSnapshot([]byte(fixture.OldLog)); err != nil || event != nil || !changed {
		t.Fatalf("authoritative snapshot clearing pending = event %#v changed %t error %v", event, changed, err)
	}
}

func TestProcessorSnapshotFingerprintIncludesActiveCodeFenceContext(t *testing.T) {
	processor := newGenericTestProcessor(t, 4096, Hooks{})
	const prompt = "Overwrite sample? [Y/n]"
	if event, changed, err := processor.ReconcileSnapshot([]byte("```text\n" + prompt)); err != nil || event != nil || changed {
		t.Fatalf("fenced snapshot = event %#v changed %t error %v", event, changed, err)
	}
	event, changed, err := processor.ReconcileSnapshot([]byte("ready\n" + prompt))
	if err != nil || event == nil || !changed || !event.Actionable() {
		t.Fatalf("same active line outside fence = event %#v changed %t error %v", event, changed, err)
	}
}

func TestProcessorEmitsTwoIdenticalStreamPromptsAsDistinctOccurrences(t *testing.T) {
	var events []Event
	processor := newGenericTestProcessor(t, 4096, Hooks{
		OnEvent: func(event Event) { events = append(events, event) },
	})
	prompt := []byte("Overwrite current file? [Y/n]")
	if err := processor.Consume(prompt); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("first stream events = %#v", events)
	}
	if err := processor.Acknowledge(events[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := processor.Consume([]byte("\ndecision recorded\n")); err != nil {
		t.Fatal(err)
	}
	if err := processor.Consume(prompt); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("identical stream prompt events = %#v", events)
	}
	if events[0].Signature != events[1].Signature || events[0].ID == events[1].ID ||
		events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatalf("occurrence identities = first %#v second %#v", events[0], events[1])
	}
}

func TestProcessorProcessExitAtomicallyClearsPendingAndAdvancesSequence(t *testing.T) {
	var events []Event
	processor := newGenericTestProcessor(t, 4096, Hooks{
		OnEvent: func(event Event) { events = append(events, event) },
	})
	if err := processor.Consume([]byte("Overwrite current file? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	pending := processor.Pending()
	if pending == nil {
		t.Fatal("prompt was not pending before process exit")
	}
	code := 7
	exit := processor.NewProcessExitEvent(&code, true)
	if processor.Pending() != nil || processor.IsBlocked() {
		t.Fatalf("process exit retained an actionable prompt: %#v", processor.Pending())
	}
	if exit.Type != EventProcessExit || exit.Sequence != pending.Sequence+1 ||
		exit.ID == pending.ID || exit.Metadata["exit_code"] != "7" ||
		exit.Metadata["failed"] != "true" {
		t.Fatalf("process exit occurrence = %#v after %#v", exit, pending)
	}
	if repeated := processor.NewProcessExitEvent(nil, false); repeated.ID != exit.ID || repeated.Sequence != exit.Sequence {
		t.Fatalf("repeated process exit changed identity: first %#v repeated %#v", exit, repeated)
	}
	eventsBeforeLateOutput := len(events)
	revision := processor.Revision()
	if err := processor.Consume([]byte("\nOverwrite late output? [Y/n]")); err != nil {
		t.Fatalf("late output Consume: %v", err)
	}
	if len(events) != eventsBeforeLateOutput || processor.Pending() != nil || processor.Revision() != revision {
		t.Fatalf("late output reopened semantic state: events %#v pending %#v revision %d/%d",
			events, processor.Pending(), processor.Revision(), revision)
	}
	if !strings.Contains(processor.Output(), "Overwrite late output? [Y/n]") {
		t.Fatalf("late output was not retained for rendering: %q", processor.Output())
	}
	if reconciled, changed, err := processor.ReconcileSnapshot([]byte("Overwrite late output? [Y/n]")); err != nil || reconciled != nil || changed {
		t.Fatalf("terminated snapshot reconcile = event %#v changed %t error %v", reconciled, changed, err)
	}
	if err := processor.Restore(*pending); !errors.Is(err, ErrProcessorTerminated) {
		t.Fatalf("Restore after process exit error = %v", err)
	}
	delivered := false
	if err := processor.Resolve(pending.ID, func() error { delivered = true; return nil }); !errors.Is(err, ErrProcessorTerminated) || delivered {
		t.Fatalf("Resolve after process exit = delivered %t error %v", delivered, err)
	}
}

func TestProcessorOrdersEarlierSemanticHookBeforeProcessExitReturns(t *testing.T) {
	hookStarted := make(chan struct{})
	releaseHook := make(chan struct{})
	processor := newGenericTestProcessor(t, 4096, Hooks{
		OnEvent: func(Event) {
			close(hookStarted)
			<-releaseHook
		},
	})
	consumeDone := make(chan error, 1)
	go func() {
		consumeDone <- processor.Consume([]byte("Overwrite current file? [Y/n]"))
	}()
	select {
	case <-hookStarted:
	case <-time.After(time.Second):
		t.Fatal("semantic hook did not start")
	}

	exitDone := make(chan Event, 1)
	go func() { exitDone <- processor.NewProcessExitEvent(nil, false) }()
	select {
	case event := <-exitDone:
		t.Fatalf("process exit overtook an earlier semantic hook: %#v", event)
	case <-time.After(40 * time.Millisecond):
	}
	close(releaseHook)
	select {
	case err := <-consumeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Consume did not finish after releasing hook")
	}
	select {
	case event := <-exitDone:
		if event.Type != EventProcessExit || event.Sequence != 2 {
			t.Fatalf("ordered process exit = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("process exit did not finish after earlier hook")
	}
}

func TestProcessorRestorePreservesExactOccurrenceAndHooksReceiveCopiesOutsideLock(t *testing.T) {
	var (
		processor   *Processor
		received    Event
		hookPending *Event
	)
	adapter, err := NewGenericRegexAdapter(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	state := NewDetectionState("session-a", "agent-a", GenericID)
	processor, err = NewProcessor(adapter, state, 4096, Hooks{
		OnEvent: func(event Event) {
			received = event
			hookPending = processor.Pending()
			event.Metadata["pattern"] = "mutated in hook"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.Consume([]byte("Overwrite current file? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	if hookPending == nil || hookPending.ID != received.ID {
		t.Fatalf("hook could not read pending state outside processor lock: %#v", hookPending)
	}
	if pending := processor.Pending(); pending.Metadata["pattern"] != "overwrite" {
		t.Fatalf("hook mutation leaked into pending metadata: %#v", pending)
	}
	if err := processor.Acknowledge(received.ID); err != nil {
		t.Fatal(err)
	}
	if err := processor.Restore(received); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if pending := processor.Pending(); pending == nil || !reflect.DeepEqual(*pending, received) {
		t.Fatalf("restored occurrence = %#v, want %#v", pending, received)
	}
	different := received.Clone()
	different.ID = "evt-different"
	if err := processor.Restore(different); !errors.Is(err, ErrEventMismatch) {
		t.Fatalf("restore over different pending occurrence error = %v", err)
	}
	if pending := processor.Pending(); pending == nil || pending.ID != received.ID {
		t.Fatalf("failed Restore replaced pending occurrence: %#v", pending)
	}
}

func TestProcessorBoundsRenderedOutputDetectionWindowAndMalformedANSIState(t *testing.T) {
	const capacity = 64
	adapter, err := NewGenericRegexAdapter(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	processor, err := NewProcessor(
		adapter,
		NewDetectionState("session-a", "agent-a", GenericID),
		capacity,
		Hooks{OnEvent: func(event Event) { events = append(events, event) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	stream := strings.Repeat("0123456789\n", 4000)
	if err := processor.Consume([]byte(stream)); err != nil {
		t.Fatal(err)
	}
	if got := processor.Output(); len(got) != capacity || got != stream[len(stream)-capacity:] {
		t.Fatalf("bounded output len=%d value=%q", len(got), got)
	}
	if got := processor.DetectionWindowLen(); got > detectionWindowSize {
		t.Fatalf("detection window grew to %d, limit %d", got, detectionWindowSize)
	}

	if err := processor.Consume([]byte("\x1b]" + strings.Repeat("x", maxANSICarrySize+32))); err != nil {
		t.Fatal(err)
	}
	if err := processor.Consume([]byte("\nPassword:")); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != EventCredential || !events[0].Sensitive {
		t.Fatalf("processor did not recover after malformed ANSI: %#v", events)
	}
	if got := processor.DetectionWindowLen(); got > detectionWindowSize {
		t.Fatalf("post-recovery detection window grew to %d", got)
	}
}

func TestNewProcessorValidationAndRunContracts(t *testing.T) {
	state := NewDetectionState("session-a", "agent-a", GenericID)
	adapter, err := NewGenericRegexAdapter(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewProcessor(nil, state, 32, Hooks{}); err == nil {
		t.Fatal("nil adapter was accepted")
	}
	if _, err := NewProcessor(adapter, nil, 32, Hooks{}); err == nil {
		t.Fatal("nil state was accepted")
	}
	if _, err := NewProcessor(adapter, NewDetectionState("session-a", "agent-a", "different"), 32, Hooks{}); err == nil {
		t.Fatal("adapter/state mismatch was accepted")
	}
	blankAdapterState := NewDetectionState("session-a", "agent-a", "")
	processor, err := NewProcessor(adapter, blankAdapterState, 32, Hooks{})
	if err != nil || blankAdapterState.AdapterID != GenericID {
		t.Fatalf("blank adapter state resolution = state %#v error %v", blankAdapterState, err)
	}
	if err := processor.Run(context.Background(), strings.NewReader("ordinary output")); err != nil {
		t.Fatalf("Run through EOF: %v", err)
	}
	readErr := errors.New("synthetic read failure")
	if err := processor.Run(context.Background(), readerFunc(func([]byte) (int, error) { return 0, readErr })); !errors.Is(err, readErr) {
		t.Fatalf("Run unexpected error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := processor.Run(cancelled, readerFunc(func([]byte) (int, error) { return 0, readErr })); err != nil {
		t.Fatalf("Run returned read error after cancellation: %v", err)
	}
}

func TestProcessorIgnoresEmptyInputAndSafelyNormalizesInvalidUTF8(t *testing.T) {
	var events []Event
	processor := newGenericTestProcessor(t, 32, Hooks{
		OnEvent: func(event Event) { events = append(events, event) },
	})
	if err := processor.Consume(nil); err != nil {
		t.Fatalf("empty Consume: %v", err)
	}
	if processor.Output() != "" || len(events) != 0 {
		t.Fatalf("empty input changed state: output %q events %#v", processor.Output(), events)
	}
	if err := processor.Consume([]byte{0xff, 0xfe, '\n'}); err != nil {
		t.Fatalf("invalid UTF-8 Consume: %v", err)
	}
	if len(events) != 0 || processor.DetectionWindowLen() > detectionWindowSize || len(processor.Output()) > 32 {
		t.Fatalf("invalid input produced unsafe state: output %q events %#v window %d",
			processor.Output(), events, processor.DetectionWindowLen())
	}
}

func newGenericTestProcessor(t *testing.T, capacity int, hooks Hooks) *Processor {
	t.Helper()
	adapter, err := NewGenericRegexAdapter(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	processor, err := NewProcessor(
		adapter,
		NewDetectionState("session-a", "agent-a", GenericID),
		capacity,
		hooks,
	)
	if err != nil {
		t.Fatal(err)
	}
	return processor
}

func loadSnapshotReplayFixture(t *testing.T) snapshotReplayFixture {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", "generic", "snapshot_replay.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture snapshotReplayFixture
	if err := json.Unmarshal(payload, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.OldLog == "" || fixture.Prompt == "" || fixture.Cleared == "" || fixture.SameNewPrompt == "" {
		t.Fatalf("incomplete snapshot replay fixture: %#v", fixture)
	}
	return fixture
}

type readerFunc func([]byte) (int, error)

func (read readerFunc) Read(buffer []byte) (int, error) { return read(buffer) }

var _ io.Reader = readerFunc(nil)
