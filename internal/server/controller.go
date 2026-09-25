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
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/agentprofile"
	"github.com/Hocsman/Relayer/internal/app"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/notify"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/preflight"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
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
)

var (
	errStaleRevision  = errors.New("configuration has changed, reload before saving")
	errStaleRun       = errors.New("run has changed, reload before retrying")
	errUnknownSession = errors.New("unknown session")
	// errLifecycleBlocked refuses to start or stop a run once a run's agents
	// could not be confirmed stopped: another run started beside them would
	// leave processes nothing supervises.
	errLifecycleBlocked = errors.New("the previous run's agents could not be confirmed stopped; restart Relayer before starting another run")
)

// Controller owns the headless supervisor runtime and provides thread-safe
// query and mutation methods mirroring the RelayerBridge interface.
//
// c.mu guards the gateway's own state: the run, its agents' output and
// presence, the hands and the subscribers. What decides whether a byte reaches
// an agent — its prompts, the policy's decisions, the agents' supervision
// state and the journal's failure — is the run's supervision core's, which
// GetState lays over the gateway's own. The core's sink takes c.mu (see
// gatewaySink): c.mu is held while reading the core or calling SetHolder,
// RecordAudit and BeginDrain, never while calling an operation that changes
// the core, or Wait.
//
// lifecycleMu serialises what ends a run, StopRun, "Save and restart" and
// Close, and is taken before c.mu, never after it. A run ends by draining,
// which waits for what its core admitted and so never happens under c.mu.
type Controller struct {
	lifecycleMu sync.Mutex
	mu          sync.RWMutex
	configPath  string
	diagnostics io.Writer

	ctx    context.Context
	cancel context.CancelFunc
	// loopDone is closed once the run's event loop has returned.
	loopDone chan struct{}

	plan    *app.DesktopPlan
	runtime *app.DesktopRuntime
	runID   string
	// sup is the run's supervision core, replaced with its runtime: nothing of
	// a run's prompts, answers or freezes outlives it. It is nil once a run
	// stopped, or failed to start, until another run starts.
	sup *supervise.Supervisor
	// ending is set from the moment a run begins to end until another run
	// starts: no terminal is handed over meanwhile, since the hands go with
	// the run.
	ending bool
	// lifecycleBlocked is set once a run's processes could not be confirmed
	// stopped: no other run is started beside processes that may still run.
	lifecycleBlocked bool
	// engineWrap, when a test sets it before Start, wraps each run's runtime
	// where the supervision core meets it: the gateway tests' seam for a
	// journal, a write or a stop that fails.
	engineWrap func(supervise.Engine) supervise.Engine
	// drainBudget, when a test sets it, replaces runEndBudget for the phase
	// of a run's end that waits for what its core admitted.
	drainBudget time.Duration

	state       AppState
	agentIndex  map[string]int // lowercase sessionID -> slice index
	subscribers map[uint64]func(event string, payload any)
	nextSubID   uint64

	revisionHash         string
	revisionToken        string
	activeConfigRevision string

	presence           map[string]presenceEntry // connID -> live connection
	hands              map[string]handState     // lowercase sessionID -> write lock
	requestTimeout     time.Duration
	allowForceTakeover bool

	detector           toolcatalog.Detector
	notifier           notify.Notifier
	notificationConfig notify.Config
	stopped            int32
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
		configPath:         configPath,
		diagnostics:        diagnostics,
		agentIndex:         make(map[string]int),
		subscribers:        make(map[uint64]func(event string, payload any)),
		detector:           toolcatalog.DefaultDetector(),
		notificationConfig: notify.DefaultConfig(),
		notifier:           notify.New(notify.DefaultConfig(), diagnostics),
	}, nil
}

// Start boots the supervisor runtime and begins event processing.
//
// The run's context keeps ctx's values but not its cancellation: a run ends
// by StopRun, "Save and restart" or Close, which drain it in order, and never
// because the caller's context ended. Serve passes the context its signals
// cancel, and an interrupt cancelled the run before Close drained it: an
// answer being written was cut off and recorded as uncertain, and the exits of
// the agents Close then stopped were never journaled. A run started by "Save
// and restart" already had a context of its own.
func (c *Controller) Start(ctx context.Context) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	runID, err := newRunID()
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.startLocked(context.WithoutCancel(ctx), runID)
}

// newRunID draws the ID of a run about to start.
func newRunID() (string, error) {
	runIDBytes := make([]byte, 8)
	if _, err := rand.Read(runIDBytes); err != nil {
		return "", fmt.Errorf("generating run id: %w", err)
	}
	return hex.EncodeToString(runIDBytes), nil
}

// startLocked starts the run runID, with a supervision core of its own and no
// hand held: the caller holds c.mu, and has ended any previous run.
func (c *Controller) startLocked(ctx context.Context, runID string) error {
	opts := app.DesktopOptions{
		ConfigPath:  c.configPath,
		Diagnostics: c.diagnostics,
	}

	plan, err := app.PrepareDesktopRuntime(opts)
	if err != nil {
		return fmt.Errorf("preparing desktop runtime: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	rt, err := app.StartDesktopRuntime(runCtx, plan, runID)
	if err != nil {
		cancel()
		return fmt.Errorf("starting desktop runtime: %w", err)
	}

	sessions := rt.Sessions()
	specs := make([]supervise.AgentSpec, 0, len(sessions))
	for _, s := range sessions {
		specs = append(specs, supervise.AgentSpec{
			SessionID: s.ID,
			AgentID:   s.ID,
			Name:      s.Name,
			Backend:   s.Backend,
			Adapter:   s.Adapter,
		})
	}
	// Each run gets a supervision core of its own, over its own runtime.
	var engine supervise.Engine = rt
	if c.engineWrap != nil {
		engine = c.engineWrap(engine)
	}
	sink := &gatewaySink{c: c, rt: rt}
	sup, err := supervise.New(runCtx, engine, supervise.Options{RunID: runID, Agents: specs, Sink: sink})
	if err != nil {
		cancel()
		_ = rt.Close(context.Background())
		return fmt.Errorf("starting the supervision core: %w", err)
	}
	sink.sup = sup

	c.ctx = runCtx
	c.cancel = cancel
	c.plan = plan
	c.runtime = rt
	c.runID = runID
	c.sup = sup
	c.ending = false
	c.loopDone = make(chan struct{})

	metadata := rt.Metadata()
	c.activeConfigRevision = metadata.ConfigRevision
	c.notificationConfig = metadata.Notifications
	c.notifier = notify.NewWithDiagnostics(metadata.Notifications, c.diagnostics, c.diagnostics)

	agents := make([]AgentState, 0, len(sessions))
	c.agentIndex = make(map[string]int, len(sessions))

	for i, s := range sessions {
		out, _ := rt.Output(s.ID)
		c.agentIndex[strings.ToLower(s.ID)] = i
		// Status, Running and the other supervision fields are the core's,
		// which starts every agent running.
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

	go c.eventLoop(runCtx, rt, sup, c.loopDone)

	return nil
}

// Close gracefully stops the running supervisor and all agent processes. It
// drains the run first, as StopRun does, and within ctx: what the run's core
// admitted, an answer, a line or keystrokes being written, reaches its
// journaled outcome before the runtime and its journal close.
func (c *Controller) Close(ctx context.Context) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()

	if !atomic.CompareAndSwapInt32(&c.stopped, 0, 1) {
		return nil
	}
	c.mu.Lock()
	end := c.beginRunEndLocked()
	c.mu.Unlock()
	err := c.endRun(ctx, end, false)
	c.mu.Lock()
	c.clearRunLocked()
	c.state.RunStatus = "stopped"
	c.mu.Unlock()
	return err
}

// runEnd is what ending one run needs, taken under c.mu when it begins.
type runEnd struct {
	runID    string
	sup      *supervise.Supervisor
	runtime  *app.DesktopRuntime
	cancel   context.CancelFunc
	loopDone <-chan struct{}
}

// runEndBudget bounds each phase of a run's end, as the desktop's does: the
// backends' stop, the wait for what the run's core admitted, then the
// runtime's close.
const runEndBudget = 12 * time.Second

// errDrainIncomplete is a run's end whose core still had a write, or a
// decision, in progress once its budget was spent: the run is not known to
// have ended cleanly.
var errDrainIncomplete = errors.New("the run's writes did not all finish before it ended")

// waitWithin waits for wait to return, for at most budget, and reports
// whether it did. A wait that outlives its budget goes on, on its own
// goroutine, and returns whenever what it waits for does.
func waitWithin(budget time.Duration, wait func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait()
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// beginRunEndLocked closes the current run to everything new, and returns
// what ending it needs: its core admits nothing more and schedules no answer
// (BeginDrain, which never calls the sink and so is made under c.mu), and no
// terminal is handed over. The caller holds c.mu, then ends the run without it
// (endRun).
func (c *Controller) beginRunEndLocked() runEnd {
	end := runEnd{runID: c.runID, sup: c.sup, runtime: c.runtime, cancel: c.cancel, loopDone: c.loopDone}
	c.ending = true
	if c.sup != nil {
		c.sup.BeginDrain()
	}
	return end
}

// endRun ends a run beginRunEndLocked closed, never under c.mu, in the
// desktop's order. The backends stop first, strictly for a restart or a stop,
// every session explicitly, tmux ones that persist on exit included, so that
// no write stays blocked below its context; the run's context is cancelled
// and its event loop returns; the core's admitted writes and started
// decisions reach their journaled outcome (Wait), while the run's journal is
// still open; the run's hands are let go and journaled; and only then does
// the runtime close, which writes the run's last entries and closes its
// journal. The core's sink takes c.mu, and Wait waits on goroutines that call
// it, so none of this may hold c.mu. The gateway closed the runtime under c.mu
// without waiting at all: an answer being written lost its outcome with the
// journal.
//
// Each phase is bounded by runEndBudget, and by parent when it is not nil,
// the wait for the event loop and the drain included. A non-nil result means
// the run's processes may not all have stopped, or that what its core
// admitted had not all finished (errDrainIncomplete): the runtime is closed
// all the same, which ends what it can of such a write. The drain waited with
// no bound, under lifecycleMu: keystrokes blocked in the terminal of an agent
// that reads nothing, a write whose context the kernel ignores, held StopRun,
// "Save and restart" and Close for good, the run shown stopping, Serve unable
// to shut down and no run started or stopped again until the process was
// killed.
func (c *Controller) endRun(parent context.Context, end runEnd, strict bool) error {
	if parent == nil {
		parent = context.Background()
	}
	phase := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(parent, runEndBudget)
	}
	var result error
	if end.runtime != nil {
		ctx, cancel := phase()
		if strict {
			result = end.runtime.BeginRestart(ctx)
		} else {
			result = end.runtime.BeginShutdown(ctx)
		}
		cancel()
	}
	if end.cancel != nil {
		end.cancel()
	}
	drain := runEndBudget
	if c.drainBudget > 0 {
		drain = c.drainBudget
	}
	if deadline, bounded := parent.Deadline(); bounded && time.Until(deadline) < drain {
		drain = max(time.Until(deadline), 0)
	}
	if end.loopDone != nil && !waitWithin(drain, func() { <-end.loopDone }) {
		result = errors.Join(result, errDrainIncomplete)
	}
	if end.sup != nil && !waitWithin(drain, end.sup.Wait) {
		result = errors.Join(result, errDrainIncomplete)
	}
	c.releaseHandsAtRunEnd()
	if end.runtime != nil {
		ctx, cancel := phase()
		result = errors.Join(result, end.runtime.Close(ctx))
		cancel()
	}
	return result
}

// releaseHandsAtRunEnd lets go of every terminal of the run that ends, and
// journals each hand it drops as control_released, by the system: the hands
// go with the run. The run's journal is still open, and its core, drained,
// still journals what a front end gives it. Every client is told the
// terminals are free.
//
// The hands outlived a run: a restart gave the new run the previous run's,
// so a holder's connection went on typing into a process it had never
// attached to, with no attach of it in the new run's journal, and its tab
// showed a terminal of the process that was gone as its own. A run's
// terminals are its processes'; whoever wants one of the new run's takes it,
// which is journaled.
func (c *Controller) releaseHandsAtRunEnd() {
	c.mu.Lock()
	var dropped []handTransition
	touched := make(map[string]struct{}, len(c.hands))
	for key, hand := range c.hands {
		touched[key] = struct{}{}
		if !hand.held() {
			continue
		}
		idx, known := c.agentIndex[key]
		if !known || idx >= len(c.state.Agents) {
			continue
		}
		holder, live := c.presence[hand.holderConnID]
		if !live {
			holder = presenceEntry{connID: hand.holderConnID, identity: hand.holderIdentity}
		}
		dropped = append(dropped, handTransition{actor: holder, agent: c.state.Agents[idx], before: hand})
	}
	c.hands = nil
	for key := range touched {
		c.setAttachedLocked(key, false)
	}
	views := c.snapshotsLocked(touched)
	c.mu.Unlock()

	for _, change := range dropped {
		c.recordControlAudit(change, controlRecord{
			kind:    audit.KindControlReleased,
			by:      audit.DecisionBySystem,
			outcome: audit.OutcomeApplied,
			reason:  "control_released_run_end",
		})
	}
	c.broadcastSnapshots(views)
}

// clearRunLocked forgets the run that ended: its core, its runtime and its
// agents, whose supervision state was the core's, and the configuration it
// ran, so the settings say a start is owed. The run's ID stays, with its
// status, until another run starts: it is the run a client stopped, and
// "Save and restart" names it. The presence of each connection stays: it is
// the connection's, and the session IDs are the configured agents'. The
// caller holds c.mu.
func (c *Controller) clearRunLocked() {
	c.sup = nil
	c.runtime = nil
	c.cancel = nil
	c.loopDone = nil
	c.plan = nil
	c.hands = nil
	c.activeConfigRevision = ""
	c.agentIndex = make(map[string]int)
	c.state.Agents = []AgentState{}
	c.state.PendingEvents = []SupervisionEvent{}
	c.state.StartedAt = ""
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

func (c *Controller) eventLoop(ctx context.Context, rt *app.DesktopRuntime, sup *supervise.Supervisor, done chan<- struct{}) {
	defer close(done)
	events := rt.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			c.handleEvent(rt, sup, ev)
		}
	}
}

// handleEvent takes in one event of a run's session stream. Output stays with
// the gateway, which reads it again at once; everything else is the core's:
// a prompt, its withdrawal, a process exit, a lost tmux session and a backend
// stream error. The core journals each, takes the policy's decision, shows the
// prompt display-safe and notifies by its rules, through the gateway's sink,
// which also announces the finished recording of a process that ended.
func (c *Controller) handleEvent(rt *app.DesktopRuntime, sup *supervise.Supervisor, rawEvent session.Event) {
	if output, isOutput := rawEvent.(session.OutputAvailable); isOutput {
		c.refreshOutput(rt, sup, output.SessionID)
		return
	}
	sup.Handle(rawEvent)
}

// refreshOutput reads a session's bounded output again and shows it to every
// client, with the agent's supervision state as the core has it. The output is
// read under c.mu, so that of two refreshes the later revision always carries
// the later output: the core asks for one from any goroutine it writes on,
// beside the event loop's.
//
// The snapshot is broadcast once c.mu is released, outside the core's ordered
// flush, and the core's own statuses go out in that flush: a status shown
// between the snapshot's reading and its broadcast was followed by the
// snapshot's older one, which a client takes, its revision being the newest.
// A start that failed showed the agent failed, then starting again, and
// nothing corrected it: every client hid its Start button until it reloaded.
// The supervision state is therefore read again once the snapshot is out, and
// a snapshot that no longer says what the core holds is followed by one that
// does, under a newer revision (correctSnapshot). A change made after that
// reading is shown by the core, after the correction.
func (c *Controller) refreshOutput(rt *app.DesktopRuntime, sup *supervise.Supervisor, sessionID string) {
	c.mu.Lock()
	if sup == nil || c.sup != sup {
		c.mu.Unlock()
		return
	}
	idx, found := c.agentIndex[strings.ToLower(sessionID)]
	if !found || idx >= len(c.state.Agents) {
		c.mu.Unlock()
		return
	}
	out, err := rt.AnsiOutput(sessionID)
	if err != nil {
		out, err = rt.Output(sessionID)
	}
	if err != nil {
		c.mu.Unlock()
		return
	}
	agent := &c.state.Agents[idx]
	agent.Output = withoutHostReports(out)
	agent.Revision++
	snap := c.snapshotLocked(sup, idx)
	c.mu.Unlock()

	c.broadcast(eventSnapshot, snap)
	c.correctSnapshot(sup, idx, snap)
}

// maxSnapshotCorrections bounds how many times one refresh follows its
// snapshot with a newer one while the agent's supervision state keeps
// changing under it; each of those changes is shown by the core itself.
const maxSnapshotCorrections = 4

// snapshotLocked is the snapshot of the agent at index idx: its output and
// revision, with the supervision state its run's core holds now. The caller
// holds c.mu.
func (c *Controller) snapshotLocked(sup *supervise.Supervisor, idx int) SnapshotEvent {
	shown := c.state.Agents[idx]
	if supervised, known := sup.Agent(shown.SessionID); known {
		shown = withSupervision(shown, supervised)
	}
	return SnapshotEvent{
		RunID:       c.runID,
		SessionID:   shown.SessionID,
		Revision:    shown.Revision,
		Output:      shown.Output,
		Status:      shown.Status,
		Running:     shown.Running,
		Attached:    shown.Attached,
		InputFrozen: shown.InputFrozen,
		ExitCode:    cloneExitCode(shown.ExitCode),
	}
}

// correctSnapshot follows a broadcast snapshot with a newer one for as long as
// the agent's supervision state, read again, differs from what the last one
// said (refreshOutput). It stops once they agree, when the run changed, or
// after maxSnapshotCorrections.
func (c *Controller) correctSnapshot(sup *supervise.Supervisor, idx int, sent SnapshotEvent) {
	for attempt := 0; attempt < maxSnapshotCorrections; attempt++ {
		c.mu.Lock()
		if c.sup != sup || idx >= len(c.state.Agents) || c.state.Agents[idx].SessionID != sent.SessionID {
			c.mu.Unlock()
			return
		}
		now := c.snapshotLocked(sup, idx)
		if sameSupervision(now, sent) {
			c.mu.Unlock()
			return
		}
		c.state.Agents[idx].Revision++
		now.Revision = c.state.Agents[idx].Revision
		c.mu.Unlock()
		c.broadcast(eventSnapshot, now)
		sent = now
	}
}

// sameSupervision reports whether two snapshots say the same of the agent's
// supervision state.
func sameSupervision(left, right SnapshotEvent) bool {
	sameExit := (left.ExitCode == nil) == (right.ExitCode == nil) &&
		(left.ExitCode == nil || *left.ExitCode == *right.ExitCode)
	return left.Status == right.Status && left.Running == right.Running &&
		left.Attached == right.Attached && left.InputFrozen == right.InputFrozen && sameExit
}

// supervisor returns the run's supervision core and its run ID, or nil between
// runs. The core refuses a nil receiver's operations as a stopped run's, once
// it has checked their arguments.
func (c *Controller) supervisor() (*supervise.Supervisor, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sup, c.runID
}

// -------------------------------------------------------------
// RelayerBridge API Implementations
// -------------------------------------------------------------

func (c *Controller) cloneStateLocked() AppState {
	cloned := c.state
	cloned.Agents = make([]AgentState, len(c.state.Agents))
	copy(cloned.Agents, c.state.Agents)
	for index := range cloned.Agents {
		cloned.Agents[index].ExitCode = cloneExitCode(c.state.Agents[index].ExitCode)
	}
	cloned.PendingEvents = make([]SupervisionEvent, len(c.state.PendingEvents))
	copy(cloned.PendingEvents, c.state.PendingEvents)
	if c.state.Notices != nil {
		cloned.Notices = make([]string, len(c.state.Notices))
		copy(cloned.Notices, c.state.Notices)
	}
	return cloned
}

// stateLocked is a deep copy of the display state: the gateway's own fields,
// with the run's supervision state laid over them from one reading of its
// core. The caller holds c.mu.
func (c *Controller) stateLocked() AppState {
	state := c.cloneStateLocked()
	if c.sup == nil {
		return state
	}
	core := c.sup.State()
	agents := make(map[string]supervise.Agent, len(core.Agents))
	for _, agent := range core.Agents {
		agents[strings.ToLower(agent.SessionID)] = agent
	}
	for index := range state.Agents {
		if agent, found := agents[strings.ToLower(state.Agents[index].SessionID)]; found {
			state.Agents[index] = withSupervision(state.Agents[index], agent)
		}
	}
	state.PendingEvents = make([]SupervisionEvent, 0, len(core.Pending))
	for _, view := range core.Pending {
		state.PendingEvents = append(state.PendingEvents, supervisionEventFromView(view))
	}
	if core.AuditFailed {
		state.Audit.Status = "failed"
	}
	return state
}

func (c *Controller) GetState() AppState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stateLocked()
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

// SubmitDecision sends a person's typed answer to a prompt through the run's
// supervision core, which journals the decision before it writes it, keeps the
// prompt pending until the write has an outcome, writes one answer at a time
// to a session, and freezes the session when a write's outcome is unknown.
//
// The text is always sent as typed and journaled as asked: only the adapter
// knows what the bytes mean. The gateway read "y", "yes" and "allow" as the
// adapter's allow and "n", "no" and "deny" as its deny, so typing "y" into a
// Generic prompt, whose adapter encodes no allow, was refused, and any other
// text was journaled as a human allowing something. An empty answer is
// refused before anything is claimed or journaled, and so is a run the caller
// does not name: the gateway took an empty run ID for the current run, so a
// tab left open on a run that had since been replaced could answer the new
// one. actor is who typed it, from which connection; the core refuses one
// whose role may only watch, as the gateway's list of the calls a viewer may
// make already does.
func (c *Controller) SubmitDecision(runID, sessionID, eventID, value string, actor supervise.Actor) error {
	sup, _ := c.supervisor()
	return sup.SubmitDecision(runID, sessionID, eventID, value, actor)
}

// SubmitAutomaticDecision sends a chosen answer, allow or deny, which the
// adapter encodes itself. It is taken only when the prompt offers it. The
// gateway sent whatever the caller named as typed text, so an answer that was
// no choice at all reached the agent through the call meant for buttons.
func (c *Controller) SubmitAutomaticDecision(runID, sessionID, eventID, decision string, actor supervise.Actor) error {
	sup, _ := c.supervisor()
	return sup.SubmitAutomaticDecision(runID, sessionID, eventID, decision, actor)
}

// SubmitLine sends one ordinary line to a detached, running session through
// the core. The core journals the line before it writes it and fails closed,
// takes the session's one write slot, refuses the line while a prompt waits
// on the operator, while anybody holds the terminal or while the session is
// frozen, and freezes the session when the write's outcome is unknown. The
// gateway wrote the line beside whatever else was being written, with no
// check at all, journaled it afterwards with the journal's errors ignored, and
// ignored the run the caller named. The line's entries name the actor's
// identity, which is all their closed shape holds.
func (c *Controller) SubmitLine(runID, sessionID, line string, actor supervise.Actor) error {
	sup, _ := c.supervisor()
	return sup.SubmitLine(runID, sessionID, line, actor)
}

// SendTerminalInput delivers raw terminal input bytes directly to the session backend.
// Used by the web interactive terminal (full PTY mode) to stream keystrokes and signals.
//
// Keystrokes reach the agent without the policy and are never journaled: the
// documented bypass of an interactive terminal. They are bounded instead, by
// the run's supervision core, which admits a write only from the connection
// that holds the session's hand (Admit): a terminal nobody holds takes no
// keystrokes, from anybody, and neither does a call that names no connection.
// Every keystroke therefore falls within a hand whose taking was journaled.
// The write takes the session's one write slot, the one an answer and a line
// take: it is refused while either is being written, and while it is admitted
// they are refused or, for the policy's answers, wait. It is refused as well
// once the journal has failed, on a session frozen by a write whose outcome
// is unknown, on one whose process is stopped, stopping or starting, and once
// the run drains, which waits for every admitted write before its runtime
// closes.
//
// A terminal nobody held was writable by any operator connection, the binary
// frames that carry no connection's run included, and the keystrokes went to
// the terminal whatever else was being written: with the policy answering on
// its own, a client typing without the hand typed alongside the policy's
// answer to the same question, and nothing said anybody had been at the
// terminal. The resize, which writes nothing the agent reads, keeps the rule
// that a free terminal is anybody's (HoldsHand).
//
// The hand is checked when the write is admitted, and the write happens once
// it is: at most one already-in-flight keystroke may land just after a
// release. That window is documented in docs/sharing.md rather than papered
// over.
func (c *Controller) SendTerminalInput(runID, sessionID string, data []byte, operator, connID string) error {
	c.mu.RLock()
	rt, sup := c.runtime, c.sup
	currentRun := c.runID
	c.mu.RUnlock()

	if rt == nil || sup == nil {
		return errors.New("supervisor runtime not ready")
	}
	if trimmed := strings.TrimSpace(runID); trimmed != "" && trimmed != currentRun {
		return errStaleRun
	}
	// A write that is only the terminal's own replies, the focus report a
	// browser terminal sends when it takes the focus on a ConPTY session, is
	// not typing: the prompts shown stay answerable.
	admit := sup.Admit
	if supervise.IsTerminalReport(data) {
		admit = sup.AdmitReport
	}
	release, err := admit(sessionID, connID)
	if err != nil {
		if errors.Is(err, supervise.ErrNotHolder) {
			// The gateway's own refusal, which corrects a client still typing
			// into a terminal it lost.
			return ErrNotHolder
		}
		return err
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return rt.SendRaw(ctx, sessionID, data)
}

// SetInteractiveSession acquires or releases the session's write lock. It is
// the attach verb: taking the hand is what makes keystrokes routable.
//
// Taking a hand another operator holds is refused rather than silently stolen;
// the caller is expected to request control instead.
//
// Keystrokes are never journaled, so the attach record is what says who was at
// the terminal, and it is journaled first: through the run's supervision core,
// under the same lock the hand is taken under, before any client can see the
// terminal held or its holder type into it. A record the journal refuses
// leaves the terminal as it was and fails the call, and freezes the run as
// the core's own entries do; once the journal has failed, no terminal is
// taken this way. The gateway took the hand first and wrote the record
// afterwards, dropping its error, so keystrokes could reach the agent before
// any record of the attach existed, or with none at all. Releasing is
// journaled after the hand is let go, best effort, as the control records are
// (recordControlAudit): a release that could not be journaled must still
// release, and the core freezes the run all the same.
//
// A terminal is taken only for the run the caller names. The verb ignored the
// run, so a tab left open on a run that "Save and restart" had replaced
// attached to the new run's terminal, which it had never shown, and its
// keystrokes, which name no run, went to the new process: a run's hands go
// with it precisely so that such a tab holds nothing in the next one. Letting
// go of a terminal needs no run, since it is always safe.
func (c *Controller) SetInteractiveSession(runID, sessionID string, active bool, operator, connID string) error {
	c.mu.RLock()
	rt := c.runtime
	currentRun := c.runID
	idx, hasAgent := c.agentIndex[strings.ToLower(strings.TrimSpace(sessionID))]
	var agent AgentState
	if hasAgent && idx < len(c.state.Agents) {
		agent = c.state.Agents[idx]
	}
	c.mu.RUnlock()

	if rt == nil {
		return errors.New("supervisor runtime not ready")
	}
	if active && (strings.TrimSpace(runID) == "" || runID != currentRun) {
		return supervise.ErrRunStale
	}
	if !hasAgent {
		return errUnknownSession
	}

	if strings.TrimSpace(operator) == "" {
		operator = "operator"
	}
	attachEntry := func(change handTransition, active bool) audit.Entry {
		kind, outcome := audit.KindAttachStarted, "attached"
		if !active {
			kind, outcome = audit.KindAttachFinished, "detached"
		}
		role := change.actor.role
		if role == "" {
			role = string(RoleOperator)
		}
		return audit.Entry{
			Kind:       kind,
			SessionID:  change.agent.SessionID,
			AgentID:    strings.TrimSpace(change.agent.AgentID),
			Backend:    strings.ToLower(strings.TrimSpace(change.agent.Backend)),
			Adapter:    strings.ToLower(strings.TrimSpace(change.agent.Adapter)),
			DecisionBy: audit.DecisionByHuman,
			Operator:   operator,
			Outcome:    audit.OutcomeApplied,
			Reason:     "operator_interactive_" + outcome,
			Metadata: map[string]string{
				"operator": operator,
				"role":     role,
				"conn_id":  change.actor.connID,
				"active":   strconv.FormatBool(active),
			},
		}
	}

	// The unjournaled forms of the hand's verbs: this verb writes its own
	// attach record, and one action must not appear in the journal twice.
	if active {
		_, _, err := c.mutateHandRecorded(sessionID, connID, takeHand, func(change handTransition) error {
			// The run again, under the lock the hand is taken under: another
			// run may have started since it was read.
			if runID != c.runID {
				return supervise.ErrRunStale
			}
			// Under c.mu, which the core's RecordAudit allows: it never shows
			// anything on its caller's goroutine.
			return c.sup.RecordAudit(attachEntry(change, true))
		})
		if err != nil {
			return err
		}
	} else {
		_, change, err := c.releaseHand(sessionID, connID)
		if err != nil {
			return err
		}
		// Only a terminal this connection held is let go: detaching from one
		// nobody held changes nothing, and journaled attach_finished for an
		// attach that never was.
		if change.known() && change.before.held() {
			c.mu.RLock()
			sup := c.sup
			c.mu.RUnlock()
			_ = sup.RecordAudit(attachEntry(change, false))
		}
	}

	// The status is the core's: the gateway's own copy of it is never updated,
	// and a prompt the hand just turned into an ask leaves the agent waiting.
	// It is read before it is broadcast, outside the core's ordered flush, and
	// so is read again once it is out: a status the core showed meanwhile is
	// shown again after it, as refreshOutput's snapshots are.
	sup, currentRun := c.supervisor()
	shown := c.coreStatus(sup, sessionID, agent.Status)
	for attempt := 0; ; attempt++ {
		c.broadcast(eventStatus, StatusEvent{
			RunID:     currentRun,
			Scope:     "session",
			SessionID: agent.SessionID,
			Status:    shown,
		})
		now := c.coreStatus(sup, sessionID, shown)
		if now == shown || attempt == maxSnapshotCorrections {
			break
		}
		shown = now
	}

	return nil
}

// coreStatus is the session's status as the run's core holds it, or fallback
// between runs.
func (c *Controller) coreStatus(sup *supervise.Supervisor, sessionID, fallback string) string {
	if sup != nil {
		if supervised, known := sup.Agent(sessionID); known {
			return supervised.Status
		}
	}
	return fallback
}

// ResizeSession applies a terminal geometry change requested by the connection
// holding the hand. A resize from anyone else is a silent no-op: two observers
// with different window sizes must not fight over the PTY geometry and thrash
// the agent's rendering.
//
// The resize is a write to the run's backend like any other, admitted by the
// run's core (AdmitRun) as the desktop's is: a run that ends waits for it
// before its runtime closes, and takes none once it drains. The gateway
// resized whatever the run was doing, a runtime being closed included.
func (c *Controller) ResizeSession(runID, sessionID string, columns, rows int, connID string) error {
	c.mu.RLock()
	rt, sup := c.runtime, c.sup
	c.mu.RUnlock()

	if rt == nil || sup == nil {
		return errors.New("supervisor runtime not ready")
	}
	if !c.HoldsHand(sessionID, connID) {
		return nil
	}
	release, admitted := sup.AdmitRun()
	if !admitted {
		if sup.State().AuditFailed {
			return supervise.ErrAuditUnavailable
		}
		return supervise.ErrRuntimeStopped
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return rt.Resize(ctx, sessionID, terminal.Size{Columns: columns, Rows: rows})
}

// StopSession, StartSession and RestartSession go through the run's
// supervision core, which owns whether an agent runs: it takes in only the
// prompts of a running agent, so a start it did not see left the new process's
// prompts unsupervised. The core shows the agent stopping or starting while the
// operation runs, refuses one while an answer or a line is still being written
// to the session, drops the previous process's prompts when a start begins,
// freezes a session whose stop failed, and reports a failure by a fixed
// message rather than the backend's own text.
//
// Each acts only on the run the caller names. The gateway ignored the run, so
// a tab left open on a run a profile save had replaced, or a request naming
// none, stopped, started or restarted the agent of the same name in the new
// run, which the operator had never seen.
func (c *Controller) StopSession(runID, sessionID string) error {
	sup, _ := c.supervisor()
	return sup.StopSession(runID, sessionID)
}

func (c *Controller) StartSession(runID, sessionID string) error {
	sup, _ := c.supervisor()
	return sup.StartSession(runID, sessionID)
}

func (c *Controller) RestartSession(runID, sessionID string) error {
	sup, _ := c.supervisor()
	return sup.RestartSession(runID, sessionID)
}

// StopRun stops the run the caller names, and only that one. It stopped
// whatever run was current, whatever the caller named, so a tab left open on a
// replaced run, or a request naming none, stopped every agent of the new one.
//
// The run drains first (endRun): its core admits nothing more, every session
// is stopped strictly, and what the core admitted reaches its journaled
// outcome before the runtime and its journal close. None of it holds c.mu, so
// every client can still read the state meanwhile; the gateway closed the
// runtime under c.mu without waiting, which cut an answer being written off
// from its journal and blocked every other call for as long as the close took.
// Every client is told the run is stopping, then stopped. A stopped run keeps
// its ID, which "Save and restart" names to start another, and nothing else:
// its agents and prompts were its core's, and its hands are let go.
//
// A stop whose sessions could not all be confirmed stopped leaves the run
// failed and the gateway refusing to start another beside processes that may
// still run.
func (c *Controller) StopRun(runID string) (AppState, error) {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()

	c.mu.Lock()
	if atomic.LoadInt32(&c.stopped) != 0 {
		c.mu.Unlock()
		return AppState{}, supervise.ErrRuntimeStopped
	}
	if c.lifecycleBlocked {
		c.mu.Unlock()
		return AppState{}, errLifecycleBlocked
	}
	if strings.TrimSpace(runID) == "" || runID != c.runID {
		c.mu.Unlock()
		return AppState{}, supervise.ErrRunStale
	}
	if c.sup == nil {
		// Stopped already: there is nothing more to stop.
		state := c.stateLocked()
		c.mu.Unlock()
		return state, nil
	}
	end := c.beginRunEndLocked()
	c.state.RunStatus = "stopping"
	c.mu.Unlock()
	c.broadcast(eventStatus, StatusEvent{RunID: end.runID, Scope: "run", Status: "stopping"})

	err := c.endRun(context.Background(), end, true)

	c.mu.Lock()
	c.clearRunLocked()
	status := "stopped"
	if err != nil {
		c.lifecycleBlocked = true
		status = "failed"
	}
	c.state.RunStatus = status
	state := c.stateLocked()
	c.mu.Unlock()
	c.broadcast(eventStatus, StatusEvent{RunID: end.runID, Scope: "run", Status: status})
	if err != nil {
		return AppState{}, errLifecycleBlocked
	}
	return state, nil
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

	// The desktop's view, from the package both front ends share: an agent
	// the form cannot send back as it is, one with environment variables or a
	// shell script among them, is read-only, and no existing command,
	// environment value or shell body is sent, to an operator either. The
	// gateway sent each agent's full argv and marked nothing read-only, and
	// its save rebuilt every agent from the form, so every save lost every
	// agent's environment and shell script.
	profiles := make([]AgentProfile, 0, len(cfg.Agents))
	for _, spec := range cfg.Agents {
		profiles = append(profiles, AgentProfile(agentprofile.View(spec)))
	}

	view := AgentProfilesView{
		ConfigPath:      c.configPath,
		Revision:        token,
		Catalog:         c.catalogViewLocked(),
		Profiles:        profiles,
		MinProfiles:     agentprofile.MinProfiles,
		MaxProfiles:     agentprofile.MaxProfiles,
		RestartRequired: c.activeConfigRevision == "" || cfg.Revision != c.activeConfigRevision,
		Editable:        !cfg.Legacy,
	}
	if cfg.Legacy {
		view.ReadOnlyReason = "legacy_config"
	}
	return view, nil
}

// resolveProfilesLocked turns the profiles a save sent into the agents to
// write, against cfg, the configuration whose revision the save was checked
// against. The writer publishes only while the file still has that revision,
// so what a preserved agent keeps, its environment included, is exactly what
// the file holds when it is written: a file edited meanwhile makes the save
// stale, and nothing is kept from, or brought back into, a snapshot older
// than the file.
func (c *Controller) resolveProfilesLocked(inputs []AgentProfileInput, cfg config.Result) ([]agent.Spec, error) {
	if cfg.Legacy {
		return nil, errors.New("legacy configuration must be migrated to version: 1 before modifying agents")
	}
	baseDir, err := filepath.Abs(filepath.Dir(c.configPath))
	if err != nil {
		return nil, errors.New("could not resolve configuration directory")
	}
	shared := make([]agentprofile.Input, len(inputs))
	for index, input := range inputs {
		shared[index] = agentprofile.Input(input)
	}
	return agentprofile.Resolve(shared, cfg.Agents, baseDir, cfg.Backend)
}

// revisionCurrentLocked reports whether a save was prepared against the file as
// it is now: the token must be the one last handed out, and the file must
// still have the content that token was handed out for.
//
// Only the token was checked before, and it only changes when this gateway
// writes or reloads the view. A form loaded before someone edited the file by
// hand could therefore be saved over the edit: a deny rule added in the YAML
// was dropped by a save that only toggled dry-run. An empty token was also
// accepted as "no check" by the settings save.
func (c *Controller) revisionCurrentLocked(expected, fileRevision string) bool {
	return expected != "" && expected == c.revisionToken && fileRevision == c.revisionHash
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

	cfg, err := config.LoadExisting(c.configPath)
	if err != nil {
		return AgentProfilesView{}, err
	}

	if !c.revisionCurrentLocked(req.ExpectedRevision, cfg.Revision) {
		return AgentProfilesView{}, errStaleRevision
	}

	specs, err := c.resolveProfilesLocked(req.Profiles, cfg)
	if err != nil {
		return AgentProfilesView{}, err
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

// SaveAgentProfilesAndRestart writes the whole request in one write, then ends
// the run the caller names and starts another on the new configuration.
//
// It acts only on the run the caller names, the stopped one after StopRun
// included: the gateway ignored ExpectedRunID, so a tab left open on a run
// another tab had already restarted restarted the new run, and the request
// the settings panel sends with an empty run ID was taken too.
//
// The previous run drains as StopRun's does (endRun), strictly, without
// c.mu, and the new run keeps nothing of it: a supervision core of its own,
// with none of the previous run's prompts, and no hand held. The gateway
// closed the runtime under c.mu without waiting for what its core admitted,
// and handed the new run the previous run's hands, so a connection typed
// into a new process it had never attached to. Every client is told the new
// run's ID with its status, running, or failed when it could not start, so a
// tab on the previous run can load the new one; a run that failed to start
// keeps that ID, which a retry names. A previous run whose sessions could not
// all be confirmed stopped starts nothing, as on the desktop.
func (c *Controller) SaveAgentProfilesAndRestart(req SaveAgentProfilesAndRestartRequest) (LifecycleResult, error) {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()

	c.mu.Lock()
	if atomic.LoadInt32(&c.stopped) != 0 {
		c.mu.Unlock()
		return LifecycleResult{}, supervise.ErrRuntimeStopped
	}
	if c.lifecycleBlocked {
		c.mu.Unlock()
		return LifecycleResult{}, errLifecycleBlocked
	}
	if strings.TrimSpace(req.ExpectedRunID) == "" || req.ExpectedRunID != c.runID {
		c.mu.Unlock()
		return LifecycleResult{}, supervise.ErrRunStale
	}
	nextRunID, err := newRunID()
	if err != nil {
		c.mu.Unlock()
		return LifecycleResult{}, err
	}
	if err := c.saveRestartConfigurationLocked(req); err != nil {
		c.mu.Unlock()
		return LifecycleResult{}, err
	}
	end := c.beginRunEndLocked()
	if end.sup != nil {
		c.state.RunStatus = "restarting"
	}
	c.mu.Unlock()
	if end.sup != nil {
		c.broadcast(eventStatus, StatusEvent{RunID: end.runID, Scope: "run", Status: "restarting"})
	}

	stopErr := c.endRun(context.Background(), end, true)

	c.mu.Lock()
	c.clearRunLocked()
	if stopErr != nil {
		c.lifecycleBlocked = true
		c.state.RunStatus = "failed"
		c.mu.Unlock()
		c.broadcast(eventStatus, StatusEvent{RunID: end.runID, Scope: "run", Status: "failed"})
		return LifecycleResult{}, errLifecycleBlocked
	}
	if startErr := c.startLocked(context.Background(), nextRunID); startErr != nil {
		c.runID = nextRunID
		c.state.RunID = nextRunID
		c.state.RunStatus = "failed"
		c.mu.Unlock()
		c.broadcast(eventStatus, StatusEvent{RunID: nextRunID, Scope: "run", Status: "failed"})
		return LifecycleResult{}, fmt.Errorf("restarting supervisor: %w", startErr)
	}
	profilesView, _ := c.loadAgentProfilesLocked()
	result := LifecycleResult{
		Outcome:  "restarted",
		State:    c.stateLocked(),
		Profiles: profilesView,
	}
	c.mu.Unlock()
	c.broadcast(eventStatus, StatusEvent{RunID: nextRunID, Scope: "run", Status: "running"})
	return result, nil
}

// saveRestartConfigurationLocked validates a "Save and restart" request and
// writes it in one write. The caller holds c.mu.
func (c *Controller) saveRestartConfigurationLocked(req SaveAgentProfilesAndRestartRequest) error {
	cfg, err := config.LoadExisting(c.configPath)
	if err != nil {
		return err
	}

	if !c.revisionCurrentLocked(req.ExpectedRevision, cfg.Revision) {
		return errStaleRevision
	}

	specs, err := c.resolveProfilesLocked(req.Profiles, cfg)
	if err != nil {
		return err
	}

	update := config.FullConfigurationUpdate{Agents: specs, UpdateAgents: true}
	if req.Security != nil {
		pol, err := buildPolicyConfig(*req.Security, cfg.Policies, filepath.Dir(c.configPath))
		if err != nil {
			return fmt.Errorf("building policy config: %w", err)
		}
		update.Policies = &pol
	}
	if req.Notifications != nil {
		update.Notifications = convertNotificationSettings(req.Notifications)
		update.Notifications.Webhooks = notify.MergeWebhookHeaders(cfg.Notifications.Webhooks, update.Notifications.Webhooks)
	}
	// One write for the whole request. The settings panel used to save the
	// other tabs through a separate call first, so a failure here left them
	// written while the agents were not.
	_, newRev, err := config.UpdateFullConfiguration(c.configPath, cfg.Revision, update)
	if err != nil {
		return err
	}

	c.revisionHash = newRev
	tokenBytes := make([]byte, 16)
	_, _ = rand.Read(tokenBytes)
	c.revisionToken = hex.EncodeToString(tokenBytes)
	return nil
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
		Security:          extractSecuritySettings(cfg.Policies),
		SecurityPresets:   securityPresetViews(),
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

	if !c.revisionCurrentLocked(req.ExpectedRevision, cfg.Revision) {
		return FullSettingsView{}, errStaleRevision
	}

	var update config.FullConfigurationUpdate

	if len(req.Profiles) > 0 {
		specs, err := c.resolveProfilesLocked(req.Profiles, cfg)
		if err != nil {
			return FullSettingsView{}, err
		}
		update.Agents = specs
		update.UpdateAgents = true
	}

	if req.Notifications != nil {
		update.Notifications = convertNotificationSettings(req.Notifications)
		// The editor never receives header values, so it cannot send them
		// back; without this every save erased every webhook's credential.
		update.Notifications.Webhooks = notify.MergeWebhookHeaders(cfg.Notifications.Webhooks, update.Notifications.Webhooks)
	}

	if req.Security != nil {
		pol, err := buildPolicyConfig(*req.Security, cfg.Policies, filepath.Dir(c.configPath))
		if err != nil {
			return FullSettingsView{}, fmt.Errorf("building policy config: %w", err)
		}
		update.Policies = &pol
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
		c.notificationConfig = res.Notifications
		c.notifier = notify.NewWithDiagnostics(res.Notifications, c.diagnostics, c.diagnostics)
	}

	// Notifications are applied above; agents and policies only reach a new
	// run. When the save changed neither, and nothing was already waiting for
	// a restart, the running engine is still current and no restart is owed.
	agentsChanged := update.UpdateAgents && !reflect.DeepEqual(cfg.Agents, res.Agents)
	policiesChanged := !reflect.DeepEqual(cfg.Policies, res.Policies)
	if !agentsChanged && !policiesChanged && c.activeConfigRevision != "" && c.activeConfigRevision == cfg.Revision {
		c.activeConfigRevision = res.Revision
	}

	profilesView, err := c.loadAgentProfilesLocked()
	if err != nil {
		return FullSettingsView{}, err
	}

	return FullSettingsView{
		AgentProfilesView: profilesView,
		Security:          extractSecuritySettings(res.Policies),
		SecurityPresets:   securityPresetViews(),
		Notifications:     extractNotificationSettings(res.Notifications),
	}, nil
}

func (c *Controller) TestNotification() error {
	c.mu.RLock()
	notifier := c.notifier
	enabled := c.notificationConfig.Enabled
	c.mu.RUnlock()
	if !enabled {
		// A test that "succeeds" with notifications switched off tells the
		// operator the channels work when nothing would be sent.
		return errors.New("notifications are disabled")
	}

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

// extractSecuritySettings describes a policy for the settings editor. The
// conversion is a plain type conversion from policy.Settings, so the editor's
// fields cannot drift from the ones policy.ApplySettings understands.
func extractSecuritySettings(cfg policy.Config) SecuritySettings {
	return SecuritySettings(policy.SettingsFrom(cfg))
}

// securityPresetViews are the values the editor fills in when a preset is
// picked. They come from policy.ProfileConfig, like the loader's.
func securityPresetViews() map[string]SecuritySettings {
	presets := policy.PresetSettings()
	views := make(map[string]SecuritySettings, len(presets))
	for name, settings := range presets {
		views[name] = SecuritySettings(settings)
	}
	return views
}

// buildPolicyConfig applies the editor's settings to the existing policy. It
// is policy.ApplySettings, shared with the other front end: rules and blocked
// patterns survive a save that did not touch them.
func buildPolicyConfig(sec SecuritySettings, existing policy.Config, baseDir string) (policy.Config, error) {
	return policy.ApplySettings(existing, policy.Settings(sec), baseDir)
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
			HasHeaders:  len(w.Headers) > 0,
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
