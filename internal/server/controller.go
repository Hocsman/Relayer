package server

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/app"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/notify"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/preflight"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/telemetry"
	"github.com/Hocsman/Relayer/internal/terminal"
	"github.com/Hocsman/Relayer/internal/toolcatalog"
)

const (
	eventSnapshot     = "relayer:snapshot"
	eventSemantic     = "relayer:event"
	eventStatus       = "relayer:status"
	eventError        = "relayer:error"
	eventNotification = "relayer:notification"
	eventPresence     = "relayer:presence"
	eventHand         = "relayer:hand"
	eventRecording    = "relayer:recording"

	minAgentProfiles = 1
	maxAgentProfiles = 8
)

var (
	profileIDRegex    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	errStaleRevision  = errors.New("configuration has changed, reload before saving")
	errStaleRun       = errors.New("run has changed, reload before retrying")
	errUnknownSession = errors.New("unknown session")
)

type pendingItem struct {
	event adapters.Event
	view  SupervisionEvent
}

// Controller owns the headless supervisor runtime and provides thread-safe
// query and mutation methods mirroring the RelayerBridge interface.
type Controller struct {
	mu          sync.RWMutex
	configPath  string
	diagnostics io.Writer

	ctx    context.Context
	cancel context.CancelFunc

	plan    *app.DesktopPlan
	runtime *app.DesktopRuntime
	runID   string

	state       AppState
	agentIndex  map[string]int // lowercase sessionID -> slice index
	pending     map[string]pendingItem
	subscribers map[uint64]func(event string, payload any)
	nextSubID   uint64

	revisionHash  string
	revisionToken string

	presence           map[string]presenceEntry // connID -> live connection
	hands              map[string]handState     // lowercase sessionID -> write lock
	requestTimeout     time.Duration
	allowForceTakeover bool

	detector toolcatalog.Detector
	notifier notify.Notifier
	stopped  int32
}

// DefaultConfigPath resolves the default config file location.
func DefaultConfigPath() (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("RELAYER_CONFIG")); explicit != "" {
		return filepath.Clean(explicit), nil
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return filepath.Abs(config.DefaultPath)
	}
	return filepath.Join(root, "relayer", config.DefaultPath), nil
}

// NewController creates an unstarted supervisor controller.
func NewController(configPath string, diagnostics io.Writer) (*Controller, error) {
	if strings.TrimSpace(configPath) == "" {
		defaultPath, err := DefaultConfigPath()
		if err != nil {
			return nil, fmt.Errorf("resolving default config path: %w", err)
		}
		configPath = defaultPath
	}
	if diagnostics == nil {
		diagnostics = io.Discard
	}

	return &Controller{
		configPath:  configPath,
		diagnostics: diagnostics,
		agentIndex:  make(map[string]int),
		pending:     make(map[string]pendingItem),
		subscribers: make(map[uint64]func(event string, payload any)),
		detector:    toolcatalog.DefaultDetector(),
		notifier:    notify.New(notify.DefaultConfig(), diagnostics),
	}, nil
}

// Start boots the supervisor runtime and begins event processing.
func (c *Controller) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.startLocked(ctx)
}

func (c *Controller) startLocked(ctx context.Context) error {
	opts := app.DesktopOptions{
		ConfigPath:  c.configPath,
		Diagnostics: c.diagnostics,
	}

	plan, err := app.PrepareDesktopRuntime(opts)
	if err != nil {
		return fmt.Errorf("preparing desktop runtime: %w", err)
	}

	runIDBytes := make([]byte, 8)
	if _, err := rand.Read(runIDBytes); err != nil {
		return fmt.Errorf("generating run id: %w", err)
	}
	runID := hex.EncodeToString(runIDBytes)

	runCtx, cancel := context.WithCancel(ctx)
	rt, err := app.StartDesktopRuntime(runCtx, plan, runID)
	if err != nil {
		cancel()
		return fmt.Errorf("starting desktop runtime: %w", err)
	}

	c.ctx = runCtx
	c.cancel = cancel
	c.plan = plan
	c.runtime = rt
	c.runID = runID

	metadata := rt.Metadata()
	if metadata.Notifications.Enabled {
		c.notifier = notify.New(metadata.Notifications, c.diagnostics)
	}

	sessions := rt.Sessions()
	agents := make([]AgentState, 0, len(sessions))
	c.agentIndex = make(map[string]int, len(sessions))

	for i, s := range sessions {
		out, _ := rt.Output(s.ID)
		c.agentIndex[strings.ToLower(s.ID)] = i
		agents = append(agents, AgentState{
			SessionID:      s.ID,
			AgentID:        s.ID,
			Name:           s.Name,
			DisplayCommand: s.Command,
			Backend:        s.Backend,
			Adapter:        s.Adapter,
			Status:         "running",
			Output:         out,
			Revision:       1,
			Running:        true,
			Simulated:      s.Simulated,
		})
	}

	c.state = AppState{
		RunID:     runID,
		RunStatus: "running",
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Policy: PolicyState{
			DefaultAction: metadata.PolicyAction,
			DryRun:        metadata.PolicyDryRun,
		},
		Audit: AuditState{
			Enabled: metadata.AuditEnabled,
			Mode:    metadata.AuditMode,
			Status:  map[bool]string{true: "ready", false: "disabled"}[metadata.AuditEnabled],
			Path:    metadata.AuditPath,
		},
		Agents:        agents,
		PendingEvents: []SupervisionEvent{},
		Notices:       rt.StartupLogs(),
	}

	go c.eventLoop(runCtx, rt)

	return nil
}

// Close gracefully stops the running supervisor and all agent processes.
func (c *Controller) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if atomic.CompareAndSwapInt32(&c.stopped, 0, 1) {
		if c.cancel != nil {
			c.cancel()
		}
		if c.runtime != nil {
			return c.runtime.Close(ctx)
		}
	}
	return nil
}

// Subscribe registers an event listener called on each broadcast.
// Returns an unsubscribe function.
func (c *Controller) Subscribe(listener func(event string, payload any)) func() {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextSubID
	c.nextSubID++
	c.subscribers[id] = listener

	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.subscribers, id)
	}
}

func (c *Controller) broadcast(event string, payload any) {
	c.mu.RLock()
	subs := make([]func(event string, payload any), 0, len(c.subscribers))
	for _, fn := range c.subscribers {
		subs = append(subs, fn)
	}
	c.mu.RUnlock()

	for _, fn := range subs {
		fn(event, payload)
	}
}

func (c *Controller) eventLoop(ctx context.Context, rt *app.DesktopRuntime) {
	events := rt.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			c.handleEvent(ctx, rt, ev)
		}
	}
}

func (c *Controller) handleEvent(ctx context.Context, rt *app.DesktopRuntime, rawEvent session.Event) {
	switch ev := rawEvent.(type) {
	case session.OutputAvailable:
		out, err := rt.AnsiOutput(ev.SessionID)
		if err != nil {
			out, err = rt.Output(ev.SessionID)
		}
		if err != nil {
			return
		}
		c.mu.Lock()
		idx, found := c.agentIndex[strings.ToLower(ev.SessionID)]
		if !found {
			c.mu.Unlock()
			return
		}
		c.state.Agents[idx].Output = out
		c.state.Agents[idx].Revision++
		snap := SnapshotEvent{
			RunID:       c.runID,
			SessionID:   ev.SessionID,
			Revision:    c.state.Agents[idx].Revision,
			Output:      out,
			Status:      c.state.Agents[idx].Status,
			Running:     c.state.Agents[idx].Running,
			Attached:    c.state.Agents[idx].Attached,
			InputFrozen: c.state.Agents[idx].InputFrozen,
			ExitCode:    c.state.Agents[idx].ExitCode,
		}
		c.mu.Unlock()

		c.broadcast(eventSnapshot, snap)

	case session.AdapterEvent:
		adapterEv := ev.Event
		if adapterEv.Type == adapters.EventProcessExit {
			c.mu.Lock()
			idx, found := c.agentIndex[strings.ToLower(adapterEv.SessionID)]
			if found {
				c.state.Agents[idx].Running = false
				c.state.Agents[idx].Status = "stopped"
			}
			c.mu.Unlock()

			c.broadcast(eventStatus, StatusEvent{
				RunID:     c.runID,
				Scope:     "session",
				SessionID: adapterEv.SessionID,
				Status:    "stopped",
			})
			return
		}

		if !adapterEv.Actionable() {
			return
		}

		evaluation := rt.Evaluate(adapterEv)
		decisions := rt.SupportedDecisions(adapterEv)
		decisionStrings := make([]string, 0, len(decisions))
		for _, d := range decisions {
			decisionStrings = append(decisionStrings, string(d))
		}

		ts := adapterEv.Timestamp
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		view := SupervisionEvent{
			RunID:     c.runID,
			ID:        adapterEv.ID,
			SessionID: adapterEv.SessionID,
			AgentID:   adapterEv.AgentID,
			Adapter:   adapterEv.Adapter,
			Type:      string(adapterEv.Type),
			Summary:   adapterEv.Summary,
			Sensitive: adapterEv.Sensitive,
			Risk:      string(adapterEv.Risk),
			Timestamp: ts.Format(time.RFC3339),
			Evaluation: PolicyEvaluation{
				Action:         string(evaluation.Action),
				ProposedAction: string(evaluation.ProposedAction),
				RuleName:       evaluation.RuleName,
				Reason:         evaluation.Reason,
				Automatic:      evaluation.Automatic,
				DryRun:         evaluation.DryRun,
			},
			DeliveryStatus: "pending",
			Decisions:      decisionStrings,
			ToolCall:       toolCallView(adapterEv.ToolCall),
		}

		c.mu.Lock()
		key := adapterEv.SessionID + ":" + adapterEv.ID
		c.pending[key] = pendingItem{event: adapterEv, view: view}
		if idx, found := c.agentIndex[strings.ToLower(adapterEv.SessionID)]; found {
			c.state.Agents[idx].Status = "waiting"
		}
		c.rebuildPendingEventsLocked()
		c.mu.Unlock()

		c.broadcast(eventSemantic, view)
		c.broadcast(eventStatus, StatusEvent{
			RunID:     c.runID,
			Scope:     "session",
			SessionID: adapterEv.SessionID,
			Status:    "waiting",
		})

		// Notification
		notif := notify.Notification{
			Title:     "Relayer Arbitration Required",
			AgentName: adapterEv.AgentID,
			SessionID: adapterEv.SessionID,
			Reason:    evaluation.Reason,
			EventID:   adapterEv.ID,
			Kind:      notify.KindPendingDecision,
			Severity:  notify.SeverityWarning,
			Details:   adapterEv.Summary,
			Timestamp: time.Now().UTC(),
		}
		if c.notifier != nil {
			c.notifier.Notify(notif)
		}

		c.broadcast(eventNotification, NotificationEvent{
			Title:     notif.Title,
			Body:      adapterEv.Summary,
			AgentName: adapterEv.AgentID,
			SessionID: adapterEv.SessionID,
			EventID:   adapterEv.ID,
			Kind:      notif.Kind,
			Severity:  notif.Severity,
			Reason:    evaluation.Reason,
			Timestamp: notif.Timestamp.Format(time.RFC3339),
		})

	case session.AdapterEventWithdrawn:
		adapterEv := ev.Event
		key := adapterEv.SessionID + ":" + adapterEv.ID
		c.mu.Lock()
		delete(c.pending, key)
		if idx, found := c.agentIndex[strings.ToLower(adapterEv.SessionID)]; found {
			c.state.Agents[idx].Status = "running"
		}
		c.rebuildPendingEventsLocked()
		c.mu.Unlock()

		c.broadcast(eventStatus, StatusEvent{
			RunID:     c.runID,
			Scope:     "session",
			SessionID: adapterEv.SessionID,
			Status:    "running",
		})

	case session.Exited:
		c.mu.Lock()
		idx, found := c.agentIndex[strings.ToLower(ev.SessionID)]
		if found {
			c.state.Agents[idx].Running = false
			c.state.Agents[idx].Status = "stopped"
		}
		c.mu.Unlock()

		c.broadcast(eventStatus, StatusEvent{
			RunID:     c.runID,
			Scope:     "session",
			SessionID: ev.SessionID,
			Status:    "stopped",
		})
		c.announceFinishedRecording(ev.SessionID)

	case session.Error:
		c.broadcast(eventError, SafeErrorEvent{
			RunID:     c.runID,
			Code:      "session_error",
			Message:   ev.Err.Error(),
			SessionID: ev.SessionID,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		})
	}
}

func (c *Controller) rebuildPendingEventsLocked() {
	list := make([]SupervisionEvent, 0, len(c.pending))
	for _, item := range c.pending {
		list = append(list, item.view)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Timestamp < list[j].Timestamp
	})
	c.state.PendingEvents = list
}

// -------------------------------------------------------------
// RelayerBridge API Implementations
// -------------------------------------------------------------

func (c *Controller) cloneStateLocked() AppState {
	cloned := c.state
	cloned.Agents = make([]AgentState, len(c.state.Agents))
	copy(cloned.Agents, c.state.Agents)
	cloned.PendingEvents = make([]SupervisionEvent, len(c.state.PendingEvents))
	copy(cloned.PendingEvents, c.state.PendingEvents)
	if c.state.Notices != nil {
		cloned.Notices = make([]string, len(c.state.Notices))
		copy(cloned.Notices, c.state.Notices)
	}
	return cloned
}

func (c *Controller) GetState() AppState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cloneStateLocked()
}

func (c *Controller) RunPreflight(ctx context.Context) (PreflightReport, error) {
	c.mu.RLock()
	configPath := c.configPath
	c.mu.RUnlock()

	opts := app.PreflightOptions{
		ConfigPath: configPath,
	}
	report, err := app.RunPreflight(ctx, opts)
	if err != nil {
		return PreflightReport{}, err
	}

	return projectPreflightReport(report), nil
}

func (c *Controller) SubmitDecision(runID, sessionID, eventID, value string) error {
	return c.SubmitDecisionWithOperator(runID, sessionID, eventID, value, "operator")
}

func (c *Controller) SubmitDecisionWithOperator(runID, sessionID, eventID, value, operator string) error {
	c.mu.Lock()
	if c.runID != runID && runID != "" {
		c.mu.Unlock()
		return errors.New("run is no longer active")
	}

	key := sessionID + ":" + eventID
	item, found := c.pending[key]
	if !found {
		c.mu.Unlock()
		return errors.New("pending event not found")
	}

	rt := c.runtime
	delete(c.pending, key)
	backend := ""
	if idx, ok := c.agentIndex[strings.ToLower(sessionID)]; ok {
		c.state.Agents[idx].Status = "running"
		backend = c.state.Agents[idx].Backend
	}
	c.rebuildPendingEventsLocked()
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if strings.TrimSpace(operator) == "" {
		operator = "operator"
	}

	var (
		decision      adapters.Decision
		manualInput   string
		auditDecision audit.Decision
	)
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "allow", "yes", "y":
		decision = adapters.DecisionAllow
		auditDecision = audit.DecisionAllow
	case "deny", "no", "n":
		decision = adapters.DecisionDeny
		auditDecision = audit.DecisionDeny
	default:
		decision = adapters.DecisionManual
		manualInput = strings.TrimRight(value, "\r\n")
		auditDecision = audit.DecisionAllow
	}

	ruleName := item.view.Evaluation.RuleName

	// 1. Record decision audit entry with operator attribution
	_ = rt.RecordAudit(audit.Entry{
		Kind:       audit.KindDecision,
		SessionID:  strings.TrimSpace(sessionID),
		AgentID:    strings.TrimSpace(item.event.AgentID),
		Backend:    strings.ToLower(strings.TrimSpace(backend)),
		Adapter:    strings.ToLower(strings.TrimSpace(item.event.Adapter)),
		EventID:    strings.TrimSpace(eventID),
		EventType:  item.event.Type,
		Risk:       item.event.Risk,
		Rule:       ruleName,
		Decision:   auditDecision,
		DecisionBy: audit.DecisionByHuman,
		Operator:   operator,
		Outcome:    audit.OutcomeInFlight,
		Reason:     "decision_selected",
		Metadata: map[string]string{
			"operator": operator,
			"role":     "operator",
		},
	})

	err := rt.ApplyDecision(ctx, sessionID, item.event, decision, manualInput)
	if err != nil {
		_ = rt.RecordAudit(audit.Entry{
			Kind:       audit.KindDelivery,
			SessionID:  strings.TrimSpace(sessionID),
			AgentID:    strings.TrimSpace(item.event.AgentID),
			Backend:    strings.ToLower(strings.TrimSpace(backend)),
			Adapter:    strings.ToLower(strings.TrimSpace(item.event.Adapter)),
			EventID:    strings.TrimSpace(eventID),
			EventType:  item.event.Type,
			Risk:       item.event.Risk,
			Rule:       ruleName,
			Decision:   auditDecision,
			DecisionBy: audit.DecisionByHuman,
			Operator:   operator,
			Outcome:    audit.OutcomeFailed,
			Reason:     "delivery_failed",
			Metadata: map[string]string{
				"operator": operator,
				"role":     "operator",
			},
		})
		return err
	}

	_ = rt.RecordAudit(audit.Entry{
		Kind:       audit.KindDelivery,
		SessionID:  strings.TrimSpace(sessionID),
		AgentID:    strings.TrimSpace(item.event.AgentID),
		Backend:    strings.ToLower(strings.TrimSpace(backend)),
		Adapter:    strings.ToLower(strings.TrimSpace(item.event.Adapter)),
		EventID:    strings.TrimSpace(eventID),
		EventType:  item.event.Type,
		Risk:       item.event.Risk,
		Rule:       ruleName,
		Decision:   auditDecision,
		DecisionBy: audit.DecisionByHuman,
		Operator:   operator,
		Outcome:    audit.OutcomeApplied,
		Reason:     "delivery_applied",
		Metadata: map[string]string{
			"operator": operator,
			"role":     "operator",
		},
	})

	item.view.DeliveryStatus = "delivered"
	c.broadcast(eventSemantic, item.view)
	c.broadcast(eventStatus, StatusEvent{
		RunID:     runID,
		Scope:     "session",
		SessionID: sessionID,
		Status:    "running",
	})

	return nil
}

func (c *Controller) SubmitAutomaticDecision(runID, sessionID, eventID, decision string) error {
	return c.SubmitDecision(runID, sessionID, eventID, decision)
}

func (c *Controller) SubmitLine(runID, sessionID, line string) error {
	return c.SubmitLineWithOperator(runID, sessionID, line, "operator")
}

func (c *Controller) SubmitLineWithOperator(runID, sessionID, line, operator string) error {
	c.mu.RLock()
	rt := c.runtime
	idx, hasAgent := c.agentIndex[strings.ToLower(sessionID)]
	var agent AgentState
	if hasAgent && idx < len(c.state.Agents) {
		agent = c.state.Agents[idx]
	}
	c.mu.RUnlock()

	if rt == nil {
		return errors.New("supervisor runtime not ready")
	}

	if strings.TrimSpace(operator) == "" {
		operator = "operator"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := rt.SendLine(ctx, sessionID, line)
	outcome := audit.OutcomeApplied
	reason := "operator_input_applied"
	if err != nil {
		outcome = audit.OutcomeFailed
		reason = "operator_input_invalid"
	}

	_ = rt.RecordAudit(audit.Entry{
		Kind:       audit.KindOperatorInput,
		SessionID:  strings.TrimSpace(sessionID),
		AgentID:    strings.TrimSpace(agent.AgentID),
		Backend:    strings.ToLower(strings.TrimSpace(agent.Backend)),
		Adapter:    strings.ToLower(strings.TrimSpace(agent.Adapter)),
		DecisionBy: audit.DecisionByHuman,
		Operator:   operator,
		Outcome:    outcome,
		Reason:     reason,
	})

	return err
}

// SendTerminalInput delivers raw terminal input bytes directly to the session backend.
// Used by the web interactive terminal (full PTY mode) to stream keystrokes and signals.
//
// The hand is checked before the write. The check and the write cannot share a
// single lock acquisition without holding c.mu across five seconds of I/O, so
// at most one already-in-flight keystroke may land just after a release. That
// window is documented in docs/sharing.md rather than papered over.
func (c *Controller) SendTerminalInput(runID, sessionID string, data []byte, operator, connID string) error {
	c.mu.RLock()
	rt := c.runtime
	currentRun := c.runID
	c.mu.RUnlock()

	if rt == nil {
		return errors.New("supervisor runtime not ready")
	}
	if trimmed := strings.TrimSpace(runID); trimmed != "" && trimmed != currentRun {
		return errStaleRun
	}
	if !c.HoldsHand(sessionID, connID) {
		return ErrNotHolder
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return rt.SendRaw(ctx, sessionID, data)
}

// SetInteractiveSession acquires or releases the session's write lock. It is
// the attach verb: taking the hand is what makes keystrokes routable.
//
// Taking a hand another operator holds is refused rather than silently stolen;
// the caller is expected to request control instead.
func (c *Controller) SetInteractiveSession(runID, sessionID string, active bool, operator, connID string) error {
	c.mu.RLock()
	rt := c.runtime
	idx, hasAgent := c.agentIndex[strings.ToLower(strings.TrimSpace(sessionID))]
	var agent AgentState
	if hasAgent && idx < len(c.state.Agents) {
		agent = c.state.Agents[idx]
	}
	c.mu.RUnlock()

	if rt == nil {
		return errors.New("supervisor runtime not ready")
	}
	if !hasAgent {
		return errUnknownSession
	}

	var err error
	if active {
		_, err = c.TakeControl(sessionID, connID, operator)
	} else {
		_, err = c.ReleaseControl(sessionID, connID, operator)
	}
	if err != nil {
		return err
	}

	if strings.TrimSpace(operator) == "" {
		operator = "operator"
	}

	kind := audit.KindAttachStarted
	outcome := "attached"
	if !active {
		kind = audit.KindAttachFinished
		outcome = "detached"
	}

	_ = rt.RecordAudit(audit.Entry{
		Kind:       kind,
		SessionID:  strings.TrimSpace(sessionID),
		AgentID:    strings.TrimSpace(agent.AgentID),
		Backend:    strings.ToLower(strings.TrimSpace(agent.Backend)),
		Adapter:    strings.ToLower(strings.TrimSpace(agent.Adapter)),
		DecisionBy: audit.DecisionByHuman,
		Operator:   operator,
		Outcome:    audit.OutcomeApplied,
		Reason:     "operator_interactive_" + outcome,
		Metadata: map[string]string{
			"operator": operator,
			"role":     c.roleFor(connID),
			"conn_id":  strings.TrimSpace(connID),
			"active":   strconv.FormatBool(active),
		},
	})

	c.broadcast(eventStatus, StatusEvent{
		RunID:     runID,
		Scope:     "session",
		SessionID: sessionID,
		Status:    agent.Status,
	})

	return nil
}

// ResizeSession applies a terminal geometry change requested by the connection
// holding the hand. A resize from anyone else is a silent no-op: two observers
// with different window sizes must not fight over the PTY geometry and thrash
// the agent's rendering.
func (c *Controller) ResizeSession(runID, sessionID string, columns, rows int, connID string) error {
	c.mu.RLock()
	rt := c.runtime
	c.mu.RUnlock()

	if rt == nil {
		return errors.New("supervisor runtime not ready")
	}
	if !c.HoldsHand(sessionID, connID) {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return rt.Resize(ctx, sessionID, terminal.Size{Columns: columns, Rows: rows})
}

// roleFor reports the role recorded for a connection, defaulting to operator
// for callers the registrar never saw (the desktop bridge has no connections).
func (c *Controller) roleFor(connID string) string {
	c.mu.RLock()
	entry, found := c.presence[strings.TrimSpace(connID)]
	c.mu.RUnlock()

	if !found || entry.role == "" {
		return string(RoleOperator)
	}
	return entry.role
}

func (c *Controller) StopSession(runID, sessionID string) error {
	c.mu.RLock()
	rt := c.runtime
	c.mu.RUnlock()

	if rt == nil {
		return errors.New("supervisor runtime not ready")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return rt.StopAgent(ctx, sessionID)
}

func (c *Controller) StartSession(runID, sessionID string) error {
	c.mu.RLock()
	rt := c.runtime
	c.mu.RUnlock()

	if rt == nil {
		return errors.New("supervisor runtime not ready")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return rt.StartAgent(ctx, sessionID)
}

func (c *Controller) RestartSession(runID, sessionID string) error {
	c.mu.RLock()
	rt := c.runtime
	c.mu.RUnlock()

	if rt == nil {
		return errors.New("supervisor runtime not ready")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return rt.RestartAgent(ctx, sessionID)
}

func (c *Controller) StopRun(runID string) (AppState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cancel != nil {
		c.cancel()
	}
	if c.runtime != nil {
		_ = c.runtime.Close(context.Background())
	}
	c.state.RunStatus = "stopped"
	return c.cloneStateLocked(), nil
}

// -------------------------------------------------------------
// Agent Profiles & Catalog Management
// -------------------------------------------------------------

func (c *Controller) GetAgentProfiles() (AgentProfilesView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.loadAgentProfilesLocked()
}

func (c *Controller) loadAgentProfilesLocked() (AgentProfilesView, error) {
	cfg, err := config.LoadExisting(c.configPath)
	if err != nil {
		return AgentProfilesView{}, err
	}

	token := c.getOrGenerateToken(cfg.Revision)

	profiles := make([]AgentProfile, 0, len(cfg.Agents))
	for _, spec := range cfg.Agents {
		profiles = append(profiles, profileFromSpec(spec))
	}

	return AgentProfilesView{
		ConfigPath:      c.configPath,
		Revision:        token,
		Catalog:         c.catalogViewLocked(),
		Profiles:        profiles,
		MinProfiles:     minAgentProfiles,
		MaxProfiles:     maxAgentProfiles,
		RestartRequired: c.plan == nil || cfg.Revision != c.revisionHash,
		Editable:        !cfg.Legacy,
	}, nil
}

func (c *Controller) getOrGenerateToken(revisionHash string) string {
	if c.revisionHash != revisionHash || c.revisionToken == "" {
		tokenBytes := make([]byte, 16)
		_, _ = rand.Read(tokenBytes)
		c.revisionToken = hex.EncodeToString(tokenBytes)
		c.revisionHash = revisionHash
	}
	return c.revisionToken
}

func (c *Controller) catalogViewLocked() []AgentCatalogEntry {
	descriptors := toolcatalog.Descriptors()
	result := make([]AgentCatalogEntry, 0, len(descriptors))
	adapterStatuses := make(map[string]string)

	if registry, err := adapters.NewRegistry(adapters.DefaultPatterns()); err == nil {
		for _, desc := range registry.Descriptors() {
			if desc.Implemented {
				adapterStatuses[desc.ID] = string(desc.Status)
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	for _, d := range descriptors {
		detection, err := toolcatalog.Detect(ctx, d.ID, "", c.detector)
		status := toolcatalog.InstallUnknown
		if err == nil {
			status = detection.Status
		}
		defaultArgv := []string{}
		if len(d.Executables) > 0 {
			defaultArgv = append(defaultArgv, d.Executables[0])
			defaultArgv = append(defaultArgv, d.ArgumentPrefix...)
		}
		adStatus := adapterStatuses[d.DefaultAdapter]
		if adStatus == "" {
			adStatus = string(adapters.StatusExperimental)
		}
		result = append(result, AgentCatalogEntry{
			ID:                 string(d.ID),
			Name:               d.Name,
			Description:        profileDescription(d.ID),
			InstallStatus:      string(status),
			Installed:          status == toolcatalog.InstallInstalled,
			Adapter:            d.DefaultAdapter,
			AdapterStatus:      adStatus,
			DefaultArgv:        defaultArgv,
			RequiresCustomArgv: d.RequiresExecutable,
			MinimumArguments:   d.MinimumArguments,
			ArgumentPrefix:     append([]string{}, d.ArgumentPrefix...),
		})
	}
	return result
}

func profileDescription(id toolcatalog.ProfileID) string {
	switch id {
	case toolcatalog.Aider:
		return "Aider coding assistant; interactive pair programming with terminal prompts."
	case toolcatalog.GooseCLI:
		return "Goose CLI; developer AI agent with automated tool and command execution prompts."
	case toolcatalog.OpenInterpreter:
		return "Open Interpreter; local code and command execution with human approval prompts."
	case toolcatalog.ClaudeCode:
		return "Claude Code; experimental rules verified on 2.1.59, then generic fallback."
	case toolcatalog.CodexCLI:
		return "Codex CLI; experimental rules verified on 0.148.0-alpha.21, then generic fallback."
	case toolcatalog.MimoCode:
		return "MiMo Code launch profile; local command and generic detection."
	case toolcatalog.Ollama:
		return "Local Ollama / DeepSeek; the run subcommand and the model stay explicit arguments."
	default:
		return "Any local interactive CLI with an explicit argv."
	}
}

func (c *Controller) SaveAgentProfiles(runID string, req SaveAgentProfilesRequest) (AgentProfilesView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	specs, err := c.validateAndBuildSpecsLocked(req.Profiles)
	if err != nil {
		return AgentProfilesView{}, err
	}

	cfg, err := config.LoadExisting(c.configPath)
	if err != nil {
		return AgentProfilesView{}, err
	}

	if req.ExpectedRevision != c.revisionToken {
		return AgentProfilesView{}, errStaleRevision
	}

	_, newRev, err := config.ReplaceAgents(c.configPath, cfg.Revision, specs)
	if err != nil {
		return AgentProfilesView{}, err
	}

	c.revisionHash = newRev
	tokenBytes := make([]byte, 16)
	_, _ = rand.Read(tokenBytes)
	c.revisionToken = hex.EncodeToString(tokenBytes)

	return c.loadAgentProfilesLocked()
}

func (c *Controller) SaveAgentProfilesAndRestart(req SaveAgentProfilesAndRestartRequest) (LifecycleResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	specs, err := c.validateAndBuildSpecsLocked(req.Profiles)
	if err != nil {
		return LifecycleResult{}, err
	}

	cfg, err := config.LoadExisting(c.configPath)
	if err != nil {
		return LifecycleResult{}, err
	}

	if req.ExpectedRevision != c.revisionToken {
		return LifecycleResult{}, errStaleRevision
	}

	_, newRev, err := config.ReplaceAgents(c.configPath, cfg.Revision, specs)
	if err != nil {
		return LifecycleResult{}, err
	}

	c.revisionHash = newRev
	tokenBytes := make([]byte, 16)
	_, _ = rand.Read(tokenBytes)
	c.revisionToken = hex.EncodeToString(tokenBytes)

	// Stop existing runtime
	if c.cancel != nil {
		c.cancel()
	}
	if c.runtime != nil {
		_ = c.runtime.Close(context.Background())
	}

	// Restart runtime with new plan
	if err := c.startLocked(context.Background()); err != nil {
		return LifecycleResult{}, fmt.Errorf("restarting supervisor: %w", err)
	}

	profilesView, _ := c.loadAgentProfilesLocked()
	return LifecycleResult{
		Outcome:  "restarted",
		State:    c.cloneStateLocked(),
		Profiles: profilesView,
	}, nil
}

func (c *Controller) validateAndBuildSpecsLocked(inputs []AgentProfileInput) ([]agent.Spec, error) {
	if len(inputs) < minAgentProfiles || len(inputs) > maxAgentProfiles {
		return nil, fmt.Errorf("agent count must be between %d and %d", minAgentProfiles, maxAgentProfiles)
	}

	seenIDs := make(map[string]bool)
	specs := make([]agent.Spec, 0, len(inputs))

	for _, p := range inputs {
		id := strings.TrimSpace(p.ID)
		if !profileIDRegex.MatchString(id) {
			return nil, fmt.Errorf("invalid agent ID %q: must match %s", id, profileIDRegex.String())
		}
		if seenIDs[id] {
			return nil, fmt.Errorf("duplicate agent ID %q", id)
		}
		seenIDs[id] = true

		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = id
		}

		argv := p.Argv
		if len(argv) == 0 {
			// Preset fallback
			if p.PresetID != "" {
				desc, ok := toolcatalog.Lookup(toolcatalog.ProfileID(p.PresetID))
				if ok && len(desc.Executables) > 0 {
					argv = append([]string{desc.Executables[0]}, desc.ArgumentPrefix...)
				}
			}
		}
		if len(argv) == 0 {
			argv = []string{id}
		}

		backend := strings.TrimSpace(p.Backend)
		if backend == "" {
			backend = "auto"
		}

		adapter := strings.TrimSpace(p.Adapter)
		if adapter == "auto" {
			adapter = ""
		}
		if adapter == "" && p.PresetID != "" {
			desc, ok := toolcatalog.Lookup(toolcatalog.ProfileID(p.PresetID))
			if ok && desc.DefaultAdapter != "" {
				adapter = desc.DefaultAdapter
			}
		}

		specs = append(specs, agent.Spec{
			ID:      id,
			Name:    name,
			Command: argv,
			Cwd:     p.Cwd,
			Backend: backend,
			Adapter: adapter,
		})
	}

	return specs, nil
}

// -------------------------------------------------------------
// Settings Management
// -------------------------------------------------------------

func (c *Controller) GetFullSettings() (FullSettingsView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	profilesView, err := c.loadAgentProfilesLocked()
	if err != nil {
		return FullSettingsView{}, err
	}

	cfg, err := config.LoadExisting(c.configPath)
	if err != nil {
		return FullSettingsView{}, err
	}

	return FullSettingsView{
		AgentProfilesView: profilesView,
		Security:          extractSecuritySettings(cfg),
		Notifications:     extractNotificationSettings(cfg.Notifications),
	}, nil
}

func (c *Controller) SaveFullSettings(runID string, req SaveFullSettingsRequest) (FullSettingsView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cfg, err := config.LoadExisting(c.configPath)
	if err != nil {
		return FullSettingsView{}, err
	}

	if req.ExpectedRevision != "" && req.ExpectedRevision != c.revisionToken {
		return FullSettingsView{}, errStaleRevision
	}

	var update config.FullConfigurationUpdate

	if len(req.Profiles) > 0 {
		specs, err := c.validateAndBuildSpecsLocked(req.Profiles)
		if err != nil {
			return FullSettingsView{}, err
		}
		update.Agents = specs
		update.UpdateAgents = true
	}

	if req.Notifications != nil {
		update.Notifications = convertNotificationSettings(req.Notifications)
	}

	if req.Security != nil {
		policies := cfg.Policies
		if req.Security.DefaultAction != "" {
			policies.DefaultAction = policy.Action(strings.ToLower(req.Security.DefaultAction))
		}
		policies.DryRun = req.Security.DryRun
		if req.Security.RateLimitPerMinute > 0 {
			policies.RateLimitPerMinute = req.Security.RateLimitPerMinute
		}
		if req.Security.MaxConsecutiveAutoDecisions > 0 {
			policies.MaxConsecutiveAutoDecisions = req.Security.MaxConsecutiveAutoDecisions
		}
		update.Policies = &policies
	}

	res, newRev, err := config.UpdateFullConfiguration(c.configPath, cfg.Revision, update)
	if err != nil {
		return FullSettingsView{}, err
	}

	c.revisionHash = newRev
	tokenBytes := make([]byte, 16)
	_, _ = rand.Read(tokenBytes)
	c.revisionToken = hex.EncodeToString(tokenBytes)

	// Live reload notifier if notifications were updated
	if update.Notifications != nil {
		c.notifier = notify.New(res.Notifications, c.diagnostics)
	}

	profilesView, err := c.loadAgentProfilesLocked()
	if err != nil {
		return FullSettingsView{}, err
	}

	return FullSettingsView{
		AgentProfilesView: profilesView,
		Security:          extractSecuritySettings(res),
		Notifications:     extractNotificationSettings(res.Notifications),
	}, nil
}

func (c *Controller) TestNotification() error {
	c.mu.RLock()
	notifier := c.notifier
	c.mu.RUnlock()

	notif := notify.Notification{
		Title:     "Relayer Test Notification",
		Body:      "This is a test notification from Relayer Supervisor to verify alert channels.",
		AgentName: "supervisor",
		EventID:   fmt.Sprintf("test-%d", time.Now().UnixNano()),
		Kind:      notify.KindSessionState,
		Severity:  notify.SeverityInfo,
		Reason:    "Operator initiated test",
		Timestamp: time.Now().UTC(),
	}

	if notifier != nil {
		notifier.Notify(notif)
	}

	c.broadcast(eventNotification, NotificationEvent{
		Title:     notif.Title,
		Body:      notif.Body,
		AgentName: notif.AgentName,
		EventID:   notif.EventID,
		Kind:      notif.Kind,
		Severity:  notif.Severity,
		Reason:    notif.Reason,
		Timestamp: notif.Timestamp.Format(time.RFC3339),
	})

	return nil
}

func convertNotificationSettings(ns *NotificationSettings) *notify.Config {
	if ns == nil {
		return nil
	}
	webhooks := make([]notify.WebhookConfig, 0, len(ns.Webhooks))
	for _, w := range ns.Webhooks {
		webhooks = append(webhooks, notify.WebhookConfig{
			Name:        w.Name,
			URL:         w.URL,
			Format:      w.Format,
			MinSeverity: w.MinSeverity,
			Timeout:     w.Timeout,
		})
	}
	return &notify.Config{
		Enabled:     ns.Enabled,
		Bell:        ns.Bell,
		Desktop:     ns.Desktop,
		MinSeverity: ns.MinSeverity,
		Webhooks:    webhooks,
	}
}

// -------------------------------------------------------------
// Audit & Telemetry
// -------------------------------------------------------------

func (c *Controller) GetAuditSummary() (AuditSummaryView, error) {
	c.mu.RLock()
	auditPath := c.state.Audit.Path
	c.mu.RUnlock()

	if auditPath == "" {
		var err error
		auditPath, err = audit.DefaultPath()
		if err != nil {
			return AuditSummaryView{}, err
		}
	}

	file, err := os.Open(auditPath)
	if err != nil {
		if os.IsNotExist(err) {
			return AuditSummaryView{Path: auditPath}, nil
		}
		return AuditSummaryView{}, err
	}
	defer file.Close()

	summary, err := audit.SummarizeJournal(file)
	if err != nil {
		return AuditSummaryView{}, err
	}

	decisions := make(map[string]int)
	for k, v := range summary.DecisionsCount {
		decisions[string(k)] = v
	}
	actors := make(map[string]int)
	for k, v := range summary.ActorsCount {
		actors[string(k)] = v
	}
	outcomes := make(map[string]int)
	for k, v := range summary.OutcomesCount {
		outcomes[string(k)] = v
	}

	var firstTs, lastTs string
	if !summary.FirstTimestamp.IsZero() {
		firstTs = summary.FirstTimestamp.Format(time.RFC3339)
	}
	if !summary.LastTimestamp.IsZero() {
		lastTs = summary.LastTimestamp.Format(time.RFC3339)
	}

	kinds := make(map[string]int, len(summary.KindCounts))
	for k, v := range summary.KindCounts {
		kinds[string(k)] = v
	}

	return AuditSummaryView{
		Path:           auditPath,
		TotalEntries:   summary.TotalEntries,
		RunsCount:      summary.RunsCount,
		SessionsCount:  summary.SessionsCount,
		AgentCounts:    summary.AgentCounts,
		KindCounts:     kinds,
		DecisionsCount: decisions,
		ActorsCount:    actors,
		OutcomesCount:  outcomes,
		SensitiveCount: summary.SensitiveCount,
		FirstTimestamp: firstTs,
		LastTimestamp:  lastTs,
	}, nil
}

func (c *Controller) GetAuditEntries(filter AuditFilterInput) ([]AuditEntryView, error) {
	c.mu.RLock()
	auditPath := c.state.Audit.Path
	c.mu.RUnlock()

	if auditPath == "" {
		var err error
		auditPath, err = audit.DefaultPath()
		if err != nil {
			return nil, err
		}
	}

	file, err := os.Open(auditPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []AuditEntryView{}, nil
		}
		return nil, err
	}
	defer file.Close()

	entries, err := audit.ReadEntries(file, audit.Filter{
		AgentID:   filter.AgentID,
		SessionID: filter.SessionID,
		RunID:     filter.RunID,
		Kind:      audit.Kind(filter.Kind),
		Limit:     filter.Limit,
	})
	if err != nil {
		return nil, err
	}

	views := make([]AuditEntryView, 0, len(entries))
	for _, e := range entries {
		views = append(views, AuditEntryView{
			Sequence:   e.Sequence,
			Timestamp:  e.Timestamp.Format(time.RFC3339),
			EntryID:    e.EntryID,
			RunID:      e.RunID,
			Kind:       string(e.Kind),
			SessionID:  e.SessionID,
			AgentID:    e.AgentID,
			Backend:    e.Backend,
			Adapter:    e.Adapter,
			EventType:  string(e.EventType),
			Risk:       string(e.Risk),
			Rule:       e.Rule,
			Decision:   string(e.Decision),
			DecisionBy: string(e.DecisionBy),
			Operator:   e.Operator,
			Outcome:    string(e.Outcome),
			Reason:     e.Reason,
			Summary:    e.Summary,
			Sensitive:  e.Sensitive,
			Metadata:   e.Metadata,
		})
	}
	return views, nil
}

func (c *Controller) VerifyAuditJournal() (AuditVerificationView, error) {
	c.mu.RLock()
	auditPath := c.state.Audit.Path
	c.mu.RUnlock()

	if auditPath == "" {
		var err error
		auditPath, err = audit.DefaultPath()
		if err != nil {
			return AuditVerificationView{}, err
		}
	}

	file, err := os.Open(auditPath)
	if err != nil {
		if os.IsNotExist(err) {
			return AuditVerificationView{Path: auditPath, Passed: true}, nil
		}
		return AuditVerificationView{}, err
	}
	defer file.Close()

	report, err := audit.VerifyJournal(file)
	if err != nil {
		return AuditVerificationView{}, err
	}

	issues := make([]AuditVerificationIssueView, 0, len(report.Issues))
	for _, issue := range report.Issues {
		issues = append(issues, AuditVerificationIssueView{
			Line:    issue.Line,
			EntryID: issue.EntryID,
			Message: issue.Message,
		})
	}

	return AuditVerificationView{
		Path:       auditPath,
		TotalLines: report.TotalLines,
		TotalRuns:  report.TotalRuns,
		ValidLines: report.ValidLines,
		Issues:     issues,
		Passed:     report.Passed,
	}, nil
}

func (c *Controller) ExportAuditReport(format string) (string, error) {
	entries, err := c.GetAuditEntries(AuditFilterInput{Limit: 0})
	if err != nil {
		return "", err
	}

	if format == "csv" {
		var sb strings.Builder
		// encoding/csv rather than a format string: an operator identity is
		// free-form enough to contain a comma or a quote, and a hand-built row
		// would silently shift every column after it.
		writer := csv.NewWriter(&sb)
		if err := writer.Write([]string{
			"sequence", "timestamp", "runID", "sessionID", "agentID", "kind",
			"eventType", "decision", "decisionBy", "operator", "outcome", "reason",
		}); err != nil {
			return "", err
		}
		for _, e := range entries {
			if err := writer.Write([]string{
				strconv.FormatUint(e.Sequence, 10), e.Timestamp, e.RunID, e.SessionID,
				e.AgentID, e.Kind, e.EventType, e.Decision, e.DecisionBy, e.Operator,
				e.Outcome, e.Reason,
			}); err != nil {
				return "", err
			}
		}
		writer.Flush()
		if err := writer.Error(); err != nil {
			return "", err
		}
		return sb.String(), nil
	}

	// Real JSON. This used to be fmt.Sprintf("%v", entries), which renders Go
	// struct syntax: the interface offered a .json download that no JSON parser
	// could read.
	encoded, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func (c *Controller) GetTelemetrySnapshot() TelemetrySnapshotView {
	c.mu.RLock()
	rt := c.runtime
	c.mu.RUnlock()

	emptyView := TelemetrySnapshotView{
		Timestamp:           time.Now().UTC().Format(time.RFC3339),
		GuardrailsBreakdown: make(map[string]int64),
		DecisionDurations:   buildDefaultLatencyBuckets(),
	}

	if rt == nil {
		return emptyView
	}

	metadata := rt.Metadata()
	raw := rt.TelemetrySnapshot()

	view := TelemetrySnapshotView{
		Timestamp:            raw.Timestamp.Format(time.RFC3339),
		Enabled:              metadata.TelemetryEnabled,
		PrometheusEnabled:    metadata.TelemetryProm != "",
		PrometheusAddress:    metadata.TelemetryProm,
		SessionsActive:       sumMetricSampleValues(raw.SessionsActive),
		EventsPending:        raw.EventsPending,
		SessionsTotal:        sumMetricSampleValues(raw.SessionsTotal),
		EventsDetectedTotal:  sumMetricSampleValues(raw.EventsDetectedTotal),
		EventsWithdrawnTotal: sumMetricSampleValues(raw.EventsWithdrawnTotal),
		DecisionsTotal:       sumMetricSampleValues(raw.DecisionsTotal),
		OperatorInputsTotal:  sumMetricSampleValues(raw.OperatorInputsTotal),
		GuardrailsBreakdown:  make(map[string]int64),
	}

	// 1. Decisions breakdown
	for _, sample := range raw.DecisionsTotal {
		dec := strings.ToLower(sample.Labels["decision"])
		by := strings.ToLower(sample.Labels["decision_by"])
		val := int64(sample.Value)

		switch {
		case by == "policy" || by == "auto":
			if dec == "allow" {
				view.DecisionsBreakdown.AutoAllow += val
			} else {
				view.DecisionsBreakdown.AutoDeny += val
			}
		default: // human
			switch dec {
			case "allow":
				view.DecisionsBreakdown.Allow += val
			case "deny", "abort":
				view.DecisionsBreakdown.Deny += val
			default:
				view.DecisionsBreakdown.Custom += val
			}
		}
	}

	// 2. Guardrails violations breakdown
	for _, sample := range raw.GuardrailsViolations {
		rule := sample.Labels["rule"]
		if rule == "" {
			rule = "general"
		}
		val := int64(sample.Value)
		view.GuardrailsBreakdown[rule] += val
		view.GuardrailsTotal += val
	}

	// 3. Latency duration averages and histogram
	view.DecisionDurations, view.AverageReactionTime = computeLatencyDistribution(raw.DecisionDurations)

	return view
}

func sumMetricSampleValues(samples []telemetry.MetricSample) int64 {
	var total int64
	for _, s := range samples {
		total += int64(s.Value)
	}
	return total
}

func buildDefaultLatencyBuckets() []LatencyBucketView {
	return []LatencyBucketView{
		{Le: 0.5, Label: "< 0.5s", Count: 0},
		{Le: 1.0, Label: "0.5s - 1s", Count: 0},
		{Le: 2.0, Label: "1s - 2s", Count: 0},
		{Le: 5.0, Label: "2s - 5s", Count: 0},
		{Le: 10.0, Label: "5s - 10s", Count: 0},
		{Le: 30.0, Label: "10s - 30s", Count: 0},
		{Le: 60.0, Label: "30s - 60s", Count: 0},
		{Le: 300.0, Label: "> 60s", Count: 0},
	}
}

func computeLatencyDistribution(samples []telemetry.HistogramSample) ([]LatencyBucketView, float64) {
	thresholds := []struct {
		le    float64
		label string
	}{
		{0.5, "< 0.5s"},
		{1.0, "0.5s - 1s"},
		{2.0, "1s - 2s"},
		{5.0, "2s - 5s"},
		{10.0, "5s - 10s"},
		{30.0, "10s - 30s"},
		{60.0, "30s - 60s"},
		{300.0, "> 60s"},
	}

	cumulative := make(map[float64]uint64)
	var totalSum float64
	var totalCount uint64

	for _, sample := range samples {
		totalSum += sample.Sum
		totalCount += sample.Count
		for le, count := range sample.Buckets {
			cumulative[le] += count
		}
	}

	var avg float64
	if totalCount > 0 {
		avg = totalSum / float64(totalCount)
	}

	buckets := make([]LatencyBucketView, len(thresholds))
	var prevCount uint64
	for i, t := range thresholds {
		cumCount := cumulative[t.le]
		var discrete uint64
		if cumCount >= prevCount {
			discrete = cumCount - prevCount
		}
		buckets[i] = LatencyBucketView{
			Le:    t.le,
			Label: t.label,
			Count: discrete,
		}
		prevCount = cumCount
	}

	sort.Slice(buckets, func(i, j int) bool {
		return buckets[i].Le < buckets[j].Le
	})

	return buckets, avg
}

// -------------------------------------------------------------
// Helper projections
// -------------------------------------------------------------

func profileFromSpec(spec agent.Spec) AgentProfile {
	presetID := ""
	for _, desc := range toolcatalog.Descriptors() {
		for _, exe := range desc.Executables {
			if len(spec.Command) > 0 && (spec.Command[0] == exe || filepath.Base(spec.Command[0]) == exe) {
				presetID = string(desc.ID)
				break
			}
		}
		if presetID != "" {
			break
		}
	}
	label := ""
	if len(spec.Command) > 0 {
		label = filepath.Base(spec.Command[0])
	}
	return AgentProfile{
		ID:              spec.ID,
		Name:            spec.Name,
		PresetID:        presetID,
		Cwd:             spec.Cwd,
		Backend:         spec.Backend,
		Adapter:         spec.Adapter,
		Argv:            spec.Command,
		ExecutableLabel: label,
		ArgumentCount:   len(spec.Command),
		Locked:          false,
	}
}

func extractSecuritySettings(cfg config.Result) SecuritySettings {
	return SecuritySettings{
		Profile:                     "default",
		DefaultAction:               string(cfg.Policies.DefaultAction),
		DryRun:                      cfg.Policies.DryRun,
		BlockDestructive:            true,
		BlockExfiltration:           true,
		BlockSensitivePaths:         true,
		BlockOutsideWorkspace:       false,
		RateLimitPerMinute:          60,
		MaxConsecutiveAutoDecisions: 5,
	}
}

func extractNotificationSettings(cfg notify.Config) NotificationSettings {
	hooks := make([]NotificationWebhookSetting, 0, len(cfg.Webhooks))
	for _, w := range cfg.Webhooks {
		hooks = append(hooks, NotificationWebhookSetting{
			Name:        w.Name,
			URL:         w.URL,
			Format:      w.Format,
			MinSeverity: string(w.MinSeverity),
			Timeout:     w.Timeout,
		})
	}
	return NotificationSettings{
		Enabled:     cfg.Enabled,
		Bell:        cfg.Bell,
		Desktop:     cfg.Desktop,
		MinSeverity: string(cfg.MinSeverity),
		Webhooks:    hooks,
	}
}

func projectPreflightReport(report preflight.Report) PreflightReport {
	tools := make([]PreflightTool, 0, len(report.Tools))
	for _, t := range report.Tools {
		tools = append(tools, PreflightTool{
			ProfileID:    string(t.ProfileID),
			Installation: string(t.Installation),
		})
	}

	agents := make([]PreflightAgent, 0, len(report.Agents))
	for _, a := range report.Agents {
		agents = append(agents, PreflightAgent{
			Ordinal:         a.Ordinal,
			Source:          string(a.Source),
			Command:         string(a.Command),
			Installation:    string(a.Installation),
			Adapter:         a.Adapter,
			AdapterMaturity: string(a.AdapterMaturity),
			Backend:         a.Backend,
		})
	}

	checks := make([]PreflightCheck, 0, len(report.Checks))
	for _, c := range report.Checks {
		checks = append(checks, PreflightCheck{
			ID:          c.ID,
			Scope:       string(c.Scope),
			Status:      string(c.Status),
			Summary:     c.Summary,
			Remediation: c.Remediation,
		})
	}

	return PreflightReport{
		SchemaVersion: report.SchemaVersion,
		Status:        string(report.Status),
		Platform: PreflightPlatform{
			OS:        report.Platform.OS,
			Arch:      report.Platform.Arch,
			Supported: report.Platform.Supported,
		},
		Configuration: PreflightConfiguration{
			Version:         report.Configuration.Version,
			Legacy:          report.Configuration.Legacy,
			AgentCount:      report.Configuration.AgentCount,
			PolicyRuleCount: report.Configuration.PolicyRuleCount,
		},
		Audit: PreflightAudit{
			Enabled:       report.Audit.Enabled,
			Mode:          string(report.Audit.Mode),
			Location:      string(report.Audit.Location),
			MaxFileSizeMB: report.Audit.MaxFileSizeMB,
			MaxFiles:      report.Audit.MaxFiles,
		},
		Tools:  tools,
		Agents: agents,
		Checks: checks,
	}
}

// toolCallView converts a detected MCP tool call into its display DTO.
//
// A sensitive occurrence never carries one: the detector refuses to parse a
// credential prompt's surroundings, so nil here is the normal case and not a
// failure to report.
func toolCallView(call *adapters.ToolCall) *ToolCallView {
	if call == nil {
		return nil
	}

	view := &ToolCallView{
		Server:          call.Server,
		Tool:            call.Tool,
		Risk:            string(call.Risk),
		ParamsTruncated: call.ParamsTruncated,
	}
	if len(call.Params) > 0 {
		view.Params = make([]ToolCallParamView, 0, len(call.Params))
		for _, param := range call.Params {
			view.Params = append(view.Params, ToolCallParamView{
				Name:      param.Name,
				Value:     param.Value,
				Truncated: param.Truncated,
			})
		}
	}
	return view
}
