package tmuxbackend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/intercept"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
)

const (
	defaultPollInterval   = time.Second
	minimumPollInterval   = 100 * time.Millisecond
	commandTimeout        = 3 * time.Second
	defaultCaptureLimit   = 256 * 1024
	pendingExitGracePolls = 3
)

var errOwnershipInvalid = errors.New("invalid tmux ownership")
var errPaneExitPending = errors.New("tmux exit status still pending")

// Manager is the sole owner of tmux sessions bearing its generated run prefix.
type Manager struct {
	ctx    context.Context
	cancel context.CancelFunc
	events chan<- session.Event

	runner           CommandRunner
	tmuxPath         string
	helperPath       string
	runtimeDirectory string
	runID            string
	ownerToken       string
	persistOnExit    bool
	cleanupOnSuccess bool
	pollInterval     time.Duration
	registry         *adapters.Registry
	ringCapacity     int
	handoffWaiter    func(context.Context, *launchFiles) error

	mu       sync.RWMutex
	sessions map[string]*managedSession
	wg       sync.WaitGroup

	lifecycleMu    sync.Mutex
	closed         bool
	activeOps      int
	operationsIdle chan struct{}
	closeGate      chan struct{}
	secretBufferMu sync.Mutex
	pendingBuffers map[string]struct{}
}

var _ terminal.Backend = (*Manager)(nil)
var _ terminal.EventSender = (*Manager)(nil)
var _ terminal.LineSender = (*Manager)(nil)
var _ terminal.PendingEventProvider = (*Manager)(nil)

// NewManager verifies tmux before creating any session and allocates a private
// 0700 runtime directory for specs and FIFO transports.
func NewManager(
	parent context.Context,
	events chan<- session.Event,
	patterns []intercept.Pattern,
	ringCapacity int,
	options Options,
) (*Manager, error) {
	registry, err := adapters.NewRegistry(patterns)
	if err != nil {
		return nil, err
	}
	return NewManagerWithRegistry(parent, events, registry, ringCapacity, options)
}

// NewManagerWithRegistry is the production constructor. Each Start resolves
// an independent adapter instance from registry before any tmux process is
// created; the legacy constructor above only translates intercept_patterns.
func NewManagerWithRegistry(
	parent context.Context,
	events chan<- session.Event,
	registry *adapters.Registry,
	ringCapacity int,
	options Options,
) (*Manager, error) {
	if err := ensurePlatformSupport(); err != nil {
		return nil, err
	}
	if events == nil {
		return nil, errors.New("nil tmux event channel")
	}
	if registry == nil {
		return nil, errors.New("registry d'adaptateurs tmux nil")
	}
	if parent == nil {
		parent = context.Background()
	}
	if ringCapacity < 1 {
		ringCapacity = defaultCaptureLimit
	}
	if options.CaptureLimit > 0 && options.CaptureLimit < ringCapacity {
		ringCapacity = options.CaptureLimit
	}
	runner := options.Runner
	if runner == nil {
		runner = execRunner{}
	}
	tmuxPath, err := ResolveBinary(runner, options.TmuxPath)
	if err != nil {
		return nil, err
	}
	helperPath := options.HelperPath
	if helperPath == "" {
		helperPath, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolving the Relayer helper: %w", err)
		}
	}
	runID := strings.TrimSpace(options.RunID)
	if runID == "" {
		runID, err = newRunID()
		if err != nil {
			return nil, fmt.Errorf("generating the tmux identifier: %w", err)
		}
	}
	ownerToken, err := newRunID()
	if err != nil {
		return nil, fmt.Errorf("generating the tmux ownership marker: %w", err)
	}
	pollInterval := options.PollInterval
	if pollInterval == 0 {
		pollInterval = defaultPollInterval
	}
	if pollInterval < minimumPollInterval {
		pollInterval = minimumPollInterval
	}

	baseDirectory := options.RuntimeDir
	if baseDirectory != "" {
		if err := os.MkdirAll(baseDirectory, 0o700); err != nil {
			return nil, fmt.Errorf("creating the tmux runtime directory: %w", err)
		}
	}
	runtimeDirectory, err := os.MkdirTemp(baseDirectory, ".relayer-tmux-"+slug(runID, 12)+"-")
	if err != nil {
		return nil, fmt.Errorf("creating the private tmux runtime: %w", err)
	}
	if err := os.Chmod(runtimeDirectory, 0o700); err != nil {
		_ = os.RemoveAll(runtimeDirectory)
		return nil, fmt.Errorf("private tmux runtime permissions: %w", err)
	}

	ctx, cancel := context.WithCancel(parent)
	manager := &Manager{
		ctx:              ctx,
		cancel:           cancel,
		events:           events,
		runner:           runner,
		tmuxPath:         tmuxPath,
		helperPath:       helperPath,
		runtimeDirectory: runtimeDirectory,
		runID:            runID,
		ownerToken:       ownerToken,
		persistOnExit:    options.PersistOnExit,
		cleanupOnSuccess: options.CleanupOnSuccess,
		pollInterval:     pollInterval,
		registry:         registry,
		ringCapacity:     ringCapacity,
		sessions:         make(map[string]*managedSession),
		operationsIdle:   closedSignal(),
		closeGate:        make(chan struct{}, 1),
		pendingBuffers:   make(map[string]struct{}),
	}
	manager.handoffWaiter = options.handoffWaiter
	if manager.handoffWaiter == nil {
		manager.handoffWaiter = func(ctx context.Context, files *launchFiles) error {
			return files.waitForHandoff(ctx)
		}
	}
	manager.closeGate <- struct{}{}
	return manager, nil
}

func (m *Manager) Name() string { return agent.BackendTmux }

func (m *Manager) Context() context.Context { return m.ctx }

func closedSignal() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

// beginOperation registers a public tmux operation before checking shutdown.
// The returned context is cancelled when either the caller or Manager stops;
// Close first closes this admission gate, then waits for all registered
// operations, so an operation cannot slip past a point-in-time closed check.
func (m *Manager) beginOperation(ctx context.Context) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.lifecycleMu.Lock()
	if m.closed || m.ctx.Err() != nil {
		m.lifecycleMu.Unlock()
		return nil, nil, ErrClosed
	}
	if m.activeOps == 0 {
		m.operationsIdle = make(chan struct{})
	}
	m.activeOps++
	m.lifecycleMu.Unlock()

	operationCtx, cancel := context.WithCancel(ctx)
	stopManagerCancellation := context.AfterFunc(m.ctx, cancel)
	var once sync.Once
	finish := func() {
		once.Do(func() {
			stopManagerCancellation()
			cancel()
			m.lifecycleMu.Lock()
			m.activeOps--
			if m.activeOps == 0 {
				close(m.operationsIdle)
			}
			m.lifecycleMu.Unlock()
		})
	}
	return operationCtx, finish, nil
}

func (m *Manager) closeAdmission() <-chan struct{} {
	m.lifecycleMu.Lock()
	m.closed = true
	m.cancel()
	idle := m.operationsIdle
	m.lifecycleMu.Unlock()
	return idle
}

// Start configures capture and remain-on-exit before releasing the
// helper gate, ensuring that even very short-lived commands are observable.
func (m *Manager) Start(ctx context.Context, spec agent.Spec, size terminal.Size) (_ terminal.Info, resultErr error) {
	operationCtx, finishOperation, err := m.beginOperation(ctx)
	if err != nil {
		return terminal.Info{}, err
	}
	defer finishOperation()
	ctx = operationCtx

	normalized, err := agent.ValidateSpec(spec, ".", agent.BackendTmux)
	if err != nil {
		return terminal.Info{}, fmt.Errorf("invalid tmux agent: %w", err)
	}
	if normalized.Backend != agent.BackendTmux {
		return terminal.Info{}, fmt.Errorf("invalid concrete backend %q for tmux", normalized.Backend)
	}
	size = size.Normalize()
	executable := ""
	if len(normalized.Command) > 0 {
		executable = normalized.Command[0]
	}
	resolvedAdapter, adapterDescriptor, err := m.registry.Resolve(normalized.Adapter, executable)
	if err != nil {
		return terminal.Info{}, fmt.Errorf("resolving adapter %q: %w", normalized.Adapter, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return terminal.Info{}, ErrClosed
	}
	for existingID := range m.sessions {
		if strings.EqualFold(existingID, normalized.ID) {
			return terminal.Info{}, fmt.Errorf("session %q already started", normalized.ID)
		}
	}
	tmuxName := SessionName(m.runID, normalized.ID)
	for _, existing := range m.sessions {
		if existing.tmuxName == tmuxName {
			return terminal.Info{}, fmt.Errorf("internal tmux name collision %q", tmuxName)
		}
	}

	files, err := createLaunchFiles(m.runtimeDirectory, tmuxName, normalized)
	if err != nil {
		return terminal.Info{}, err
	}
	cleanupFiles := true
	defer func() {
		if cleanupFiles {
			files.close()
			files.remove()
		}
	}()

	startArgs := []string{
		"new-session", "-d", "-P", "-F", tmuxFormat("#{session_id}", "#{window_id}", "#{pane_id}"), "-s", tmuxName,
		"-x", strconv.Itoa(size.Columns), "-y", strconv.Itoa(size.Rows),
	}
	if normalized.Cwd != "" {
		startArgs = append(startArgs, "-c", normalized.Cwd)
	}
	startArgs = append(startArgs, helperCommand(m.helperPath, files.specPath, files.gatePath))
	identityOutput, err := m.run(ctx, startArgs...)
	if err != nil {
		return terminal.Info{}, fmt.Errorf("creating the tmux session: %w", err)
	}
	identity := tmuxIdentity{}
	rollbackMarked := false
	created := true
	defer func() {
		if !created {
			return
		}

		// Once the private marker exists on an immutable $session ID, every
		// rollback verifies it before killing. A failed verified rollback remains
		// internal bookkeeping so Close can safely retry the immutable target.
		sessionCtx, sessionCancel := context.WithCancel(m.ctx)
		rollback := &managedSession{
			info: terminal.Info{
				ID:      normalized.ID,
				Name:    normalized.Name,
				Backend: agent.BackendTmux,
				Adapter: adapterDescriptor.ID,
			},
			tmuxName:     tmuxName,
			sessionID:    identity.sessionID,
			windowID:     identity.windowID,
			paneID:       identity.paneID,
			ownerToken:   conditionalOwnerToken(rollbackMarked, m.ownerToken),
			ctx:          sessionCtx,
			cancel:       sessionCancel,
			files:        files,
			done:         make(chan struct{}),
			exists:       true,
			forceCleanup: true,
			state: terminal.Snapshot{
				ID:     normalized.ID,
				Status: terminal.StatusFailed,
			},
		}
		var killErr error
		if !rollbackMarked {
			// Without a marker applied through the immutable ID there is no safe
			// target for kill-session: the session may have been renamed, removed,
			// or replaced between commands. Closing the gate below makes the
			// blocked helper exit; remain-on-exit has not yet been enabled.
			sessionCancel()
			return
		}
		killErr = m.killSession(ctx, rollback)
		if killErr == nil {
			sessionCancel()
			return
		}
		if rollbackMarked {
			if _, alreadyTracked := m.sessions[normalized.ID]; !alreadyTracked {
				m.sessions[normalized.ID] = rollback
				cleanupFiles = false
			} else {
				sessionCancel()
			}
		} else {
			sessionCancel()
		}
		resultErr = errors.Join(resultErr, fmt.Errorf("roll back the tmux session: %w", killErr))
	}()
	identity, err = parseIdentity(string(identityOutput))
	if err != nil {
		return terminal.Info{}, err
	}
	if _, err := m.run(ctx, "set-option", "-t", identity.sessionID, "@relayer_owner", m.ownerToken); err != nil {
		return terminal.Info{}, fmt.Errorf("marquage d'ownership tmux: %w", err)
	}
	rollbackMarked = true
	if _, err := m.run(ctx, "set-option", "-w", "-t", identity.windowID, "remain-on-exit", "on"); err != nil {
		return terminal.Info{}, fmt.Errorf("configuration remain-on-exit: %w", err)
	}
	if _, err := m.run(ctx, "pipe-pane", "-o", "-t", identity.paneID, pipeCommand(files.outputPath)); err != nil {
		return terminal.Info{}, fmt.Errorf("configure the tmux capture: %w", err)
	}
	if m.ctx.Err() != nil {
		return terminal.Info{}, ErrClosed
	}

	metadata := terminal.Info{
		ID:             normalized.ID,
		Name:           normalized.Name,
		DisplayCommand: displayCommand(normalized),
		Shell:          normalized.Shell != "",
		Backend:        agent.BackendTmux,
		Adapter:        adapterDescriptor.ID,
	}
	sessionCtx, sessionCancel := context.WithCancel(m.ctx)
	managed := &managedSession{
		info:       metadata,
		tmuxName:   tmuxName,
		sessionID:  identity.sessionID,
		windowID:   identity.windowID,
		paneID:     identity.paneID,
		ownerToken: m.ownerToken,
		ctx:        sessionCtx,
		cancel:     sessionCancel,
		files:      files,
		done:       make(chan struct{}),
		exists:     true,
		state: Snapshot{
			ID:      normalized.ID,
			Status:  StatusDetached,
			Running: true,
		},
		appliedSize: size,
		sizeKnown:   true,
	}
	managed.processor, err = adapters.NewProcessor(
		resolvedAdapter,
		adapters.NewDetectionState(normalized.ID, normalized.ID, adapterDescriptor.ID),
		m.ringCapacity,
		adapters.Hooks{
			OnOutput: func() {
				managed.outputObserved()
				m.emit(session.OutputAvailable{SessionID: normalized.ID}, false)
			},
			OnEvent: func(event adapters.Event) {
				if managed.claimAdapterEvent(event) {
					m.emit(session.AdapterEvent{Event: event.Clone()}, true)
				}
			},
		})
	if err != nil {
		sessionCancel()
		return terminal.Info{}, err
	}
	initialSnapshot, pipeActive, err := m.inspectRaw(ctx, managed)
	if err != nil {
		sessionCancel()
		return terminal.Info{}, fmt.Errorf("verifying the tmux capture: %w", err)
	}
	if !initialSnapshot.Running || !pipeActive {
		sessionCancel()
		return terminal.Info{}, errors.New("tmux capture inactive before agent start")
	}
	managed.updateState(initialSnapshot)
	m.sessions[normalized.ID] = managed
	m.wg.Add(2)
	output := files.output
	go m.readOutput(managed, output)
	go m.monitor(managed)
	if err := files.release(); err != nil {
		delete(m.sessions, normalized.ID)
		managed.closeTransport()
		return terminal.Info{}, fmt.Errorf("releasing the tmux helper: %w", err)
	}
	handoffCtx, handoffCancel := context.WithTimeout(ctx, commandTimeout)
	handoffErr := m.handoffWaiter(handoffCtx, files)
	handoffCancel()
	if handoffErr != nil {
		delete(m.sessions, normalized.ID)
		managed.closeTransport()
		return terminal.Info{}, handoffErr
	}
	cleanupFiles = false
	created = false
	return metadata, nil
}

func (m *Manager) emit(event session.Event, essential bool) bool {
	return m.emitWithContext(context.Background(), event, essential)
}

func (m *Manager) emitWithContext(ctx context.Context, event session.Event, essential bool) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	if essential {
		select {
		case m.events <- event:
			return true
		case <-m.ctx.Done():
			return false
		case <-ctx.Done():
			return false
		}
	}
	select {
	case m.events <- event:
		return true
	case <-m.ctx.Done():
		return false
	case <-ctx.Done():
		return false
	default:
		return false
	}
}

func (m *Manager) readOutput(target *managedSession, output *os.File) {
	defer m.wg.Done()
	err := target.processor.Run(target.ctx, output)
	m.emit(session.OutputAvailable{SessionID: target.info.ID}, true)
	if err != nil && target.ctx.Err() == nil && !errors.Is(err, os.ErrClosed) && !errors.Is(err, io.EOF) {
		m.emit(session.Error{SessionID: target.info.ID, Err: err}, true)
	}
}

func (m *Manager) monitor(target *managedSession) {
	defer m.wg.Done()
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()

	var (
		deadSeen         bool
		deadSequence     uint64
		pollFailures     int
		pendingExitPolls int
	)
	for {
		select {
		case <-target.ctx.Done():
			return
		case <-ticker.C:
		}
		ctx, cancel := context.WithTimeout(target.ctx, commandTimeout)
		snapshot, err := m.inspect(ctx, target)
		cancel()
		if err != nil {
			if target.ctx.Err() != nil {
				return
			}
			// tmux can report pane_dead before its wait status or signal is
			// ready. This is a normal intermediate lifecycle state, not a
			// backend failure worth surfacing to the operator.
			if errors.Is(err, errPaneExitPending) {
				pollFailures = 0
				pendingExitPolls++
				if pendingExitPolls < pendingExitGracePolls {
					continue
				}
				// Some tmux 3.4 servers leave both wait formats empty even
				// after pane_dead is stable. The pane is authoritatively closed,
				// but its exit code is unknown: publish a neutral terminal state
				// without inventing success, failure or a numeric status.
				snapshot = Snapshot{ID: target.info.ID, Status: StatusExited}
			} else {
				pendingExitPolls = 0
				pollFailures++
				if pollFailures >= 3 {
					if errors.Is(err, errOwnershipInvalid) {
						m.handleOwnershipLoss(target, err)
						return
					}
					probeCtx, probeCancel := context.WithTimeout(target.ctx, commandTimeout)
					_, probeErr := m.run(probeCtx, "has-session", "-t", target.sessionID)
					probeCancel()
					if isMissingTargetProbe(probeErr) {
						m.handleMissingTarget(target, err)
						return
					}
					pollFailures = 0
					m.emit(session.Error{SessionID: target.info.ID, Err: errors.Join(err, probeErr)}, false)
				}
				continue
			}
		} else {
			pendingExitPolls = 0
		}
		pollFailures = 0
		if snapshot.Running {
			target.updateState(snapshot)
			deadSeen = false
			continue
		}
		target.updateProcessExitState(snapshot)
		sequence := target.outputSequenceValue()
		if !deadSeen || sequence != deadSequence {
			deadSeen = true
			deadSequence = sequence
			continue
		}
		m.finishSession(target, snapshot)
		return
	}
}

func (m *Manager) handleMissingTarget(target *managedSession, cause error) {
	failure := fmt.Errorf("%w: %v", ErrSessionNotFound, cause)
	if !target.isPresent() {
		return
	}
	target.markRemoved()
	snapshot := Snapshot{ID: target.info.ID, Status: StatusFailed}
	target.updateProcessExitState(snapshot)
	if target.finish() {
		m.emit(session.Error{SessionID: target.info.ID, Err: failure}, true)
		m.emit(session.AdapterEvent{Event: target.processExitEvent(snapshot)}, true)
	}
	target.closeTransport()
}

func (m *Manager) handleOwnershipLoss(target *managedSession, cause error) {
	failure := fmt.Errorf("supervision tmux interrompue: %w", cause)
	if !target.isPresent() {
		return
	}
	target.markRemoved()
	snapshot := Snapshot{ID: target.info.ID, Status: StatusFailed}
	target.updateState(snapshot)
	if target.finish() {
		m.emit(session.Error{SessionID: target.info.ID, Err: failure}, true)
		// Losing ownership ends Relayer's supervision, not necessarily the
		// foreign process. A legacy Exited message closes local UI state without
		// asserting the canonical process_exit/session_finished audit fact.
		m.emit(session.Exited{SessionID: target.info.ID, Err: failure}, true)
	}
	target.closeTransport()
}

func (m *Manager) finishSession(target *managedSession, snapshot Snapshot) {
	target.updateProcessExitState(snapshot)
	if !target.finish() {
		return
	}
	m.emit(session.AdapterEvent{Event: target.processExitEvent(snapshot)}, true)
	if m.cleanupOnSuccess && snapshot.ExitCode != nil && *snapshot.ExitCode == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		err := m.killSession(ctx, target)
		cancel()
		if err != nil {
			m.emit(session.Error{SessionID: target.info.ID, Err: err}, true)
			return
		}
		target.closeTransport()
	}
}

func (m *Manager) session(id string) (*managedSession, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for existingID, target := range m.sessions {
		// forceCleanup entries are bookkeeping for failed Start rollbacks. They
		// were never published as terminal sessions and must not be reachable
		// through the public API. Close still iterates the map directly.
		if !target.forceCleanup && strings.EqualFold(existingID, id) {
			return target, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrSessionNotFound, id)
}

func (m *Manager) Output(id string) (string, error) {
	target, err := m.session(id)
	if err != nil {
		return "", err
	}
	if target.processor == nil {
		return "", nil
	}
	return target.processor.Output(), nil
}

func (m *Manager) Done(id string) (<-chan struct{}, error) {
	target, err := m.session(id)
	if err != nil {
		return nil, err
	}
	return target.done, nil
}

func (m *Manager) SendInput(id, value string) error {
	return m.Send(m.ctx, id, []byte(value+"\r"))
}

func (m *Manager) Send(ctx context.Context, id string, data []byte) error {
	return m.SendEvent(ctx, id, "", data)
}

// SendLine serializes ordinary text with tmux delivery, event detection and
// process termination. Processor.SendLine appends the sole carriage return
// and does not acknowledge an actionable event.
func (m *Manager) SendLine(ctx context.Context, id, line string) error {
	operationCtx, finishOperation, err := m.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer finishOperation()
	ctx = operationCtx

	target, err := m.session(id)
	if err != nil {
		return err
	}
	if !target.isPresent() {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	target.inputMu.Lock()
	defer target.inputMu.Unlock()
	target.interactionMu.Lock()
	defer target.interactionMu.Unlock()
	err = target.sendLine(ctx, line, func(data []byte) error {
		return m.sendBytes(ctx, target, data)
	})
	if errors.Is(err, ErrSessionNotFound) {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	if errors.Is(err, adapters.ErrProcessorTerminated) {
		return ErrClosed
	}
	return err
}

// SendEvent serializes an exact event decision with terminal delivery. The
// Processor clears the pending occurrence only after tmux accepted the bytes;
// an empty eventID preserves the legacy raw Send contract.
func (m *Manager) SendEvent(ctx context.Context, id, eventID string, data []byte) error {
	operationCtx, finishOperation, err := m.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer finishOperation()
	ctx = operationCtx

	target, err := m.session(id)
	if err != nil {
		return err
	}
	if !target.isPresent() {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	target.inputMu.Lock()
	defer target.inputMu.Unlock()
	target.interactionMu.Lock()
	defer target.interactionMu.Unlock()

	err = target.processor.Resolve(eventID, func() error {
		return m.sendBytes(ctx, target, data)
	})
	if errors.Is(err, adapters.ErrProcessorTerminated) {
		return ErrClosed
	}
	return err
}

func (m *Manager) sendBytes(ctx context.Context, target *managedSession, data []byte) error {
	if _, err := m.verifyPane(ctx, target); err != nil {
		return err
	}
	bufferName := target.tmuxName + "-" + shortHash(target.ownerToken) + "-input"
	// Register the deterministic name before load-buffer. If tmux accepts the
	// bytes but the client loses the acknowledgement, cleanup still knows exactly
	// which private buffer may contain the user's input.
	m.trackSecretBuffer(bufferName)
	if _, err := m.runInput(ctx, data, "load-buffer", "-b", bufferName, "-"); err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), commandTimeout)
		cleanupErr := m.deleteSecretBuffer(cleanupCtx, bufferName)
		cleanupCancel()
		if cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("deleting the private tmux buffer: %w", cleanupErr))
		}
		return err
	}
	if _, err := m.run(ctx, "paste-buffer", "-d", "-b", bufferName, "-t", target.paneID); err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), commandTimeout)
		cleanupErr := m.deleteSecretBuffer(cleanupCtx, bufferName)
		cleanupCancel()
		if cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("deleting the private tmux buffer: %w", cleanupErr))
		}
		return err
	}
	m.forgetSecretBuffer(bufferName)
	return nil
}

func (m *Manager) trackSecretBuffer(name string) {
	m.secretBufferMu.Lock()
	m.pendingBuffers[name] = struct{}{}
	m.secretBufferMu.Unlock()
}

func (m *Manager) forgetSecretBuffer(name string) {
	m.secretBufferMu.Lock()
	delete(m.pendingBuffers, name)
	m.secretBufferMu.Unlock()
}

func (m *Manager) secretBuffers() []string {
	m.secretBufferMu.Lock()
	defer m.secretBufferMu.Unlock()
	names := make([]string, 0, len(m.pendingBuffers))
	for name := range m.pendingBuffers {
		names = append(names, name)
	}
	return names
}

func (m *Manager) deleteSecretBuffer(ctx context.Context, name string) error {
	if _, err := m.run(ctx, "delete-buffer", "-b", name); err != nil {
		return err
	}
	m.forgetSecretBuffer(name)
	return nil
}

func (m *Manager) Resize(ctx context.Context, id string, size terminal.Size) error {
	operationCtx, finishOperation, err := m.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer finishOperation()
	return m.resize(operationCtx, id, size)
}

func (m *Manager) resize(ctx context.Context, id string, size terminal.Size) error {
	target, err := m.session(id)
	if err != nil {
		return err
	}
	if !target.isPresent() {
		return nil
	}
	size = size.Normalize()
	target.resizeMu.Lock()
	defer target.resizeMu.Unlock()
	if target.sizeApplied(size) {
		return nil
	}
	windowID, err := m.verifyPane(ctx, target)
	if err != nil {
		return err
	}
	_, err = m.run(ctx,
		"resize-window", "-t", windowID,
		"-x", strconv.Itoa(size.Columns), "-y", strconv.Itoa(size.Rows),
	)
	if err == nil {
		target.recordAppliedSize(size)
	}
	return err
}

func (m *Manager) Snapshot(ctx context.Context, id string) (terminal.Snapshot, error) {
	operationCtx, finishOperation, err := m.beginOperation(ctx)
	if err != nil {
		return terminal.Snapshot{}, err
	}
	defer finishOperation()
	ctx = operationCtx

	target, err := m.session(id)
	if err != nil {
		return terminal.Snapshot{}, err
	}
	if !target.isPresent() {
		return target.snapshot(), nil
	}
	snapshot, err := m.inspect(ctx, target)
	if err != nil {
		return terminal.Snapshot{}, err
	}
	if snapshot.Running {
		target.updateState(snapshot)
	} else {
		target.updateProcessExitState(snapshot)
	}
	return target.snapshot(), nil
}

// PendingEvent returns cached processor state only. In particular, it never
// starts a tmux subprocess from Bubble Tea's Update path.
func (m *Manager) PendingEvent(ctx context.Context, id string) (*adapters.Event, error) {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}
	target, err := m.session(id)
	if err != nil {
		return nil, err
	}
	return target.pendingEvent(), nil
}

func (m *Manager) inspect(ctx context.Context, target *managedSession) (terminal.Snapshot, error) {
	snapshot, pipeActive, err := m.inspectRaw(ctx, target)
	if err != nil {
		return terminal.Snapshot{}, err
	}
	if !snapshot.Running || pipeActive {
		return snapshot, nil
	}
	if _, err := m.run(ctx, "pipe-pane", "-o", "-t", target.paneID, pipeCommand(target.files.outputPath)); err != nil {
		return terminal.Snapshot{}, fmt.Errorf("restore the tmux capture: %w", err)
	}
	snapshot, pipeActive, err = m.inspectRaw(ctx, target)
	if err != nil {
		return terminal.Snapshot{}, err
	}
	if !pipeActive {
		return terminal.Snapshot{}, errors.New("tmux capture inactive after restoration")
	}
	return snapshot, nil
}

func (m *Manager) inspectRaw(ctx context.Context, target *managedSession) (terminal.Snapshot, bool, error) {
	if target.ownerToken == "" || !validTmuxID(target.sessionID, '$') || !validTmuxID(target.paneID, '%') {
		return terminal.Snapshot{}, false, fmt.Errorf("%w: identity missing or invalid", errOwnershipInvalid)
	}
	output, err := m.run(ctx,
		"display-message", "-p", "-t", target.paneID,
		tmuxFormat(
			"#{session_id}", "#{pane_id}", "#{@relayer_owner}", "#{pane_dead}",
			"#{pane_dead_status}", "#{pane_dead_signal}", "#{session_attached}", "#{pane_pipe}",
		),
	)
	if err != nil {
		return terminal.Snapshot{}, false, err
	}
	fields := splitTmuxFields(string(output))
	if len(fields) != 8 {
		// Without the complete immutable IDs and owner marker, the response cannot
		// be attributed to this Manager. Treat malformed identity output like an
		// ownership loss instead of probing only for name existence forever.
		return terminal.Snapshot{}, false, fmt.Errorf("%w: incomplete inspection", errOwnershipInvalid)
	}
	if fields[0] != target.sessionID || fields[1] != target.paneID || fields[2] != target.ownerToken {
		return terminal.Snapshot{}, false, fmt.Errorf("%w: cible inattendue", errOwnershipInvalid)
	}
	snapshot, err := parseSnapshot(target.info.ID, strings.Join(fields[3:7], tmuxFieldSeparator))
	if err != nil {
		return terminal.Snapshot{}, false, err
	}
	pipe, err := strconv.Atoi(strings.TrimSpace(fields[7]))
	if err != nil || (pipe != 0 && pipe != 1) {
		return terminal.Snapshot{}, false, errors.New("invalid tmux pane_pipe flag")
	}
	return snapshot, pipe == 1, nil
}

// tmux rewrites unprintable bytes while rendering a format: on tmux 3.7 a TAB
// becomes "_" unless TMUX is present in the environment, which it never is when
// Relayer runs from an ordinary shell. Every machine-read format therefore uses
// a printable separator that tmux passes through unchanged. Each field read this
// way is a tmux ID ($n, @n, %n), a decimal number, a signal name, or the hex
// owner token, so the separator cannot occur inside a value.
const tmuxFieldSeparator = "|"

// tmuxFormat builds a machine-readable -F or display-message format. Callers
// pair it with splitTmuxFields so the separator cannot drift between the
// request and its parser.
func tmuxFormat(fields ...string) string {
	return strings.Join(fields, tmuxFieldSeparator)
}

// splitTmuxFields parses a response produced by tmuxFormat. Unlike a TAB
// separator, a trailing empty field survives TrimSpace and is reported as the
// malformed response it is.
func splitTmuxFields(output string) []string {
	return strings.Split(strings.TrimSpace(output), tmuxFieldSeparator)
}

func parseSnapshot(id, output string) (terminal.Snapshot, error) {
	fields := splitTmuxFields(output)
	if len(fields) != 3 && len(fields) != 4 {
		return terminal.Snapshot{}, fmt.Errorf("invalid tmux state")
	}
	attachedField := fields[2]
	deadSignal := ""
	if len(fields) == 4 {
		deadSignal = strings.TrimSpace(fields[2])
		attachedField = fields[3]
	}
	dead, err := strconv.Atoi(strings.TrimSpace(fields[0]))
	if err != nil || (dead != 0 && dead != 1) {
		return terminal.Snapshot{}, fmt.Errorf("invalid tmux pane_dead flag")
	}
	attached, err := strconv.Atoi(strings.TrimSpace(attachedField))
	if err != nil || attached < 0 {
		return terminal.Snapshot{}, fmt.Errorf("invalid tmux session_attached flag")
	}
	snapshot := terminal.Snapshot{ID: id, Running: dead == 0, Attached: attached > 0}
	if dead == 0 {
		if attached > 0 {
			snapshot.Status = StatusAttached
		} else {
			snapshot.Status = StatusDetached
		}
		return snapshot, nil
	}
	deadStatus := strings.TrimSpace(fields[1])
	if deadStatus == "" {
		if deadSignal != "" {
			snapshot.Status = StatusFailed
			return snapshot, nil
		}
		return terminal.Snapshot{}, errPaneExitPending
	}
	code, err := strconv.Atoi(deadStatus)
	if err != nil {
		return terminal.Snapshot{}, fmt.Errorf("invalid tmux exit code")
	}
	snapshot.ExitCode = &code
	if code == 0 {
		snapshot.Status = StatusExited
	} else {
		snapshot.Status = StatusFailed
	}
	return snapshot, nil
}

type tmuxIdentity struct {
	sessionID string
	windowID  string
	paneID    string
}

func parseIdentity(output string) (tmuxIdentity, error) {
	fields := splitTmuxFields(output)
	if len(fields) != 3 || !validTmuxID(fields[0], '$') || !validTmuxID(fields[1], '@') || !validTmuxID(fields[2], '%') {
		return tmuxIdentity{}, errors.New("identifiants immuables tmux invalides")
	}
	return tmuxIdentity{sessionID: fields[0], windowID: fields[1], paneID: fields[2]}, nil
}

func validTmuxID(value string, prefix byte) bool {
	if len(value) < 2 || value[0] != prefix {
		return false
	}
	number, err := strconv.Atoi(value[1:])
	return err == nil && number >= 0
}

func (m *Manager) verifySession(ctx context.Context, target *managedSession) error {
	if target.ownerToken == "" || !validTmuxID(target.sessionID, '$') {
		return fmt.Errorf("%w: session missing or invalid", errOwnershipInvalid)
	}
	output, err := m.run(ctx,
		"display-message", "-p", "-t", target.sessionID,
		tmuxFormat("#{session_id}", "#{@relayer_owner}"),
	)
	if err != nil {
		return err
	}
	fields := splitTmuxFields(string(output))
	if len(fields) != 2 || fields[0] != target.sessionID || fields[1] != target.ownerToken {
		return fmt.Errorf("%w: session inattendue", errOwnershipInvalid)
	}
	return nil
}

func (m *Manager) verifyPane(ctx context.Context, target *managedSession) (string, error) {
	if target.ownerToken == "" || !validTmuxID(target.sessionID, '$') || !validTmuxID(target.paneID, '%') {
		return "", fmt.Errorf("%w: pane missing or invalid", errOwnershipInvalid)
	}
	output, err := m.run(ctx,
		"display-message", "-p", "-t", target.paneID,
		tmuxFormat("#{session_id}", "#{window_id}", "#{pane_id}", "#{@relayer_owner}"),
	)
	if err != nil {
		return "", err
	}
	fields := splitTmuxFields(string(output))
	if len(fields) != 4 || fields[0] != target.sessionID || fields[2] != target.paneID || fields[3] != target.ownerToken || !validTmuxID(fields[1], '@') {
		return "", fmt.Errorf("%w: pane inattendu", errOwnershipInvalid)
	}
	return fields[1], nil
}

func (m *Manager) AttachCommand(ctx context.Context, id string) (*exec.Cmd, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	attachParent := ctx
	operationCtx, finishOperation, err := m.beginOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer finishOperation()

	target, err := m.session(id)
	if err != nil {
		return nil, err
	}
	if !target.isPresent() {
		return nil, fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	target.interactionMu.Lock()
	defer target.interactionMu.Unlock()
	attachCtx, cancelAttach := context.WithCancel(attachParent)
	stopManagerCancellation := context.AfterFunc(m.ctx, cancelAttach)
	if !target.beginAttach(cancelAttach, stopManagerCancellation) {
		stopManagerCancellation()
		cancelAttach()
		return nil, errors.New("tmux attachment already active")
	}
	if err := m.verifySession(operationCtx, target); err != nil {
		target.endAttach()
		return nil, err
	}
	command := m.runner.Command(attachCtx, CommandSpec{
		Path: m.tmuxPath,
		Args: []string{"attach-session", "-t", target.sessionID},
	})
	if command == nil {
		target.endAttach()
		return nil, errors.New("CommandRunner returned a nil attach command")
	}
	return command, nil
}

// Resync suppresses live adapter events while the real terminal is attached,
// then atomically reconciles the Processor against the current active pane
// line. Event occurrence IDs provide deduplication across output and snapshots.
func (m *Manager) Resync(ctx context.Context, id string, columns, rows int) error {
	operationCtx, finishOperation, err := m.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer finishOperation()
	ctx = operationCtx

	target, err := m.session(id)
	if err != nil {
		return err
	}
	target.endAttach()
	if err := m.resize(ctx, id, terminal.Size{Columns: columns, Rows: rows}); err != nil {
		return err
	}
	var (
		currentSnapshot terminal.Snapshot
		raw             []byte
	)
	if target.isPresent() {
		snapshot, inspectErr := m.inspect(ctx, target)
		if inspectErr != nil {
			return inspectErr
		}
		if snapshot.Running {
			target.updateState(snapshot)
			raw, err = m.capturePaneTail(ctx, target)
			if err != nil {
				return err
			}
		} else {
			target.updateProcessExitState(snapshot)
		}
		currentSnapshot = snapshot
	}
	target.interactionMu.Lock()
	pending, _, err := target.processor.ReconcileSnapshot(raw)
	target.interactionMu.Unlock()
	if err != nil {
		return err
	}
	if pending != nil && target.claimAdapterEvent(*pending) {
		m.emit(session.AdapterEvent{Event: pending.Clone()}, true)
	}
	if currentSnapshot.ID != "" && !currentSnapshot.Running {
		m.finishSession(target, currentSnapshot)
	}
	m.emit(session.OutputAvailable{SessionID: id}, true)
	return nil
}

func (m *Manager) capturePaneTail(ctx context.Context, target *managedSession) ([]byte, error) {
	if _, err := m.verifyPane(ctx, target); err != nil {
		return nil, err
	}
	output, err := m.run(ctx,
		"capture-pane", "-p", "-J", "-t", target.paneID, "-S", "-8",
	)
	if err != nil {
		return nil, err
	}
	return output, nil
}

func (m *Manager) Stop(ctx context.Context, id string) error {
	operationCtx, finishOperation, err := m.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer finishOperation()
	ctx = operationCtx

	target, err := m.session(id)
	if err != nil {
		return err
	}
	if !target.isPresent() {
		return nil
	}
	if err := m.killSession(ctx, target); err != nil {
		return err
	}
	stopped := terminal.Snapshot{
		ID:      target.info.ID,
		Status:  terminal.StatusExited,
		Running: false,
	}
	target.updateProcessExitState(stopped)
	if target.finish() {
		m.emitWithContext(ctx, session.AdapterEvent{Event: target.processExitEvent(stopped)}, true)
	}
	target.closeTransport()
	return nil
}

func (m *Manager) BeginShutdown() { m.closeAdmission() }

// Close is idempotent after success and retryable after a timeout or cleanup
// error. PersistOnExit skips owned session kills, but private tmux buffers and
// failed-Start placeholders are always cleaned. Every kill is ownership
// checked and every attempt shares one bounded caller-aware cleanup budget.
func (m *Manager) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		m.closeAdmission()
		return ctx.Err()
	case <-m.closeGate:
	}
	defer func() { m.closeGate <- struct{}{} }()

	idle := m.closeAdmission()
	select {
	case <-idle:
	case <-ctx.Done():
		return ctx.Err()
	}

	m.mu.RLock()
	targets := make([]*managedSession, 0, len(m.sessions))
	for _, target := range m.sessions {
		targets = append(targets, target)
	}
	m.mu.RUnlock()

	type cleanupTask func(context.Context) error
	tasks := make([]cleanupTask, 0, len(targets)+len(m.secretBuffers()))
	for _, target := range targets {
		if !target.isPresent() {
			continue
		}
		target := target
		if m.persistOnExit && !target.forceCleanup {
			if target.captureNeedsDisable() {
				tasks = append(tasks, func(cleanupCtx context.Context) error {
					return m.disableCapture(cleanupCtx, target)
				})
			}
			continue
		}
		tasks = append(tasks, func(cleanupCtx context.Context) error {
			return m.killSession(cleanupCtx, target)
		})
	}
	for _, bufferName := range m.secretBuffers() {
		bufferName := bufferName
		tasks = append(tasks, func(cleanupCtx context.Context) error {
			if err := m.deleteSecretBuffer(cleanupCtx, bufferName); err != nil {
				return fmt.Errorf("deleting the private tmux buffer: %w", err)
			}
			return nil
		})
	}

	var closeErr error
	if len(tasks) > 0 {
		cleanupCtx, cleanupCancel := context.WithTimeout(ctx, commandTimeout)
		errorsChannel := make(chan error, len(tasks))
		var cleanupGroup sync.WaitGroup
		for _, task := range tasks {
			cleanupGroup.Add(1)
			go func(task cleanupTask) {
				defer cleanupGroup.Done()
				if err := task(cleanupCtx); err != nil {
					errorsChannel <- err
				}
			}(task)
		}
		cleanupGroup.Wait()
		cleanupCancel()
		close(errorsChannel)
		for err := range errorsChannel {
			closeErr = errors.Join(closeErr, err)
		}
	}

	for _, target := range targets {
		target.closeTransport()
		target.finish()
	}
	waitDone := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-ctx.Done():
		return errors.Join(closeErr, ctx.Err())
	}
	if err := os.RemoveAll(m.runtimeDirectory); err != nil {
		closeErr = errors.Join(closeErr, err)
	}
	return closeErr
}

func (m *Manager) disableCapture(ctx context.Context, target *managedSession) error {
	target.killMu.Lock()
	defer target.killMu.Unlock()
	if !target.captureNeedsDisable() {
		return nil
	}
	if _, err := m.verifyPane(ctx, target); err != nil {
		return err
	}
	// Calling pipe-pane without a shell command disables the existing pipe while
	// leaving the explicitly persistent pane and its user process untouched.
	if _, err := m.run(ctx, "pipe-pane", "-t", target.paneID); err != nil {
		return err
	}
	target.markCaptureDisabled()
	return nil
}

func (m *Manager) killSession(ctx context.Context, target *managedSession) error {
	target.killMu.Lock()
	defer target.killMu.Unlock()
	if !target.isPresent() {
		return nil
	}
	if target.sessionID == "" {
		return errors.New("tmux session without an immutable ID")
	}
	if err := m.verifySession(ctx, target); err != nil {
		return err
	}
	if _, err := m.run(ctx, "kill-session", "-t", target.sessionID); err != nil {
		return err
	}
	if _, err := m.run(ctx, "has-session", "-t", target.sessionID); err == nil {
		return fmt.Errorf("%w: the immutable identifier still exists", ErrStopUncertain)
	} else if !isMissingTargetProbe(err) {
		return errors.Join(ErrStopUncertain, err)
	}
	target.markRemoved()
	return nil
}

func isMissingTargetProbe(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		return false
	}
	diagnostic := strings.ToLower(strings.TrimSpace(string(exitError.Stderr)))
	if diagnostic == "" || strings.Contains(diagnostic, "\n") {
		return false
	}
	return strings.HasPrefix(diagnostic, "can't find session:") ||
		strings.HasPrefix(diagnostic, "no server running on ")
}

func conditionalOwnerToken(marked bool, token string) string {
	if marked {
		return token
	}
	return ""
}

func displayCommand(spec agent.Spec) string {
	if spec.Shell != "" {
		return "[shell explicite]"
	}
	parts := make([]string, len(spec.Command))
	for index, argument := range spec.Command {
		parts[index] = strconv.Quote(argument)
	}
	return strings.Join(parts, " ")
}

// RuntimeDirectory exposes only the private directory location for diagnostics
// and permission tests; it never exposes a specification path or its content.
func (m *Manager) RuntimeDirectory() string { return filepath.Clean(m.runtimeDirectory) }
