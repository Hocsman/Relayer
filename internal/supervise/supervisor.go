package supervise

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
)

const maxResolvedEvents = 1024

// Engine is what the core needs of a run's runtime: the policy, the adapters'
// encoders, the backends' writes, the agents' lifecycle and the journal.
// *app.DesktopRuntime satisfies it.
type Engine interface {
	Evaluate(adapters.Event) policy.Evaluation
	SupportedDecisions(adapters.Event) []adapters.Decision
	ApplyDecision(ctx context.Context, sessionID string, event adapters.Event, decision adapters.Decision, manualInput string) error
	PendingEvent(ctx context.Context, sessionID string) (*adapters.Event, error)
	SendLine(ctx context.Context, sessionID, line string) error
	StopAgent(ctx context.Context, agentID string) error
	StartAgent(ctx context.Context, agentID string) error
	RestartAgent(ctx context.Context, agentID string) error
	MarkProcessExited(agentID string) bool
	// RecordAudit is synchronous and fail-closed: when it returns an error the
	// core sends nothing more for the whole run.
	RecordAudit(audit.Entry) error
}

// Sink receives what the core has to show, in the order of the state changes
// it reports: what one change shows is queued with the change, under the
// core's lock, and the queue is shown in order by one goroutine at a time,
// never while the core's lock is held. The sink is therefore never called by
// two goroutines at once, and a prompt is never shown delivering after it was
// shown delivered. A call is made on the goroutine that queued it, or on
// another that was showing the queue meanwhile; either way, an operation's
// calls are made before it returns, as the desktop always made them.
//
// A sink may read the core (State, Agent, Draining), but must not call an
// operation that changes it, and should not block: the goroutine calling it
// may be delivering a decision, and the other goroutines that show something
// wait for it. A front end may hold a lock of its own while it reads the core
// or calls SetHolder, AdmitRun, Admit, the release Admit returns, RecordAudit
// or BeginDrain, none of which calls the sink on the caller's goroutine. It must
// not hold a lock its sink takes while it calls an operation that changes the
// core, which may show what another goroutine queued, nor while it calls
// Wait, which waits for goroutines that show things.
type Sink interface {
	// Prompt shows a prompt, or a change of its delivery state.
	Prompt(View)
	// Status reports a session's status or, with Scope "audit", that the
	// journal failed.
	Status(Status)
	// Error reports a failure by a fixed code and message, never by an
	// error's own text.
	Error(SafeError)
	// Refresh asks the front end to read the session's bounded output again,
	// before what the core shows next.
	Refresh(sessionID string)
	// Lifecycle reports a change of the session's process that the front
	// end's own state has to follow.
	Lifecycle(sessionID string, phase Phase)
	// Notify asks the front end to tell the operator; how is its own choice.
	Notify(Notice)
}

// Phase is a change of a session's process reported through Sink.Lifecycle.
type Phase string

// PhaseStarted: a new process replaced the session's previous one. Its output
// belongs to the previous process and does not carry over. It is reported
// before the core shows the agent running, so a front end that drops the
// output on it never shows the new process with its predecessor's output.
const PhaseStarted Phase = "started"

// Status is a session-scoped status, or the audit-scoped one that reports a
// journal failure. It has the fields and the JSON of the desktop's StatusEvent.
type Status struct {
	RunID     string `json:"runID"`
	Scope     string `json:"scope"`
	Status    string `json:"status"`
	SessionID string `json:"sessionID,omitempty"`
	// ClearedBefore, when set, says the backend dropped every prompt of the
	// session detected before this time (RFC 3339): the session ended, or a
	// new process replaced it. Clients drop the same prompts; a status without
	// it, such as a stream error on a live session, leaves them answerable.
	ClearedBefore string `json:"clearedBefore,omitempty"`
}

// SafeError is a failure as it may be shown: a fixed code and message. It has
// the fields and the JSON of the desktop's SafeErrorEvent.
type SafeError struct {
	RunID     string `json:"runID"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	SessionID string `json:"sessionID,omitempty"`
	Timestamp string `json:"timestamp"`
}

// NoticeKind says why the operator is notified.
type NoticeKind string

const (
	// NoticePendingDecision: a prompt waits for a human.
	NoticePendingDecision NoticeKind = "pending_decision"
	// NoticeGuardrailBlocked: a guardrail decided a prompt, automatic or not.
	NoticeGuardrailBlocked NoticeKind = "guardrail_blocked"
)

// NoticeSeverity is how urgently the operator is notified.
type NoticeSeverity string

const (
	SeverityWarning  NoticeSeverity = "warning"
	SeverityCritical NoticeSeverity = "critical"
)

// Notice is a request to tell the operator about a prompt. The front end picks
// the transport and the wording of the title.
type Notice struct {
	Kind      NoticeKind
	Severity  NoticeSeverity
	AgentName string
	SessionID string
	EventID   string
	Reason    string
	// Details is the prompt's display-safe summary, the one its View shows:
	// bounded, redacted, and a fixed text for a prompt whose text must not be
	// shown. A front end may send it anywhere a View may go.
	Details string
}

// Options configure the supervisor of one run.
type Options struct {
	// RunID names the run in every view, status and error, and is the run an
	// operation must address.
	RunID string
	// Agents are the run's agents, all running when the run starts.
	Agents []AgentSpec
	// Sink receives what the core shows; nil discards it.
	Sink Sink
	// RepeatWindow is how long after an answer is written a prompt of the
	// same session with the same Signature is taken for its repeat, which
	// the policy never answers. Zero or less means DefaultRepeatWindow: the
	// guard cannot be turned off.
	RepeatWindow time.Duration
	// Now is the clock that times RepeatWindow; nil means time.Now.
	Now func() time.Time
}

// DefaultRepeatWindow is the RepeatWindow of a run whose options set none.
const DefaultRepeatWindow = 2 * time.Second

// ReasonRepeatAfterDelivery is the reason of a prompt the policy would have
// answered automatically but that repeats a prompt of its session whose answer
// is being written, or was written less than RepeatWindow ago: it is asked,
// never answered. An adapter that reads an answered question again, from its
// echo or a repaint, raises such a repeat under a new ID, and answering it
// types a second answer into an agent that already consumed the first.
const ReasonRepeatAfterDelivery = "repeat_after_delivery"

// State is one consistent reading of what the core holds for a run.
type State struct {
	Agents      []Agent
	Pending     []View
	AuditFailed bool
}

type eventKey struct {
	sessionID string
	eventID   string
}

// writeClaim is a session's claim by the decision whose answer is being
// written: the prompt it answers, and that prompt's Signature. Both stay with
// the claim until the write returns, even when the agent withdraws the prompt
// meanwhile, which is when its echo is most likely read as a repeat.
type writeClaim struct {
	key       eventKey
	signature string
}

// ingestion is a prompt being taken in (reserveEvent), until it is pending or
// set aside. hand is the session's hand generation when it was reserved: a
// hand taken since, even one released again, may have typed its answer.
// signature is the prompt's, which keystrokes written meanwhile count as
// answered (finishRaw). withdrawn is the tombstone its withdrawal leaves
// when it arrives before the prompt is pending.
type ingestion struct {
	hand      uint64
	signature string
	withdrawn bool
}

// pendingEvent is a prompt the run waits on. evaluation is the policy's, as
// the engine returned it, and what the core decides from: the view shows it
// bounded and redacted, and an evaluation rebuilt from the view named its rule
// as the prompt shows it. The two change together when the prompt goes to the
// operator.
type pendingEvent struct {
	event      adapters.Event
	view       View
	evaluation policy.Evaluation
}

// Supervisor is the supervision state machine of one run generation. A run
// that stops is drained and dropped with its supervisor; the next run gets a
// new one, so nothing of a run's prompts, answers or freezes outlives it.
type Supervisor struct {
	ctx          context.Context
	runID        string
	engine       Engine
	sink         Sink
	now          func() time.Time
	repeatWindow time.Duration

	mu           sync.RWMutex
	agents       []Agent
	agentIndex   map[string]int
	pendingViews []View
	pending      map[eventKey]pendingEvent
	ingesting    map[eventKey]ingestion
	// withdrawing marks a pending prompt whose withdrawal is being journaled:
	// it is nobody's to act on any more (handleAdapterEventWithdrawn).
	withdrawing   map[eventKey]struct{}
	resolved      map[eventKey]struct{}
	resolvedOrder []eventKey
	inFlight      map[string]writeClaim
	// answered is, per session, when an answer to a prompt of each Signature
	// was last written, for as long as a prompt with that Signature would be
	// taken for its repeat.
	answered         map[string]map[string]time.Time
	lineInFlight     map[string]bool
	stoppingSessions map[string]bool
	// startingSessions marks a Start or Restart in progress. The agent is not
	// yet marked running, and a prompt its replacement raised then used to be
	// dropped as the prompt of a stopped agent.
	startingSessions map[string]bool
	frozen           map[string]bool
	// holders is, per session, the connection that holds its terminal.
	holders map[string]string
	// handGenerations counts, per session, the times its hand was taken
	// (SetHolder). A prompt compares it with the count it started from, and
	// learns that a hand came and went while it was being taken in.
	handGenerations map[string]uint64
	// heldEntries is, per session, closed once the evaluation entries of the
	// prompts the hand last turned into asks are journaled.
	heldEntries map[string]chan struct{}
	// rawInFlight marks a session whose holder's keystrokes are admitted
	// (Admit): the session's one write slot is theirs until released.
	rawInFlight  map[string]bool
	auditFailed  bool
	shuttingDown bool
	// outbox holds the sink calls not yet made, in the order of the state
	// changes they report: each is queued under mu, with its change.
	outbox []func(Sink)

	// flushMu is held by the one goroutine that makes the outbox's calls.
	flushMu sync.Mutex

	deliveryMu        sync.Mutex
	deliveryAvailable bool
	deliveryWG        sync.WaitGroup

	eventWG sync.WaitGroup
}

// New returns the supervisor of one run, with every agent running. ctx is the
// run's own context: cancelling it interrupts the writes in flight when the
// run stops.
func New(ctx context.Context, engine Engine, options Options) (*Supervisor, error) {
	if ctx == nil {
		return nil, errors.New("supervise: a run context is required")
	}
	if engine == nil {
		return nil, errors.New("supervise: an engine is required")
	}
	if strings.TrimSpace(options.RunID) == "" {
		return nil, errors.New("supervise: a run ID is required")
	}
	sink := options.Sink
	if sink == nil {
		sink = discardSink{}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	repeatWindow := options.RepeatWindow
	if repeatWindow <= 0 {
		repeatWindow = DefaultRepeatWindow
	}
	agents := make([]Agent, 0, len(options.Agents))
	index := make(map[string]int, len(options.Agents))
	for _, spec := range options.Agents {
		index[strings.ToLower(spec.SessionID)] = len(agents)
		agents = append(agents, Agent{AgentSpec: spec, Status: "running", Running: true})
	}
	return &Supervisor{
		ctx:               ctx,
		runID:             options.RunID,
		engine:            engine,
		sink:              sink,
		now:               now,
		repeatWindow:      repeatWindow,
		agents:            agents,
		agentIndex:        index,
		pendingViews:      []View{},
		pending:           make(map[eventKey]pendingEvent),
		ingesting:         make(map[eventKey]ingestion),
		withdrawing:       make(map[eventKey]struct{}),
		resolved:          make(map[eventKey]struct{}),
		inFlight:          make(map[string]writeClaim),
		answered:          make(map[string]map[string]time.Time),
		lineInFlight:      make(map[string]bool),
		stoppingSessions:  make(map[string]bool),
		startingSessions:  make(map[string]bool),
		frozen:            make(map[string]bool),
		holders:           make(map[string]string),
		handGenerations:   make(map[string]uint64),
		heldEntries:       make(map[string]chan struct{}),
		rawInFlight:       make(map[string]bool),
		deliveryAvailable: true,
	}, nil
}

type discardSink struct{}

func (discardSink) Prompt(View)             {}
func (discardSink) Status(Status)           {}
func (discardSink) Error(SafeError)         {}
func (discardSink) Refresh(string)          {}
func (discardSink) Lifecycle(string, Phase) {}
func (discardSink) Notify(Notice)           {}

// State returns the agents, the pending prompts in display order and whether
// the journal failed, read together. It never reaches the sink, so a front end
// may call it under its own lock.
func (s *Supervisor) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	agents := make([]Agent, len(s.agents))
	for index, agent := range s.agents {
		agent.ExitCode = cloneExitCode(agent.ExitCode)
		agents[index] = agent
	}
	return State{
		Agents:      agents,
		Pending:     append([]View(nil), s.pendingViews...),
		AuditFailed: s.auditFailed,
	}
}

// Agent returns one agent's state. Like State, it may be called under a
// front end's own lock.
func (s *Supervisor) Agent(sessionID string) (Agent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	index, found := s.agentIndex[strings.ToLower(strings.TrimSpace(sessionID))]
	if !found {
		return Agent{}, false
	}
	agent := s.agents[index]
	agent.ExitCode = cloneExitCode(agent.ExitCode)
	return agent, true
}

// Draining reports whether BeginDrain was called: the run takes nothing more
// in.
func (s *Supervisor) Draining() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.shuttingDown
}

// BeginDrain stops the run taking anything in: no write is admitted, no
// automatic decision is scheduled, and every operation refuses with
// ErrRuntimeStopped. What was already admitted carries on to its journaled
// outcome, which Wait waits for.
func (s *Supervisor) BeginDrain() {
	s.closeDelivery()
	s.mu.Lock()
	s.shuttingDown = true
	s.mu.Unlock()
}

// Wait returns once every admitted write, and every automatic decision the
// core started, has finished. It follows BeginDrain; the runtime's journal
// must stay open until it returns.
func (s *Supervisor) Wait() {
	s.deliveryWG.Wait()
	s.eventWG.Wait()
}

func (s *Supervisor) isActiveRun() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.shuttingDown
}

// activeRun admits an operation addressed to expectedRunID. A nil Supervisor
// is no run at all, which a front end has between runs: it refuses like a
// stopped run, once the operation has checked its own arguments.
func (s *Supervisor) activeRun(expectedRunID string) error {
	if s == nil {
		return ErrRuntimeStopped
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.shuttingDown {
		return ErrRuntimeStopped
	}
	if strings.TrimSpace(expectedRunID) == "" || expectedRunID != s.runID {
		return ErrRunStale
	}
	return nil
}

func (s *Supervisor) recordAudit(entry audit.Entry) bool {
	if err := s.engine.RecordAudit(entry); err != nil {
		s.freezeAudit(false)
		return false
	}
	return true
}

// freezeAudit freezes the run once the journal refused an entry: nothing more
// is sent, every running session is frozen and every prompt shown failed.
// What it shows is shown before it returns or, offCaller, on a goroutine of
// its own, counted under the core's lock so that a drain waits for it: a
// front end journals its own entries under a lock its sink may take
// (RecordAudit). During a drain nothing is frozen, as it never was.
func (s *Supervisor) freezeAudit(offCaller bool) {
	if !s.isActiveRun() {
		return
	}
	s.closeDelivery()
	s.mu.Lock()
	if s.auditFailed {
		s.mu.Unlock()
		return
	}
	s.auditFailed = true
	for _, agent := range s.agents {
		if agent.Running {
			s.frozen[strings.ToLower(agent.SessionID)] = true
		}
	}
	for index := range s.agents {
		if s.agents[index].Running {
			s.agents[index].InputFrozen = true
		}
	}
	for key, item := range s.pending {
		item.view.DeliveryStatus = "failed"
		item.view.Evaluation.Reason = "audit_unavailable"
		s.pending[key] = item
	}
	s.rebuildPendingLocked()
	s.showStatusLocked(Status{RunID: s.runID, Scope: "audit", Status: "failed"})
	s.emitSafeErrorLocked("audit_unavailable", "The local audit journal is unavailable. No further decision will be sent.", "")
	if offCaller {
		// A drain that began since the run was found active leaves the calls
		// queued: a goroutine started now could outlive Wait.
		if !s.shuttingDown {
			s.eventWG.Add(1)
			go func() {
				defer s.eventWG.Done()
				s.flush()
			}()
		}
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.flush()
}

// emitLocked queues a sink call behind every call queued before it. The
// caller holds mu, queues the call with the state change it reports, and
// flushes once it has released mu.
func (s *Supervisor) emitLocked(call func(Sink)) {
	s.outbox = append(s.outbox, call)
}

// showPromptLocked queues a prompt's view, as emitLocked does.
func (s *Supervisor) showPromptLocked(view View) {
	s.emitLocked(func(sink Sink) { sink.Prompt(view) })
}

// showStatusLocked queues a status, as emitLocked does.
func (s *Supervisor) showStatusLocked(status Status) {
	s.emitLocked(func(sink Sink) { sink.Status(status) })
}

// emit queues a sink call and makes it, with every call queued before it.
func (s *Supervisor) emit(call func(Sink)) {
	s.mu.Lock()
	s.emitLocked(call)
	s.mu.Unlock()
	s.flush()
}

// flush makes the queued sink calls in order, outside mu. One goroutine
// flushes at a time and the others wait for it: when flush returns, every
// call queued before it was called has been made, by this goroutine or by the
// one it waited for. Each goroutine made its own calls once it had released
// mu, and two that changed the state one after the other reached the sink in
// either order: a withdrawal's delivered came before the delivering of the
// answer it overtook, and an exit's status before the running one of the
// delivery it ended.
func (s *Supervisor) flush() {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	for {
		s.mu.Lock()
		calls := s.outbox
		s.outbox = nil
		s.mu.Unlock()
		if len(calls) == 0 {
			return
		}
		for _, call := range calls {
			call(s.sink)
		}
	}
}

func (s *Supervisor) beginDelivery() bool {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	if !s.deliveryAvailable {
		return false
	}
	s.deliveryWG.Add(1)
	return true
}

func (s *Supervisor) endDelivery() { s.deliveryWG.Done() }

func (s *Supervisor) closeDelivery() {
	s.deliveryMu.Lock()
	s.deliveryAvailable = false
	s.deliveryMu.Unlock()
}

func (s *Supervisor) backendFor(sessionID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if index, found := s.agentIndex[strings.ToLower(sessionID)]; found {
		return s.agents[index].Backend
	}
	return ""
}

func (s *Supervisor) pendingExists(key eventKey) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, exists := s.pending[key]
	return exists
}

// setAgentWaitingLocked shows a running agent waiting on a prompt. An agent
// that a Stop or a Restart holds stays "stopping": its process is on its way
// out and no answer reaches it until the operation ends, which then sets the
// status the agent really has. Shown waiting, and running once the prompt was
// withdrawn, it read as ready for input in the middle of its Stop.
func (s *Supervisor) setAgentWaitingLocked(sessionID string) {
	sessionKey := strings.ToLower(sessionID)
	if index, found := s.agentIndex[sessionKey]; found && s.agents[index].Running && !s.stoppingSessions[sessionKey] {
		s.agents[index].Status = "waiting"
	}
}

func (s *Supervisor) restoreAgentRunningLocked(sessionID string) {
	if index, found := s.agentIndex[strings.ToLower(sessionID)]; found && s.agents[index].Running {
		s.agents[index].Status = "running"
	}
}

func (s *Supervisor) markResolvedLocked(key eventKey) {
	if _, exists := s.resolved[key]; exists {
		return
	}
	s.resolved[key] = struct{}{}
	s.resolvedOrder = append(s.resolvedOrder, key)
	if len(s.resolvedOrder) <= maxResolvedEvents {
		return
	}
	oldest := s.resolvedOrder[0]
	s.resolvedOrder = append(s.resolvedOrder[:0], s.resolvedOrder[1:]...)
	delete(s.resolved, oldest)
}

func (s *Supervisor) rebuildPendingLocked() {
	items := make([]View, 0, len(s.pending))
	for _, item := range s.pending {
		items = append(items, item.view)
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].Timestamp == items[right].Timestamp {
			if items[left].SessionID == items[right].SessionID {
				return items[left].ID < items[right].ID
			}
			return items[left].SessionID < items[right].SessionID
		}
		return items[left].Timestamp < items[right].Timestamp
	})
	s.pendingViews = items
}

func (s *Supervisor) emitSafeError(code, message, sessionID string) {
	s.mu.Lock()
	s.emitSafeErrorLocked(code, message, sessionID)
	s.mu.Unlock()
	s.flush()
}

// emitSafeErrorLocked queues a failure to show, as emitLocked does.
func (s *Supervisor) emitSafeErrorLocked(code, message, sessionID string) {
	failure := SafeError{
		RunID:     s.runID,
		Code:      code,
		Message:   message,
		SessionID: sessionID,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
	s.emitLocked(func(sink Sink) { sink.Error(failure) })
}

func makeEventKey(sessionID, eventID string) eventKey {
	return eventKey{
		sessionID: strings.ToLower(strings.TrimSpace(sessionID)),
		eventID:   strings.TrimSpace(eventID),
	}
}
