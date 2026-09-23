package supervise_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// fakeEngine is the desktop's test engine (cmd/relayer-gui/app_test.go),
// reduced to what the core calls. Its knobs block, fail or reorder each call
// the way the desktop's concurrency tests need.
type fakeEngine struct {
	mu sync.Mutex

	// staleExits makes MarkProcessExited report every exit as belonging to a
	// process a replacement already superseded.
	staleExits bool

	supportedDecisions []adapters.Decision
	evaluation         policy.Evaluation
	// evaluationByID overrides evaluation for one event ID.
	evaluationByID map[string]policy.Evaluation
	pending        map[string]*adapters.Event

	applyCalls []applyCall
	// applyErrs are returned by the first calls, in order, before applyErr.
	applyErrs         []error
	applyErr          error
	beforeApplyReturn func()
	applyStarted      chan struct{}
	applyRelease      <-chan struct{}

	lineCalls []lineCall
	lineErr   error
	// lineStarted receives the session of each line the moment it is written;
	// lineRelease, when set, holds the write until it is closed.
	lineStarted chan string
	lineRelease <-chan struct{}

	auditEntries []audit.Entry
	auditCalls   int
	auditFailAt  int
	// auditBlockKind holds each entry of that kind, before it is journaled,
	// until auditRelease is closed; auditStarted is signalled as each waits.
	auditBlockKind audit.Kind
	auditStarted   chan struct{}
	auditRelease   <-chan struct{}

	stopErr           error
	stopCalls         []string
	stopStarted       chan string
	stopRelease       <-chan struct{}
	agentStartErr     error
	agentStartCalls   []string
	agentStartStarted chan string
	agentStartRelease <-chan struct{}
	agentRestartErr   error
	agentRestartCalls []string

	operations []string
}

type applyCall struct {
	sessionID   string
	event       adapters.Event
	decision    adapters.Decision
	manualInput string
}

type lineCall struct {
	sessionID string
	line      string
}

var _ supervise.Engine = (*fakeEngine)(nil)

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		evaluation: policy.Evaluation{
			Action:         policy.ActionAsk,
			ProposedAction: policy.ActionAsk,
			Reason:         policy.ReasonDefault,
		},
		evaluationByID: make(map[string]policy.Evaluation),
		pending:        make(map[string]*adapters.Event),
	}
}

// automaticAllow is the evaluation of a rule that allows without asking.
func automaticAllow() policy.Evaluation {
	return policy.Evaluation{
		Action:         policy.ActionAllow,
		ProposedAction: policy.ActionAllow,
		RuleName:       "allow-safe",
		Reason:         policy.ReasonRule,
		Automatic:      true,
	}
}

func (f *fakeEngine) Evaluate(event adapters.Event) policy.Evaluation {
	f.mu.Lock()
	defer f.mu.Unlock()
	evaluation, found := f.evaluationByID[event.ID]
	if !found {
		evaluation = f.evaluation
	}
	evaluation.EventID = event.ID
	return evaluation
}

func (f *fakeEngine) SupportedDecisions(adapters.Event) []adapters.Decision {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]adapters.Decision(nil), f.supportedDecisions...)
}

func (f *fakeEngine) PendingEvent(_ context.Context, sessionID string) (*adapters.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	event := f.pending[strings.ToLower(strings.TrimSpace(sessionID))]
	if event == nil {
		return nil, nil
	}
	clone := event.Clone()
	return &clone, nil
}

func (f *fakeEngine) ApplyDecision(
	_ context.Context,
	sessionID string,
	event adapters.Event,
	decision adapters.Decision,
	manualInput string,
) error {
	f.mu.Lock()
	f.applyCalls = append(f.applyCalls, applyCall{
		sessionID:   sessionID,
		event:       event.Clone(),
		decision:    decision,
		manualInput: manualInput,
	})
	f.operations = append(f.operations, "apply:start:"+event.ID)
	callback := f.beforeApplyReturn
	started := f.applyStarted
	release := f.applyRelease
	err := f.applyErr
	if len(f.applyErrs) > 0 {
		err = f.applyErrs[0]
		f.applyErrs = f.applyErrs[1:]
	}
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}
	if callback != nil {
		callback()
	}
	f.mu.Lock()
	f.operations = append(f.operations, "apply:return:"+event.ID)
	f.mu.Unlock()
	return err
}

func (f *fakeEngine) SendLine(_ context.Context, sessionID, line string) error {
	f.mu.Lock()
	f.lineCalls = append(f.lineCalls, lineCall{sessionID: sessionID, line: line})
	f.operations = append(f.operations, "line:start:"+strings.ToLower(sessionID))
	started := f.lineStarted
	release := f.lineRelease
	err := f.lineErr
	f.mu.Unlock()
	if started != nil {
		started <- sessionID
	}
	if release != nil {
		<-release
	}
	f.mu.Lock()
	f.operations = append(f.operations, "line:return:"+strings.ToLower(sessionID))
	f.mu.Unlock()
	return err
}

func (f *fakeEngine) StopAgent(_ context.Context, agentID string) error {
	f.mu.Lock()
	f.stopCalls = append(f.stopCalls, agentID)
	started := f.stopStarted
	release := f.stopRelease
	err := f.stopErr
	f.mu.Unlock()
	if started != nil {
		started <- agentID
	}
	if release != nil {
		<-release
	}
	return err
}

func (f *fakeEngine) StartAgent(_ context.Context, agentID string) error {
	f.mu.Lock()
	f.agentStartCalls = append(f.agentStartCalls, agentID)
	started := f.agentStartStarted
	release := f.agentStartRelease
	err := f.agentStartErr
	f.mu.Unlock()
	if started != nil {
		started <- agentID
	}
	if release != nil {
		<-release
	}
	return err
}

func (f *fakeEngine) RestartAgent(_ context.Context, agentID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agentRestartCalls = append(f.agentRestartCalls, agentID)
	return f.agentRestartErr
}

func (f *fakeEngine) MarkProcessExited(string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.staleExits
}

func (f *fakeEngine) RecordAudit(entry audit.Entry) error {
	f.mu.Lock()
	block := f.auditBlockKind != "" && entry.Kind == f.auditBlockKind
	started := f.auditStarted
	release := f.auditRelease
	f.mu.Unlock()
	if block {
		if started != nil {
			started <- struct{}{}
		}
		if release != nil {
			<-release
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.auditCalls++
	f.operations = append(f.operations, "audit:"+string(entry.Kind)+":"+string(entry.Outcome))
	if f.auditFailAt > 0 && f.auditCalls == f.auditFailAt {
		return errors.New("fixture audit unavailable")
	}
	entry.Metadata = cloneStringMap(entry.Metadata)
	f.auditEntries = append(f.auditEntries, entry)
	return nil
}

func (f *fakeEngine) set(change func(*fakeEngine)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *fakeEngine) applySnapshot() []applyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]applyCall(nil), f.applyCalls...)
}

func (f *fakeEngine) lineSnapshot() []lineCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]lineCall(nil), f.lineCalls...)
}

// lifecycleCalls is how many stops, starts and restarts reached the runtime.
func (f *fakeEngine) lifecycleCalls() (stops, starts, restarts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.stopCalls), len(f.agentStartCalls), len(f.agentRestartCalls)
}

func (f *fakeEngine) operationSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.operations...)
}

func (f *fakeEngine) auditSnapshot() []audit.Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]audit.Entry, len(f.auditEntries))
	for index := range f.auditEntries {
		result[index] = f.auditEntries[index]
		result[index].Metadata = cloneStringMap(f.auditEntries[index].Metadata)
	}
	return result
}

// auditFor returns the journal entries of one kind about one event.
func (f *fakeEngine) auditFor(kind audit.Kind, eventID string) []audit.Entry {
	var entries []audit.Entry
	for _, entry := range f.auditSnapshot() {
		if entry.Kind == kind && entry.EventID == eventID {
			entries = append(entries, entry)
		}
	}
	return entries
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

// sinkCall is one thing the core showed.
type sinkCall struct {
	kind      string // prompt, status, error, refresh, lifecycle or notify
	view      supervise.View
	status    supervise.Status
	failure   supervise.SafeError
	sessionID string
	phase     supervise.Phase
	notice    supervise.Notice
}

// recordingSink records every call in order. probe, when set, runs inside
// each call, as a front end's sink would when it reads the core.
type recordingSink struct {
	mu    sync.Mutex
	calls []sinkCall
	probe func()
}

func (s *recordingSink) record(call sinkCall) {
	s.mu.Lock()
	s.calls = append(s.calls, call)
	probe := s.probe
	s.mu.Unlock()
	if probe != nil {
		probe()
	}
}

func (s *recordingSink) Prompt(view supervise.View) {
	s.record(sinkCall{kind: "prompt", view: view, sessionID: view.SessionID})
}

func (s *recordingSink) Status(status supervise.Status) {
	s.record(sinkCall{kind: "status", status: status, sessionID: status.SessionID})
}

func (s *recordingSink) Error(failure supervise.SafeError) {
	s.record(sinkCall{kind: "error", failure: failure, sessionID: failure.SessionID})
}

func (s *recordingSink) Refresh(sessionID string) {
	s.record(sinkCall{kind: "refresh", sessionID: sessionID})
}

func (s *recordingSink) Lifecycle(sessionID string, phase supervise.Phase) {
	s.record(sinkCall{kind: "lifecycle", sessionID: sessionID, phase: phase})
}

func (s *recordingSink) Notify(notice supervise.Notice) {
	s.record(sinkCall{kind: "notify", notice: notice, sessionID: notice.SessionID})
}

func (s *recordingSink) snapshot() []sinkCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sinkCall(nil), s.calls...)
}

func (s *recordingSink) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
}

// trace names each call the way the desktop emitted it: the kind, then the
// delivery state of a prompt or the status of a status.
func trace(calls []sinkCall) []string {
	names := make([]string, 0, len(calls))
	for _, call := range calls {
		switch call.kind {
		case "prompt":
			names = append(names, "prompt:"+call.view.DeliveryStatus)
		case "status":
			names = append(names, "status:"+call.status.Status)
		case "error":
			names = append(names, "error:"+call.failure.Code)
		case "notify":
			names = append(names, "notify:"+string(call.notice.Kind))
		case "lifecycle":
			names = append(names, "lifecycle:"+string(call.phase))
		default:
			names = append(names, call.kind)
		}
	}
	return names
}

const testRunID = "run-test"

// newCoreForTest starts the supervisor of a run whose agents are the given
// sessions, each on the pty backend with the generic adapter. The run is
// drained when the test ends, and a drain that does not finish fails it.
func newCoreForTest(t *testing.T, engine *fakeEngine, sessionIDs ...string) (*supervise.Supervisor, *recordingSink) {
	t.Helper()
	agents := make([]supervise.AgentSpec, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		agents = append(agents, supervise.AgentSpec{
			SessionID: sessionID,
			AgentID:   sessionID,
			Name:      sessionID,
			Backend:   "pty",
			Adapter:   "generic",
		})
	}
	sink := &recordingSink{}
	ctx, cancel := context.WithCancel(context.Background())
	sup, err := supervise.New(ctx, engine, supervise.Options{RunID: testRunID, Agents: agents, Sink: sink})
	if err != nil {
		cancel()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		// Drained on another goroutine: a test that failed on a deadlock
		// still holds the core's lock, and must fail rather than hang here.
		drained := make(chan struct{})
		go func() {
			sup.BeginDrain()
			sup.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-time.After(5 * time.Second):
			t.Error("the run did not drain")
		}
	})
	return sup, sink
}

// promptEvent is the desktop tests' bridgeEvent: one confirmation prompt.
func promptEvent(sessionID, eventID string) adapters.Event {
	return adapters.Event{
		ID:        eventID,
		Signature: "signature-" + sessionID + "-" + eventID,
		Sequence:  1,
		SessionID: sessionID,
		AgentID:   sessionID,
		Adapter:   "generic",
		Type:      adapters.EventConfirmation,
		Summary:   "Overwrite file?",
		Match:     "Overwrite file? [Y/n]",
		Risk:      adapters.RiskLow,
		Timestamp: time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC),
	}
}

func exitEvent(sessionID string, code int, failed bool) adapters.Event {
	return adapters.NewProcessExitEvent(sessionID, sessionID, "generic", 2, &code, failed)
}

func agentOf(t *testing.T, sup *supervise.Supervisor, sessionID string) supervise.Agent {
	t.Helper()
	agent, found := sup.Agent(sessionID)
	if !found {
		t.Fatalf("agent %q is unknown to the core", sessionID)
	}
	return agent
}

func pendingIDs(sup *supervise.Supervisor) []string {
	var ids []string
	for _, view := range sup.State().Pending {
		ids = append(ids, view.ID)
	}
	return ids
}

func waitFor(t *testing.T, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func assertJSONDoesNotContain(t *testing.T, value interface{}, forbidden string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), forbidden) {
		t.Fatalf("serialized value contains %q: %s", forbidden, encoded)
	}
}

// sinkText is everything the sink received, as text, for leak checks.
func sinkText(calls []sinkCall) string {
	var builder strings.Builder
	for _, call := range calls {
		encoded, _ := json.Marshal([]interface{}{call.view, call.status, call.failure, call.notice})
		builder.Write(encoded)
		builder.WriteString(call.sessionID)
	}
	return builder.String()
}
