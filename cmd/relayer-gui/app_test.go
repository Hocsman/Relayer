package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	appcore "github.com/Hocsman/Relayer/internal/app"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
)

type fakeApplyCall struct {
	sessionID   string
	event       adapters.Event
	decision    adapters.Decision
	manualInput string
}

type fakeDesktopEngine struct {
	mu sync.Mutex

	metadata appcore.DesktopMetadata
	sessions []appcore.DesktopSession
	events   chan session.Event

	outputs     map[string][]string
	outputReads map[string]int
	outputErr   error

	evaluation policy.Evaluation
	pending    map[string]*adapters.Event

	applyCalls        []fakeApplyCall
	applyErr          error
	beforeApplyReturn func()
	applyStarted      chan struct{}
	applyRelease      <-chan struct{}

	auditEntries         []audit.Entry
	auditCalls           int
	auditFailAt          int
	auditErr             error
	auditFailsAfterClose bool
	auditAfterClose      int
	auditBlockKind       audit.Kind
	auditStarted         chan struct{}
	auditRelease         <-chan struct{}

	resizeErr    error
	stopErr      error
	closeErr     error
	closeCalls   int
	closeStarted chan struct{}
	closed       bool
	operations   []string
}

func newFakeDesktopEngine(sessionIDs ...string) *fakeDesktopEngine {
	sessions := make([]appcore.DesktopSession, 0, len(sessionIDs))
	outputs := make(map[string][]string, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		sessions = append(sessions, appcore.DesktopSession{
			ID:      sessionID,
			Name:    sessionID,
			Command: "fixture-agent",
			Backend: "pty",
			Adapter: "generic",
		})
		outputs[strings.ToLower(sessionID)] = []string{"ready"}
	}
	return &fakeDesktopEngine{
		metadata: appcore.DesktopMetadata{
			RunID:        "run-test",
			Backend:      "pty",
			PolicyAction: string(policy.ActionAsk),
			AuditEnabled: true,
			AuditMode:    string(audit.ModeMetadata),
			AuditPath:    "/private/test/audit.jsonl",
		},
		sessions:    sessions,
		events:      make(chan session.Event),
		outputs:     outputs,
		outputReads: make(map[string]int),
		evaluation: policy.Evaluation{
			Action:         policy.ActionAsk,
			ProposedAction: policy.ActionAsk,
			Reason:         policy.ReasonDefault,
		},
		pending: make(map[string]*adapters.Event),
	}
}

func (f *fakeDesktopEngine) Metadata() appcore.DesktopMetadata { return f.metadata }

func (f *fakeDesktopEngine) Sessions() []appcore.DesktopSession {
	return append([]appcore.DesktopSession(nil), f.sessions...)
}

func (f *fakeDesktopEngine) Events() <-chan session.Event { return f.events }

func (f *fakeDesktopEngine) Output(sessionID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outputErr != nil {
		return "", f.outputErr
	}
	key := strings.ToLower(strings.TrimSpace(sessionID))
	values := f.outputs[key]
	if len(values) == 0 {
		return "", nil
	}
	index := f.outputReads[key]
	if index >= len(values) {
		index = len(values) - 1
	} else {
		f.outputReads[key]++
	}
	return values[index], nil
}

func (f *fakeDesktopEngine) PendingEvent(_ context.Context, sessionID string) (*adapters.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	event := f.pending[strings.ToLower(strings.TrimSpace(sessionID))]
	if event == nil {
		return nil, nil
	}
	clone := event.Clone()
	return &clone, nil
}

func (f *fakeDesktopEngine) Evaluate(event adapters.Event) policy.Evaluation {
	evaluation := f.evaluation
	evaluation.EventID = event.ID
	return evaluation
}

func (f *fakeDesktopEngine) ApplyDecision(
	_ context.Context,
	sessionID string,
	event adapters.Event,
	decision adapters.Decision,
	manualInput string,
) error {
	f.mu.Lock()
	f.applyCalls = append(f.applyCalls, fakeApplyCall{
		sessionID:   sessionID,
		event:       event.Clone(),
		decision:    decision,
		manualInput: manualInput,
	})
	f.operations = append(f.operations, "apply:start")
	callback := f.beforeApplyReturn
	started := f.applyStarted
	release := f.applyRelease
	err := f.applyErr
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
	f.operations = append(f.operations, "apply:return")
	f.mu.Unlock()
	return err
}

func (f *fakeDesktopEngine) Resize(context.Context, string, terminal.Size) error {
	return f.resizeErr
}

func (f *fakeDesktopEngine) Stop(context.Context, string) error { return f.stopErr }

func (f *fakeDesktopEngine) RecordAudit(entry audit.Entry) error {
	f.mu.Lock()
	shouldBlock := f.auditBlockKind != "" && entry.Kind == f.auditBlockKind
	started := f.auditStarted
	release := f.auditRelease
	f.mu.Unlock()
	if shouldBlock {
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
	if f.closed {
		f.auditAfterClose++
	}
	if f.auditFailsAfterClose && f.closed {
		return errors.New("fixture audit already closed")
	}
	if f.auditFailAt > 0 && f.auditCalls == f.auditFailAt {
		if f.auditErr != nil {
			return f.auditErr
		}
		return errors.New("fixture audit unavailable")
	}
	entry.Metadata = cloneStringMap(entry.Metadata)
	f.auditEntries = append(f.auditEntries, entry)
	return nil
}

func (f *fakeDesktopEngine) BeginShutdown(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.operations = append(f.operations, "shutdown:begin")
	return nil
}

func (f *fakeDesktopEngine) Close(context.Context) error {
	f.mu.Lock()
	f.closeCalls++
	f.closed = true
	f.operations = append(f.operations, "close:start")
	started := f.closeStarted
	err := f.closeErr
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	return err
}

func (f *fakeDesktopEngine) applySnapshot() []fakeApplyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]fakeApplyCall, len(f.applyCalls))
	copy(result, f.applyCalls)
	return result
}

func (f *fakeDesktopEngine) auditSnapshot() []audit.Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]audit.Entry, len(f.auditEntries))
	for index := range f.auditEntries {
		result[index] = f.auditEntries[index]
		result[index].Metadata = cloneStringMap(f.auditEntries[index].Metadata)
	}
	return result
}

func (f *fakeDesktopEngine) operationSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.operations...)
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

func newBridgeForTest(engine *fakeDesktopEngine) *App {
	application := NewApp()
	application.engine = engine
	application.initializeState(engine)
	// NewApp leaves the injectable frontend event sink unset, so these bridge
	// tests can use ordinary contexts without invoking the Wails runtime.
	application.ctx = nil
	return application
}

func bridgeEvent(sessionID, eventID string) adapters.Event {
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

func TestBridgeKeepsSimultaneousPromptsWithSameEventIDSeparateBySession(t *testing.T) {
	engine := newFakeDesktopEngine("Agent-A", "Agent-B")
	application := newBridgeForTest(engine)

	left := bridgeEvent("Agent-A", "prompt-1")
	right := bridgeEvent("Agent-B", "prompt-1")
	application.handleAdapterEvent(left)
	application.handleAdapterEvent(right)
	// Replaying one occurrence must not remove or duplicate the other session's
	// prompt even though both adapters chose the same occurrence ID.
	application.handleAdapterEvent(left)

	state, err := application.GetState()
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if len(state.PendingEvents) != 2 {
		t.Fatalf("pending events = %#v, want two distinct prompts", state.PendingEvents)
	}
	if _, exists := application.pending[makeEventKey("Agent-A", "prompt-1")]; !exists {
		t.Fatal("Agent-A composite prompt key is missing")
	}
	if _, exists := application.pending[makeEventKey("Agent-B", "prompt-1")]; !exists {
		t.Fatal("Agent-B composite prompt key is missing")
	}
	if state.PendingEvents[0].SessionID == state.PendingEvents[1].SessionID {
		t.Fatalf("session identity collapsed: %#v", state.PendingEvents)
	}
	if got := len(engine.auditSnapshot()); got != 4 {
		t.Fatalf("audit entries = %d, want two entries per unique prompt", got)
	}
}

func TestSensitiveManualInputNeverAppearsInDTOsOrAudit(t *testing.T) {
	const secret = "otp-493827-super-secret"
	engine := newFakeDesktopEngine("secure-agent")
	application := newBridgeForTest(engine)
	event := bridgeEvent("secure-agent", "credential-1")
	event.Type = adapters.EventCredential
	event.Sensitive = true
	event.Risk = adapters.RiskHigh
	event.Summary = "Password: " + secret
	event.Match = secret
	event.Metadata = map[string]string{"authorization": "Bearer " + secret}
	application.handleAdapterEvent(event)

	before, err := application.GetState()
	if err != nil {
		t.Fatalf("GetState before decision: %v", err)
	}
	assertJSONDoesNotContain(t, before, secret)
	if len(before.PendingEvents) != 1 || before.PendingEvents[0].Summary != "Saisie sensible requise" {
		t.Fatalf("sensitive prompt DTO = %#v", before.PendingEvents)
	}

	application.ctx = context.Background()
	if err := application.SubmitDecision(event.SessionID, event.ID, secret); err != nil {
		t.Fatalf("SubmitDecision: %v", err)
	}

	calls := engine.applySnapshot()
	if len(calls) != 1 || calls[0].decision != adapters.DecisionManual || calls[0].manualInput != secret {
		t.Fatalf("manual delivery = %#v", calls)
	}
	after, err := application.GetState()
	if err != nil {
		t.Fatalf("GetState after decision: %v", err)
	}
	assertJSONDoesNotContain(t, after, secret)
	audits := engine.auditSnapshot()
	assertJSONDoesNotContain(t, audits, secret)
	for _, entry := range audits {
		if entry.DecisionBy == audit.DecisionByHuman && (entry.Summary != "" || entry.Metadata != nil) {
			t.Fatalf("human audit entry retained content: %#v", entry)
		}
	}
}

func TestAuditFailurePreventsManualDeliveryAndFreezesBridge(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	engine.auditFailAt = 3 // event + policy succeed; the human decision record fails.
	application := newBridgeForTest(engine)
	event := bridgeEvent("agent-a", "prompt-audit")
	application.handleAdapterEvent(event)

	if err := application.SubmitDecision(event.SessionID, event.ID, "Y"); !errors.Is(err, errAuditUnavailable) {
		t.Fatalf("SubmitDecision error = %v, want audit unavailable", err)
	}
	if got := len(engine.applySnapshot()); got != 0 {
		t.Fatalf("backend received %d decision(s) after audit failure", got)
	}
	state, err := application.GetState()
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if state.Audit.Status != "failed" || len(state.PendingEvents) != 1 {
		t.Fatalf("fail-closed state = %#v", state)
	}
	prompt := state.PendingEvents[0]
	if prompt.DeliveryStatus != "failed" || prompt.Evaluation.Reason != "audit_unavailable" {
		t.Fatalf("frozen prompt = %#v", prompt)
	}
	if err := application.SubmitDecision(event.SessionID, event.ID, "n"); !errors.Is(err, errDeliveryUncertain) {
		t.Fatalf("second decision error = %v, want frozen delivery", err)
	}
	if got := len(engine.applySnapshot()); got != 0 {
		t.Fatalf("backend received %d decision(s) while audit was frozen", got)
	}
}

func TestUnsupportedAutomaticDecisionFallsBackToAsk(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	application := newBridgeForTest(engine)
	event := bridgeEvent("agent-a", "automatic-1")
	application.handleAdapterEvent(event)
	key := makeEventKey(event.SessionID, event.ID)
	automatic := policy.Evaluation{
		Action:         policy.ActionAllow,
		ProposedAction: policy.ActionAllow,
		RuleName:       "allow-safe",
		Reason:         policy.ReasonRule,
		EventID:        event.ID,
		Automatic:      true,
	}
	application.mu.Lock()
	item := application.pending[key]
	item.view = supervisionView(event, automatic, "delivering")
	application.pending[key] = item
	application.rebuildPendingLocked()
	application.mu.Unlock()

	engine.applyErr = adapters.ErrDecisionUnsupported
	application.ctx = context.Background()
	application.applyAutomatic(key, event, automatic)

	calls := engine.applySnapshot()
	if len(calls) != 1 || calls[0].decision != adapters.DecisionAllow || calls[0].manualInput != "" {
		t.Fatalf("automatic delivery = %#v", calls)
	}
	state, err := application.GetState()
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if len(state.PendingEvents) != 1 {
		t.Fatalf("pending events = %#v, want retained prompt", state.PendingEvents)
	}
	prompt := state.PendingEvents[0]
	if prompt.Evaluation.Action != string(policy.ActionAsk) || prompt.Evaluation.ProposedAction != string(policy.ActionAllow) ||
		prompt.Evaluation.Automatic || prompt.Evaluation.Reason != "fallback_unsupported" || prompt.DeliveryStatus != "pending" {
		t.Fatalf("unsupported decision did not fall back to ask: %#v", prompt)
	}
	entries := engine.auditSnapshot()
	if len(entries) == 0 || entries[len(entries)-1].Outcome != audit.OutcomeFallbackUnsupported {
		t.Fatalf("fallback audit entries = %#v", entries)
	}
}

func TestSecondPromptForSameSessionRemainsWaitingAfterFirstDecision(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	application := newBridgeForTest(engine)
	first := bridgeEvent("agent-a", "prompt-1")
	second := bridgeEvent("agent-a", "prompt-2")
	second.Sequence = 2
	second.Timestamp = second.Timestamp.Add(time.Second)
	application.handleAdapterEvent(first)

	application.ctx = context.Background()
	engine.beforeApplyReturn = func() {
		// Reproduce a second semantic event arriving before the first delivery
		// returns.
		application.handleAdapterEvent(second)
	}
	if err := application.SubmitDecision(first.SessionID, first.ID, "Y"); err != nil {
		t.Fatalf("SubmitDecision(first): %v", err)
	}

	state, err := application.GetState()
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if len(state.PendingEvents) != 1 || state.PendingEvents[0].ID != second.ID {
		t.Fatalf("second prompt was not retained: %#v", state.PendingEvents)
	}
	if len(state.Agents) != 1 || state.Agents[0].Status != "waiting" {
		t.Fatalf("agent status after overlapping prompt = %#v, want waiting", state.Agents)
	}
	if calls := engine.applySnapshot(); len(calls) != 1 || calls[0].event.ID != first.ID {
		t.Fatalf("deliveries = %#v, want only first prompt", calls)
	}
}

func TestConcurrentSubmitDecisionAdmitsExactlyOneDelivery(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	application := newBridgeForTest(engine)
	event := bridgeEvent("agent-a", "prompt-concurrent")
	application.handleAdapterEvent(event)

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)
	engine.applyStarted = started
	engine.applyRelease = release
	application.ctx = context.Background()

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- application.SubmitDecision(event.SessionID, event.ID, "Y")
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first decision never reached the backend")
	}

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- application.SubmitDecision(event.SessionID, event.ID, "n")
	}()

	var (
		secondErr     error
		secondEntered bool
	)
	select {
	case <-started:
		secondEntered = true
	case secondErr = <-secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("second concurrent decision was neither rejected nor delivered")
	}

	releaseAll()
	firstErr := <-firstDone
	if secondEntered {
		secondErr = <-secondDone
	}

	if firstErr != nil {
		t.Fatalf("first SubmitDecision: %v", firstErr)
	}
	if !errors.Is(secondErr, errDecisionInFlight) {
		t.Fatalf("second SubmitDecision error = %v, want decision in flight", secondErr)
	}
	calls := engine.applySnapshot()
	if len(calls) != 1 {
		t.Fatalf("concurrent SubmitDecision admitted %d backend deliveries: %#v", len(calls), calls)
	}
	deliveries := 0
	for _, entry := range engine.auditSnapshot() {
		if entry.Kind == audit.KindDelivery && entry.EventID == event.ID {
			deliveries++
		}
	}
	if deliveries != 1 {
		t.Fatalf("terminal delivery audit count = %d, want one", deliveries)
	}
}

func TestProcessExitDuringAutomaticDeliveryStillRecordsTerminalOutcome(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	application := newBridgeForTest(engine)
	prompt := bridgeEvent("agent-a", "automatic-exit")
	application.handleAdapterEvent(prompt)
	key := makeEventKey(prompt.SessionID, prompt.ID)
	automatic := policy.Evaluation{
		Action:         policy.ActionAllow,
		ProposedAction: policy.ActionAllow,
		RuleName:       "allow-safe",
		Reason:         policy.ReasonRule,
		EventID:        prompt.ID,
		Automatic:      true,
	}
	application.mu.Lock()
	item := application.pending[key]
	item.view = supervisionView(prompt, automatic, "delivering")
	application.pending[key] = item
	application.rebuildPendingLocked()
	application.mu.Unlock()

	exitCode := 0
	exitEvent := adapters.NewProcessExitEvent(prompt.SessionID, prompt.AgentID, prompt.Adapter, 2, &exitCode, false)
	application.ctx = context.Background()
	engine.beforeApplyReturn = func() {
		application.handleAdapterEvent(exitEvent)
	}
	application.applyAutomatic(key, prompt, automatic)

	state, err := application.GetState()
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if len(state.PendingEvents) != 0 || len(state.Agents) != 1 || state.Agents[0].Status != "exited" {
		t.Fatalf("post-exit state = %#v", state)
	}
	var terminalDeliveries []audit.Entry
	for _, entry := range engine.auditSnapshot() {
		if entry.Kind == audit.KindDelivery && entry.EventID == prompt.ID {
			terminalDeliveries = append(terminalDeliveries, entry)
		}
	}
	if len(terminalDeliveries) != 1 {
		t.Fatalf("terminal delivery audit entries = %#v, want exactly one", terminalDeliveries)
	}
	if terminalDeliveries[0].Outcome != audit.OutcomeApplied || terminalDeliveries[0].Reason != "delivery_applied" {
		t.Fatalf("terminal delivery outcome = %#v", terminalDeliveries[0])
	}
}

func TestShutdownWaitsForInFlightDeliveryBeforeClosingEngine(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	engine.auditFailsAfterClose = true
	engine.applyStarted = make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseApply := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseApply)
	engine.applyRelease = release
	engine.closeStarted = make(chan struct{}, 1)
	application := newBridgeForTest(engine)
	event := bridgeEvent("agent-a", "prompt-shutdown")
	application.handleAdapterEvent(event)
	application.ctx = context.Background()

	decisionDone := make(chan error, 1)
	go func() {
		decisionDone <- application.SubmitDecision(event.SessionID, event.ID, "Y")
	}()
	select {
	case <-engine.applyStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("decision never became in flight")
	}
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- application.Shutdown() }()
	waitForCondition(t, 2*time.Second, func() bool {
		application.mu.RLock()
		defer application.mu.RUnlock()
		return application.state.RunStatus == "stopping"
	})

	closedBeforeDelivery := false
	select {
	case <-engine.closeStarted:
		closedBeforeDelivery = true
	case <-time.After(150 * time.Millisecond):
	}
	releaseApply()
	decisionErr := <-decisionDone
	shutdownErr := <-shutdownDone

	if closedBeforeDelivery {
		t.Fatal("engine.Close ran while a decision delivery was still in flight")
	}
	if decisionErr != nil {
		t.Fatalf("in-flight SubmitDecision: %v", decisionErr)
	}
	if shutdownErr != nil {
		t.Fatalf("Shutdown: %v", shutdownErr)
	}
	operations := engine.operationSnapshot()
	applyReturn := operationIndex(operations, "apply:return")
	deliveryAudit := operationIndex(operations, "audit:delivery:applied")
	closeStart := operationIndex(operations, "close:start")
	if applyReturn < 0 || deliveryAudit < 0 || closeStart < 0 || !(applyReturn < deliveryAudit && deliveryAudit < closeStart) {
		t.Fatalf("unsafe shutdown ordering: %#v", operations)
	}
}

func TestShutdownWaitsForAdmittedDecisionBlockedBeforeApply(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	engine.auditFailsAfterClose = true
	engine.auditBlockKind = audit.KindDecision
	engine.auditStarted = make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAudit := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAudit)
	engine.auditRelease = release
	engine.closeStarted = make(chan struct{}, 1)
	application := newBridgeForTest(engine)
	event := bridgeEvent("agent-a", "prompt-shutdown-before-apply")
	application.handleAdapterEvent(event)
	application.ctx = context.Background()

	decisionDone := make(chan error, 1)
	go func() {
		decisionDone <- application.SubmitDecision(event.SessionID, event.ID, "Y")
	}()
	select {
	case <-engine.auditStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("admitted decision never reached its pre-delivery audit")
	}
	if calls := engine.applySnapshot(); len(calls) != 0 {
		t.Fatalf("backend was called before the decision audit completed: %#v", calls)
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- application.Shutdown() }()
	waitForCondition(t, 2*time.Second, func() bool {
		operations := engine.operationSnapshot()
		return operationIndex(operations, "shutdown:begin") >= 0
	})

	closedBeforeAudit := false
	select {
	case <-engine.closeStarted:
		closedBeforeAudit = true
	case <-time.After(150 * time.Millisecond):
	}
	releaseAudit()
	decisionErr := <-decisionDone
	shutdownErr := <-shutdownDone

	if closedBeforeAudit {
		t.Fatal("engine.Close ran while an admitted decision was blocked before ApplyDecision")
	}
	if decisionErr != nil {
		t.Fatalf("admitted SubmitDecision: %v", decisionErr)
	}
	if shutdownErr != nil {
		t.Fatalf("Shutdown: %v", shutdownErr)
	}
	if calls := engine.applySnapshot(); len(calls) != 1 {
		t.Fatalf("backend deliveries = %#v, want exactly one", calls)
	}
	operations := engine.operationSnapshot()
	decisionAudit := operationIndex(operations, "audit:decision:in_flight")
	applyStart := operationIndex(operations, "apply:start")
	deliveryAudit := operationIndex(operations, "audit:delivery:applied")
	closeStart := operationIndex(operations, "close:start")
	if decisionAudit < 0 || applyStart < 0 || deliveryAudit < 0 || closeStart < 0 ||
		!(decisionAudit < applyStart && applyStart < deliveryAudit && deliveryAudit < closeStart) {
		t.Fatalf("unsafe pre-Apply shutdown ordering: %#v", operations)
	}
	engine.mu.Lock()
	auditAfterClose := engine.auditAfterClose
	engine.mu.Unlock()
	if auditAfterClose != 0 {
		t.Fatalf("audit writes after Close = %d, want zero", auditAfterClose)
	}
}

func TestShutdownAuditsCancelledAutomaticBeforeClosingEngine(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	engine.evaluation = policy.Evaluation{
		Action:         policy.ActionAllow,
		ProposedAction: policy.ActionAllow,
		RuleName:       "allow-safe",
		Reason:         policy.ReasonRule,
		Automatic:      true,
	}
	engine.auditFailsAfterClose = true
	engine.auditBlockKind = audit.KindDecision
	engine.auditStarted = make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAudit := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAudit)
	engine.auditRelease = release
	engine.closeStarted = make(chan struct{}, 1)
	application := newBridgeForTest(engine)
	application.ctx = context.Background()

	event := bridgeEvent("agent-a", "automatic-shutdown-before-admission")
	application.handleAdapterEvent(event)
	select {
	case <-engine.auditStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("automatic decision never reached its pre-delivery audit")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- application.Shutdown() }()
	waitForCondition(t, 2*time.Second, func() bool {
		operations := engine.operationSnapshot()
		return operationIndex(operations, "shutdown:begin") >= 0
	})

	closedBeforeAudit := false
	select {
	case <-engine.closeStarted:
		closedBeforeAudit = true
	case <-time.After(150 * time.Millisecond):
	}
	releaseAudit()
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if closedBeforeAudit {
		t.Fatal("engine.Close ran before the automatic decision received a terminal audit outcome")
	}
	if calls := engine.applySnapshot(); len(calls) != 0 {
		t.Fatalf("backend received a delivery after shutdown closed admission: %#v", calls)
	}
	operations := engine.operationSnapshot()
	decisionAudit := operationIndex(operations, "audit:decision:in_flight")
	cancelledAudit := operationIndex(operations, "audit:delivery:cancelled")
	closeStart := operationIndex(operations, "close:start")
	if decisionAudit < 0 || cancelledAudit < 0 || closeStart < 0 ||
		!(decisionAudit < cancelledAudit && cancelledAudit < closeStart) {
		t.Fatalf("incomplete automatic shutdown audit ordering: %#v", operations)
	}
	var cancelledEntries []audit.Entry
	for _, entry := range engine.auditSnapshot() {
		if entry.Kind == audit.KindDelivery && entry.EventID == event.ID && entry.Outcome == audit.OutcomeCancelled {
			cancelledEntries = append(cancelledEntries, entry)
		}
	}
	if len(cancelledEntries) != 1 || cancelledEntries[0].Reason != "runtime_stopped" {
		t.Fatalf("cancelled automatic audit entries = %#v", cancelledEntries)
	}
	engine.mu.Lock()
	auditAfterClose := engine.auditAfterClose
	engine.mu.Unlock()
	if auditAfterClose != 0 {
		t.Fatalf("audit writes after Close = %d, want zero", auditAfterClose)
	}
}

func TestOutputRefreshPublishesSuccessiveBoundedSnapshots(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	engine.outputs["agent-a"] = []string{"first snapshot", "second snapshot", "third snapshot"}
	application := newBridgeForTest(engine)

	state, err := application.GetState()
	if err != nil {
		t.Fatalf("initial GetState: %v", err)
	}
	assertAgentSnapshot(t, state, "first snapshot", 1)

	application.refreshOutput("AGENT-A")
	state, err = application.GetState()
	if err != nil {
		t.Fatalf("second GetState: %v", err)
	}
	assertAgentSnapshot(t, state, "second snapshot", 2)

	application.refreshOutput("agent-a")
	state, err = application.GetState()
	if err != nil {
		t.Fatalf("third GetState: %v", err)
	}
	assertAgentSnapshot(t, state, "third snapshot", 3)

	state.Agents[0].Output = "mutated by caller"
	fresh, err := application.GetState()
	if err != nil {
		t.Fatalf("fresh GetState: %v", err)
	}
	assertAgentSnapshot(t, fresh, "third snapshot", 3)
}

func TestShutdownIsConcurrentAndClosesEngineExactlyOnce(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	application := newBridgeForTest(engine)

	const callers = 12
	errorsByCaller := make(chan error, callers)
	var callersWG sync.WaitGroup
	callersWG.Add(callers)
	for index := 0; index < callers; index++ {
		go func() {
			defer callersWG.Done()
			errorsByCaller <- application.Shutdown()
		}()
	}
	callersWG.Wait()
	close(errorsByCaller)
	for err := range errorsByCaller {
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}
	engine.mu.Lock()
	closeCalls := engine.closeCalls
	engine.mu.Unlock()
	if closeCalls != 1 {
		t.Fatalf("engine Close calls = %d, want one", closeCalls)
	}
	state, err := application.GetState()
	if err != nil {
		t.Fatalf("GetState after shutdown: %v", err)
	}
	if state.RunStatus != "stopped" {
		t.Fatalf("run status = %q, want stopped", state.RunStatus)
	}
}

func assertJSONDoesNotContain(t *testing.T, value interface{}, forbidden string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal display/audit value: %v", err)
	}
	if strings.Contains(string(encoded), forbidden) {
		t.Fatalf("serialized value contains sensitive input %q: %s", forbidden, encoded)
	}
}

func assertAgentSnapshot(t *testing.T, state AppState, output string, revision uint64) {
	t.Helper()
	if len(state.Agents) != 1 {
		t.Fatalf("agents = %#v", state.Agents)
	}
	if state.Agents[0].Output != output || state.Agents[0].Revision != revision {
		t.Fatalf("agent snapshot = %#v, want output %q revision %d", state.Agents[0], output, revision)
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}

func operationIndex(operations []string, expected string) int {
	for index, operation := range operations {
		if operation == expected {
			return index
		}
	}
	return -1
}
