package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/terminal"
)

// Per-agent lifecycle states owned by agentLifecycle. The identity of an agent
// never changes within a run; only its process state does.
const (
	agentStateRunning       = "running"
	agentStateStopping      = "stopping"
	agentStateStopped       = "stopped"
	agentStateStarting      = "starting"
	agentStateStopUncertain = "stop_uncertain"
	agentStateStartFailed   = "start_failed"
)

var (
	errAgentNotRunning     = errors.New("agent has no running process")
	errAgentRunning        = errors.New("agent process is still running")
	errAgentBusy           = errors.New("an agent lifecycle operation is already in progress")
	errAgentStopUncertain  = errors.New("the previous stop was never confirmed, so no replacement may start")
	errLifecycleRouterDown = errors.New("the backend router is unavailable")
)

// agentLifecycle coordinates operator-initiated stop/start/restart of
// individual agents against one router while keeping the audit trail complete
// and fail-closed. Both presentations (TUI and desktop GUI) share it so the
// safety rules of hot lifecycle live in exactly one place:
//
//   - a replacement process never starts while the previous stop is
//     unconfirmed (no two processes may ever share one agent identity);
//   - session_started is recorded before the process launches, mirroring the
//     startup invariant that audit exists before any agent runs;
//   - every transition carries the human operator as its actor.
type agentLifecycle struct {
	router  *backendRouter
	auditor *audit.Recorder
	specs   map[string]agent.Spec
	size    terminal.Size
	tracker *policy.Tracker

	mu       sync.Mutex
	states   map[string]string
	restarts map[string]int
}

// newAgentLifecycle takes ownership of no resource; callers keep closing the
// router and the auditor. Every validated spec starts in the running state
// because both run paths only build the controller after their startup loop
// succeeded (startup is all-or-nothing and rolls back otherwise).
func newAgentLifecycle(
	router *backendRouter,
	auditor *audit.Recorder,
	specs []agent.Spec,
	size terminal.Size,
	tracker *policy.Tracker,
) *agentLifecycle {
	lifecycle := &agentLifecycle{
		router:   router,
		auditor:  auditor,
		specs:    make(map[string]agent.Spec, len(specs)),
		size:     size.Normalize(),
		tracker:  tracker,
		states:   make(map[string]string, len(specs)),
		restarts: make(map[string]int, len(specs)),
	}
	for _, spec := range specs {
		key := lifecycleKey(spec.ID)
		lifecycle.specs[key] = spec
		lifecycle.states[key] = agentStateRunning
	}
	return lifecycle
}

func lifecycleKey(agentID string) string {
	return strings.ToLower(strings.TrimSpace(agentID))
}

// StopAgent strictly terminates one agent's process without affecting its
// siblings. A stop that cannot be confirmed locks the identity: StartAgent
// refuses to place a replacement process until a later StopAgent retry
// succeeds.
func (l *agentLifecycle) StopAgent(ctx context.Context, agentID, reason string) error {
	if l == nil || l.router == nil {
		return errLifecycleRouterDown
	}
	key := lifecycleKey(agentID)
	l.mu.Lock()
	spec, known := l.specs[key]
	switch {
	case !known:
		l.mu.Unlock()
		return fmt.Errorf("%w: %q", terminal.ErrSessionNotFound, agentID)
	case l.states[key] == agentStateStopping || l.states[key] == agentStateStarting:
		l.mu.Unlock()
		return errAgentBusy
	case l.states[key] == agentStateStopped || l.states[key] == agentStateStartFailed:
		l.mu.Unlock()
		return errAgentNotRunning
	}
	l.states[key] = agentStateStopping
	l.mu.Unlock()

	stopErr := l.router.Stop(ctx, spec.ID)
	l.mu.Lock()
	if stopErr != nil {
		l.states[key] = agentStateStopUncertain
	} else {
		l.states[key] = agentStateStopped
	}
	l.mu.Unlock()
	if stopErr != nil {
		l.recordLifecycle(audit.Entry{
			Kind:       audit.KindBackendError,
			SessionID:  spec.ID,
			AgentID:    spec.ID,
			Backend:    spec.Backend,
			Adapter:    spec.Adapter,
			DecisionBy: audit.DecisionByHuman,
			Outcome:    audit.OutcomeFailed,
			Reason:     "operator_stop_unconfirmed",
		})
		return fmt.Errorf("stopping agent %q: %w", spec.ID, stopErr)
	}
	if err := l.recordLifecycle(audit.Entry{
		Kind:       audit.KindSessionFinished,
		SessionID:  spec.ID,
		AgentID:    spec.ID,
		Backend:    spec.Backend,
		Adapter:    spec.Adapter,
		DecisionBy: audit.DecisionByHuman,
		Outcome:    audit.OutcomeFinished,
		Reason:     reason,
	}); err != nil {
		return fmt.Errorf("auditing the stop of agent %q: %w", spec.ID, err)
	}
	return nil
}

// StartAgent launches a fresh process for one stopped agent under its
// unchanged identity and immutable plan specification. The session_started
// record is written before the process launches; when the journal refuses it,
// no process is started at all.
func (l *agentLifecycle) StartAgent(ctx context.Context, agentID, reason string) (terminal.Info, error) {
	if l == nil || l.router == nil {
		return terminal.Info{}, errLifecycleRouterDown
	}
	key := lifecycleKey(agentID)
	l.mu.Lock()
	spec, known := l.specs[key]
	switch {
	case !known:
		l.mu.Unlock()
		return terminal.Info{}, fmt.Errorf("%w: %q", terminal.ErrSessionNotFound, agentID)
	case l.states[key] == agentStateStopping || l.states[key] == agentStateStarting:
		l.mu.Unlock()
		return terminal.Info{}, errAgentBusy
	case l.states[key] == agentStateRunning:
		l.mu.Unlock()
		return terminal.Info{}, errAgentRunning
	case l.states[key] == agentStateStopUncertain:
		l.mu.Unlock()
		return terminal.Info{}, errAgentStopUncertain
	}
	l.states[key] = agentStateStarting
	l.mu.Unlock()

	fail := func(err error) (terminal.Info, error) {
		l.mu.Lock()
		l.states[key] = agentStateStartFailed
		l.mu.Unlock()
		return terminal.Info{}, err
	}

	// Release the previous identity. A missing route is fine (the session may
	// never have registered one); every other failure means the backend could
	// not prove the previous process is gone.
	if err := l.router.Remove(ctx, spec.ID); err != nil && !errors.Is(err, terminal.ErrSessionNotFound) {
		l.mu.Lock()
		l.states[key] = agentStateStopUncertain
		l.mu.Unlock()
		return terminal.Info{}, fmt.Errorf("releasing agent %q: %w", spec.ID, err)
	}

	l.mu.Lock()
	l.restarts[key]++
	l.mu.Unlock()
	// Human lifecycle records deliberately carry no metadata: the audit model
	// (internal/audit/redact.go) drops every free-form field from entries with
	// a human actor, and the operator_start reason is the lifecycle evidence.
	if err := l.recordLifecycle(audit.Entry{
		Kind:       audit.KindSessionStarted,
		SessionID:  spec.ID,
		AgentID:    spec.ID,
		Backend:    spec.Backend,
		Adapter:    spec.Adapter,
		DecisionBy: audit.DecisionByHuman,
		Outcome:    audit.OutcomeStarted,
		Reason:     reason,
	}); err != nil {
		l.mu.Lock()
		l.states[key] = agentStateStopped
		l.mu.Unlock()
		return terminal.Info{}, fmt.Errorf("auditing the start of agent %q: %w", spec.ID, err)
	}

	info, err := l.router.Start(ctx, spec, l.size)
	if err != nil {
		l.recordLifecycle(audit.Entry{
			Kind:       audit.KindBackendError,
			SessionID:  spec.ID,
			AgentID:    spec.ID,
			Backend:    spec.Backend,
			Adapter:    spec.Adapter,
			DecisionBy: audit.DecisionByHuman,
			Outcome:    audit.OutcomeFailed,
			Reason:     "operator_start_failed",
		})
		return fail(fmt.Errorf("starting agent %q: %w", spec.ID, err))
	}
	l.mu.Lock()
	l.states[key] = agentStateRunning
	l.mu.Unlock()
	if l.tracker != nil {
		// A fresh process follows an explicit operator action, so the
		// consecutive automatic-decision streak restarts with it.
		l.tracker.Reset(spec.ID)
	}
	return info, nil
}

// RestartAgent is the transactional stop-then-start of one agent. When the
// stop cannot be confirmed the start never happens; when the start fails the
// agent stays down in an explicit failed state instead of silently keeping a
// half-replaced process.
func (l *agentLifecycle) RestartAgent(ctx context.Context, agentID string) (terminal.Info, error) {
	key := lifecycleKey(agentID)
	l.mu.Lock()
	state := l.states[key]
	_, known := l.specs[key]
	l.mu.Unlock()
	if !known {
		return terminal.Info{}, fmt.Errorf("%w: %q", terminal.ErrSessionNotFound, agentID)
	}
	if state == agentStateRunning || state == agentStateStopUncertain {
		if err := l.StopAgent(ctx, agentID, "operator_restart"); err != nil {
			return terminal.Info{}, err
		}
	}
	return l.StartAgent(ctx, agentID, "operator_restart")
}

// finishedRecorded reports whether session_finished was durably recorded for
// the agent's current process instance, letting strict shutdown paths avoid a
// duplicate terminal record for an operator-stopped agent.
func (l *agentLifecycle) finishedRecorded(agentID string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.states[lifecycleKey(agentID)] == agentStateStopped
}

func (l *agentLifecycle) recordLifecycle(entry audit.Entry) error {
	if l == nil || l.auditor == nil {
		return nil
	}
	return l.auditor.Record(entry)
}
