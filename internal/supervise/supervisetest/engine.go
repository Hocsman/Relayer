// Package supervisetest is the fake runtime the front ends' tests run the
// supervision core over: an Engine that answers as *app.DesktopRuntime does,
// and that a test sets to fail, block or answer otherwise; a Clock that moves
// only when the test moves it; and WaitFor. The core and the desktop each
// copied the engine they needed, and the copies drifted from the runtime and
// from each other; a third front end shares this one instead.
//
// It is for tests only. It imports testing, and no production file may import
// it (TestNoProductionFileImportsSupervisetest).
package supervisetest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// ErrJournalUnavailable is what RecordAudit returns once the journal fails
// (SetAuditFailAt).
var ErrJournalUnavailable = errors.New("supervisetest: the journal is unavailable")

// Apply is one answer the engine began to write: the only place a typed
// answer appears, since the core journals none.
type Apply struct {
	SessionID   string
	Event       adapters.Event
	Decision    adapters.Decision
	ManualInput string
}

// Line is one line the engine began to write.
type Line struct {
	SessionID string
	Text      string
}

// Lifecycle names one of the operations on an agent's process.
type Lifecycle string

// The operations of StopAgent, StartAgent and RestartAgent.
const (
	Stop    Lifecycle = "stop"
	Start   Lifecycle = "start"
	Restart Lifecycle = "restart"
)

// Engine is a supervise.Engine that answers as the runtime
// (*app.DesktopRuntime) does, from what a test set. Until it is told
// otherwise, it asks about every prompt for the default reason and supports no
// answer but a typed one, as the generic adapter does. It checks no session's
// pending prompt, writes every answer and every line the runtime's line
// boundary takes, and stops, starts and restarts every agent at once. It
// takes every exit for the current process's, journals every entry, and puts
// no limit on the policy's consecutive decisions.
//
// A call whose context has already ended is refused with the context's error
// before it writes or changes anything, as the runtime's backends refuse it
// (ptybackend.Manager.check, the tmux backend's PendingEvent,
// adapters.Processor.SendLine). The core does not watch its run's context, so
// a front end that cancels it before the run drains sees the writes that
// follow fail, as they would on the runtime.
//
// It is safe for concurrent use: the core calls it from several goroutines,
// and a test sets its knobs from its own, at any time.
type Engine struct {
	mu sync.Mutex

	evaluation     policy.Evaluation
	evaluationByID map[string]policy.Evaluation
	supported      []adapters.Decision
	// maxConsecutiveAuto and consecutiveAuto are the runtime's policy
	// tracker: how many of the policy's decisions each session had journaled
	// since a person's decision, a Start or a Restart. The count is kept under
	// the session ID as it was journaled, as policy.Tracker keeps it, not
	// under sessionKey.
	maxConsecutiveAuto int
	consecutiveAuto    map[string]int

	// pending is, for each session whose prompt a test set, the prompt its
	// backend holds, or nil when it holds none. A session missing from it is
	// not checked.
	pending map[string]*adapters.Event

	applies      []Apply
	applyErrs    []error
	applyStarted chan<- string
	applyRelease <-chan struct{}

	lines       []Line
	lineErr     error
	lineStarted chan<- string
	lineRelease <-chan struct{}

	lifecycle  map[Lifecycle]*lifecycleKnobs
	markExited func(agentID string) bool

	entries     []audit.Entry
	auditCalls  int
	auditFailAt int
}

// lifecycleKnobs are how one lifecycle operation answers, and the agents it
// was called for.
type lifecycleKnobs struct {
	err     error
	started chan<- string
	release <-chan struct{}
	calls   []string
}

var _ supervise.Engine = (*Engine)(nil)

// NewEngine returns an engine with the defaults Engine describes.
func NewEngine() *Engine {
	return &Engine{
		evaluation: policy.Evaluation{
			Action:         policy.ActionAsk,
			ProposedAction: policy.ActionAsk,
			Reason:         policy.ReasonDefault,
		},
		evaluationByID:  make(map[string]policy.Evaluation),
		consecutiveAuto: make(map[string]int),
		pending:         make(map[string]*adapters.Event),
		lifecycle: map[Lifecycle]*lifecycleKnobs{
			Stop:    {},
			Start:   {},
			Restart: {},
		},
	}
}

// SetEvaluation sets the policy's evaluation of every prompt SetEvaluationFor
// names no other for.
func (e *Engine) SetEvaluation(evaluation policy.Evaluation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.evaluation = evaluation
}

// SetEvaluationFor sets the policy's evaluation of the prompt with that ID, in
// every session.
func (e *Engine) SetEvaluationFor(eventID string, evaluation policy.Evaluation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.evaluationByID[eventID] = evaluation
}

// SetSupported sets what the adapters encode for every prompt besides a typed
// answer: what SupportedDecisions reports, and the only decisions
// ApplyDecision writes. The runtime asks the encoder itself, so an answer it
// reports unsupported is one it cannot write either.
func (e *Engine) SetSupported(decisions ...adapters.Decision) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.supported = append([]adapters.Decision(nil), decisions...)
}

// SetMaxConsecutiveAuto sets the policy's max_consecutive_auto_decisions;
// zero, the default, sets no limit. Once a session has journaled that many decisions
// by the policy, Evaluate asks about its prompts the policy would answer, with
// the reason policy.ReasonConsecutiveLimit, as policy.Tracker.CheckLimits does.
// The count is the runtime's (DesktopRuntime.RecordAudit): the policy's
// decision entries add one to it, and a person's decision entry sets it back
// to zero, as does a Start or a Restart that succeeds
// (agentLifecycle.StartAgent). Nothing else changes it, a line, a Stop, an
// exit and a call that failed included.
//
// As the tracker does, the engine keeps the count under the session ID each
// entry was journaled with, and reads it under the ID of the prompt Evaluate
// is given, case and all. A Start or a Restart finds the agent whatever the
// case of the ID it is handed, as the lifecycle does, which resets the count
// kept under the agent's configured ID; the engine knows no configured ID,
// and resets every count kept under an ID that matches.
func (e *Engine) SetMaxConsecutiveAuto(limit int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.maxConsecutiveAuto = limit
}

// SetPending sets the prompt the session's backend holds, or none when event
// is nil. From then on the session is checked as the runtime checks it:
// ApplyDecision refuses an answer to any other prompt with
// adapters.ErrEventMismatch (backendRouter.ApplyDecision), SendLine refuses a
// line while an actionable prompt is held with adapters.ErrEventPending, and
// PendingEvent returns the prompt, which the core takes in after either
// refusal. The backend lets go of the prompt once its answer is written, and
// of every prompt once the agent is stopped, started or restarted; the
// session stays checked, holding nothing, so an answer to the prompt it let go
// of is a mismatch.
func (e *Engine) SetPending(sessionID string, event *adapters.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var held *adapters.Event
	if event != nil {
		clone := event.Clone()
		held = &clone
	}
	e.pending[sessionKey(sessionID)] = held
}

// SetApplyErrs sets the errors the next answers the engine writes return, one
// each and in order, as the transport's: an answer it refuses before writing
// takes none of them. Once they are used, every answer is written.
func (e *Engine) SetApplyErrs(errs ...error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.applyErrs = append([]error(nil), errs...)
}

// HoldApplies holds every answer the engine writes from then on: started, when
// not nil, receives the session of each as its write begins, and release, when
// not nil, holds the write until it is closed. A held write also ends when its
// context does, with the context's error, as the runtime's does. Either
// channel may be nil; a nil pair holds nothing.
func (e *Engine) HoldApplies(started chan<- string, release <-chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.applyStarted, e.applyRelease = started, release
}

// SetLineErr sets the error every line the engine writes returns from then
// on, until it is set to nil: a terminal error the core tells apart (terminal.ErrClosed,
// terminal.ErrInvalidLine and the others) or any other, which is a line whose
// delivery is uncertain.
func (e *Engine) SetLineErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lineErr = err
}

// HoldLines holds every line the engine writes from then on, as HoldApplies
// holds answers.
func (e *Engine) HoldLines(started chan<- string, release <-chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lineStarted, e.lineRelease = started, release
}

// SetLifecycleErr sets the error every call of operation returns from then on,
// until it is set to nil.
func (e *Engine) SetLifecycleErr(operation Lifecycle, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.knobsFor(operation).err = err
}

// HoldLifecycle holds every call of operation from then on, as HoldApplies
// holds answers: started receives the agent of each.
func (e *Engine) HoldLifecycle(operation Lifecycle, started chan<- string, release <-chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	knobs := e.knobsFor(operation)
	knobs.started, knobs.release = started, release
}

// SetMarkExited answers MarkProcessExited in place of the default, which
// takes every exit for the current process's. It is called outside the
// engine's lock, and may read the engine.
func (e *Engine) SetMarkExited(markExited func(agentID string) bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.markExited = markExited
}

// SetAuditFailAt makes the journal fail at the call-th RecordAudit, counted
// from the engine's creation, and at every call after it: the runtime's
// recorder keeps the first error it met. Zero, the default, never fails.
// While nothing else is journaling, SetAuditFailAt(len(e.Entries())+1) fails
// the next entry.
func (e *Engine) SetAuditFailAt(call int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.auditFailAt = call
}

// Entries returns what was journaled, in order.
func (e *Engine) Entries() []audit.Entry {
	e.mu.Lock()
	defer e.mu.Unlock()
	entries := make([]audit.Entry, len(e.entries))
	for index, entry := range e.entries {
		entry.Metadata = cloneMetadata(entry.Metadata)
		entries[index] = entry
	}
	return entries
}

// Applies returns the answers the engine began to write, in order. An answer
// it refused before writing anything, as the runtime does a mismatch, an
// answer whose context had ended or one the adapter cannot encode, is not
// among them.
func (e *Engine) Applies() []Apply {
	e.mu.Lock()
	defer e.mu.Unlock()
	applies := make([]Apply, len(e.applies))
	for index, apply := range e.applies {
		apply.Event = apply.Event.Clone()
		applies[index] = apply
	}
	return applies
}

// Lines returns the lines the engine began to write, in order. A line it
// refused, as not one line of text, once its context had ended or while a
// prompt was pending, is not among them.
func (e *Engine) Lines() []Line {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Line(nil), e.lines...)
}

// LifecycleCalls returns the agents operation was called for, in order,
// whether or not the call succeeded.
func (e *Engine) LifecycleCalls(operation Lifecycle) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.knobsFor(operation).calls...)
}

// Evaluate returns the evaluation set for the prompt, held back by the limit
// of consecutive decisions as the runtime's Evaluate holds it back.
func (e *Engine) Evaluate(event adapters.Event) policy.Evaluation {
	e.mu.Lock()
	defer e.mu.Unlock()
	evaluation, found := e.evaluationByID[event.ID]
	if !found {
		evaluation = e.evaluation
	}
	evaluation.EventID = event.ID
	if evaluation.Automatic && e.maxConsecutiveAuto > 0 &&
		e.consecutiveAuto[event.SessionID] >= e.maxConsecutiveAuto {
		evaluation.Action = policy.ActionAsk
		evaluation.Automatic = false
		evaluation.Reason = policy.ReasonConsecutiveLimit
	}
	return evaluation
}

// SupportedDecisions returns what SetSupported set, for every prompt.
func (e *Engine) SupportedDecisions(adapters.Event) []adapters.Decision {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]adapters.Decision(nil), e.supported...)
}

// ApplyDecision writes an answer as the runtime does. Before it writes
// anything, in the router's order (backendRouter.ApplyDecision), it refuses a
// prompt with no ID or from another session, then an answer whose context has
// ended, which the backend refuses when it is asked for the prompt it holds,
// then an answer to a prompt the session's backend does not hold, then one the
// adapter cannot encode. It returns the transport's error once the write ends.
func (e *Engine) ApplyDecision(
	ctx context.Context,
	sessionID string,
	event adapters.Event,
	decision adapters.Decision,
	manualInput string,
) error {
	key := sessionKey(sessionID)
	e.mu.Lock()
	if strings.TrimSpace(event.ID) == "" || sessionKey(event.SessionID) != key {
		e.mu.Unlock()
		return fmt.Errorf("%w: event %q does not belong to session %q", adapters.ErrEventMismatch, event.ID, sessionID)
	}
	if err := ctx.Err(); err != nil {
		e.mu.Unlock()
		return err
	}
	if held, checked := e.pending[key]; checked && (held == nil || held.ID != event.ID) {
		e.mu.Unlock()
		return fmt.Errorf("%w: the backend of session %q does not hold %q", adapters.ErrEventMismatch, sessionID, event.ID)
	}
	if decision != adapters.DecisionManual && !containsDecision(e.supported, decision) {
		e.mu.Unlock()
		return fmt.Errorf("%w: %q", adapters.ErrDecisionUnsupported, decision)
	}
	e.applies = append(e.applies, Apply{
		SessionID:   sessionID,
		Event:       event.Clone(),
		Decision:    decision,
		ManualInput: manualInput,
	})
	var err error
	if len(e.applyErrs) > 0 {
		err = e.applyErrs[0]
		e.applyErrs = e.applyErrs[1:]
	}
	started, release := e.applyStarted, e.applyRelease
	e.mu.Unlock()
	if held := hold(ctx, sessionID, started, release); err == nil {
		err = held
	}
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if held := e.pending[key]; held != nil && held.ID == event.ID {
		e.pending[key] = nil
	}
	return nil
}

// PendingEvent returns the prompt SetPending set for the session, or none. It
// refuses a context that has ended, as both backends do.
func (e *Engine) PendingEvent(ctx context.Context, sessionID string) (*adapters.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	held := e.pending[sessionKey(sessionID)]
	if held == nil {
		return nil, nil
	}
	clone := held.Clone()
	return &clone, nil
}

// SendLine writes a line as the runtime does. Before it writes anything, it
// refuses a line that is not one line of text (adapters.ValidateLine) with
// adapters.ErrInvalidLine, then one whose context has ended with the
// context's error, then one sent while the session's backend holds an
// actionable prompt with adapters.ErrEventPending. That is the processor's
// order (adapters.Processor.SendLine): the core leaves the check of a line to
// the runtime, and an invalid line is refused as invalid even while a prompt
// is held.
func (e *Engine) SendLine(ctx context.Context, sessionID, line string) error {
	if err := adapters.ValidateLine(line); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	if held := e.pending[sessionKey(sessionID)]; held != nil && held.Actionable() {
		e.mu.Unlock()
		return adapters.ErrEventPending
	}
	e.lines = append(e.lines, Line{SessionID: sessionID, Text: line})
	err, started, release := e.lineErr, e.lineStarted, e.lineRelease
	e.mu.Unlock()
	if held := hold(ctx, sessionID, started, release); err == nil {
		err = held
	}
	return err
}

// StopAgent stops the agent's process, as SetLifecycleErr and HoldLifecycle
// say for Stop.
func (e *Engine) StopAgent(ctx context.Context, agentID string) error {
	return e.runLifecycle(ctx, Stop, agentID)
}

// StartAgent starts a new process for the agent, as SetLifecycleErr and
// HoldLifecycle say for Start.
func (e *Engine) StartAgent(ctx context.Context, agentID string) error {
	return e.runLifecycle(ctx, Start, agentID)
}

// RestartAgent replaces the agent's process, as SetLifecycleErr and
// HoldLifecycle say for Restart.
func (e *Engine) RestartAgent(ctx context.Context, agentID string) error {
	return e.runLifecycle(ctx, Restart, agentID)
}

// runLifecycle is one call of operation. One that succeeds leaves the backend
// holding no prompt of the agent, whose process is gone or new, and a Start or
// a Restart resets its count of the policy's consecutive decisions. One that
// fails changes neither: the engine takes it for a call that left the process
// as it was, which a stop the backend could not confirm may have done. In the
// runtime, a Restart whose stop succeeded and whose start failed has let go of
// the prompt with the old process (agentLifecycle.StartAgent releases the old
// session before it starts the new one); the engine does not tell that one
// apart. A call whose context has already ended fails before it holds or
// changes anything.
func (e *Engine) runLifecycle(ctx context.Context, operation Lifecycle, agentID string) error {
	e.mu.Lock()
	knobs := e.knobsFor(operation)
	knobs.calls = append(knobs.calls, agentID)
	err, started, release := knobs.err, knobs.started, knobs.release
	e.mu.Unlock()
	if ended := ctx.Err(); ended != nil {
		return ended
	}
	if held := hold(ctx, agentID, started, release); err == nil {
		err = held
	}
	if err != nil {
		return err
	}
	key := sessionKey(agentID)
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, checked := e.pending[key]; checked {
		e.pending[key] = nil
	}
	if operation != Stop {
		for sessionID := range e.consecutiveAuto {
			if sessionKey(sessionID) == key {
				delete(e.consecutiveAuto, sessionID)
			}
		}
	}
	return nil
}

// MarkProcessExited reports whether the exit is the current process's, as
// SetMarkExited says; by default it always is.
func (e *Engine) MarkProcessExited(agentID string) bool {
	e.mu.Lock()
	markExited := e.markExited
	e.mu.Unlock()
	if markExited != nil {
		return markExited(agentID)
	}
	return true
}

// RecordAudit journals the entry as it was given, so that a test sees what
// the core handed the journal before any redaction, and counts the decisions
// the limit of consecutive decisions reads, as the runtime's does.
func (e *Engine) RecordAudit(entry audit.Entry) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.auditCalls++
	if e.auditFailAt > 0 && e.auditCalls >= e.auditFailAt {
		return ErrJournalUnavailable
	}
	entry.Metadata = cloneMetadata(entry.Metadata)
	e.entries = append(e.entries, entry)
	if entry.Kind == audit.KindDecision {
		switch entry.DecisionBy {
		case audit.DecisionByPolicy:
			e.consecutiveAuto[entry.SessionID]++
		case audit.DecisionByHuman:
			delete(e.consecutiveAuto, entry.SessionID)
		}
	}
	return nil
}

// knobsFor is the knobs of operation; the caller holds e.mu.
func (e *Engine) knobsFor(operation Lifecycle) *lifecycleKnobs {
	knobs, found := e.lifecycle[operation]
	if !found {
		knobs = &lifecycleKnobs{}
		e.lifecycle[operation] = knobs
	}
	return knobs
}

// hold is a held call: started, when not nil, receives id, then the call
// waits until release, when not nil, is closed. It returns the context's
// error when the context ends first, and nil otherwise.
func hold(ctx context.Context, id string, started chan<- string, release <-chan struct{}) error {
	if started != nil {
		select {
		case started <- id:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if release == nil {
		return nil
	}
	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sessionKey is how the runtime's router and lifecycle match a session or an
// agent: whatever its case, and, as the lifecycle and the router's check of an
// answer do, whatever space surrounds it. The count of the policy's
// consecutive decisions is not kept under it (SetMaxConsecutiveAuto).
func sessionKey(sessionID string) string {
	return strings.ToLower(strings.TrimSpace(sessionID))
}

func containsDecision(decisions []adapters.Decision, decision adapters.Decision) bool {
	for _, candidate := range decisions {
		if candidate == decision {
			return true
		}
	}
	return false
}

func cloneMetadata(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
