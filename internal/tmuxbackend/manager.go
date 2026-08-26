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

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/intercept"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
)

const (
	defaultPollInterval = time.Second
	minimumPollInterval = 100 * time.Millisecond
	commandTimeout      = 3 * time.Second
	defaultCaptureLimit = 256 * 1024
)

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
	patterns         []intercept.Pattern
	ringCapacity     int

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

// NewManager verifies tmux before creating any session and allocates a private
// 0700 runtime directory for specs and FIFO transports.
func NewManager(
	parent context.Context,
	events chan<- session.Event,
	patterns []intercept.Pattern,
	ringCapacity int,
	options Options,
) (*Manager, error) {
	if err := ensurePlatformSupport(); err != nil {
		return nil, err
	}
	if events == nil {
		return nil, errors.New("canal d'événements tmux nil")
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
	if _, err := intercept.New(patterns, ringCapacity, intercept.Hooks{}); err != nil {
		return nil, err
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
			return nil, fmt.Errorf("résolution du helper Relayer: %w", err)
		}
	}
	runID := strings.TrimSpace(options.RunID)
	if runID == "" {
		runID, err = newRunID()
		if err != nil {
			return nil, fmt.Errorf("génération de l'identifiant tmux: %w", err)
		}
	}
	ownerToken, err := newRunID()
	if err != nil {
		return nil, fmt.Errorf("génération du marqueur d'ownership tmux: %w", err)
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
			return nil, fmt.Errorf("création du dossier runtime tmux: %w", err)
		}
	}
	runtimeDirectory, err := os.MkdirTemp(baseDirectory, ".relayer-tmux-"+slug(runID, 12)+"-")
	if err != nil {
		return nil, fmt.Errorf("création du runtime tmux privé: %w", err)
	}
	if err := os.Chmod(runtimeDirectory, 0o700); err != nil {
		_ = os.RemoveAll(runtimeDirectory)
		return nil, fmt.Errorf("permissions du runtime tmux privé: %w", err)
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
		patterns:         append([]intercept.Pattern(nil), patterns...),
		ringCapacity:     ringCapacity,
		sessions:         make(map[string]*managedSession),
		operationsIdle:   closedSignal(),
		closeGate:        make(chan struct{}, 1),
		pendingBuffers:   make(map[string]struct{}),
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
		return terminal.Info{}, fmt.Errorf("agent tmux invalide: %w", err)
	}
	if normalized.Backend != agent.BackendTmux {
		return terminal.Info{}, fmt.Errorf("backend concret %q invalide pour tmux", normalized.Backend)
	}
	size = size.Normalize()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return terminal.Info{}, ErrClosed
	}
	for existingID := range m.sessions {
		if strings.EqualFold(existingID, normalized.ID) {
			return terminal.Info{}, fmt.Errorf("session %q déjà démarrée", normalized.ID)
		}
	}
	tmuxName := SessionName(m.runID, normalized.ID)
	for _, existing := range m.sessions {
		if existing.tmuxName == tmuxName {
			return terminal.Info{}, fmt.Errorf("collision interne du nom tmux %q", tmuxName)
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
		"new-session", "-d", "-P", "-F", "#{session_id}\t#{window_id}\t#{pane_id}", "-s", tmuxName,
		"-x", strconv.Itoa(size.Columns), "-y", strconv.Itoa(size.Rows),
	}
	if normalized.Cwd != "" {
		startArgs = append(startArgs, "-c", normalized.Cwd)
	}
	startArgs = append(startArgs, helperCommand(m.helperPath, files.specPath, files.gatePath))
	identityOutput, err := m.run(ctx, startArgs...)
	if err != nil {
		return terminal.Info{}, fmt.Errorf("création de la session tmux: %w", err)
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
		resultErr = errors.Join(resultErr, fmt.Errorf("rollback de la session tmux: %w", killErr))
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
		return terminal.Info{}, fmt.Errorf("configuration de la capture tmux: %w", err)
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
	}
	managed.interceptor, err = intercept.New(m.patterns, m.ringCapacity, intercept.Hooks{
		OnOutput: func() {
			managed.outputObserved()
			m.emit(session.OutputAvailable{SessionID: normalized.ID}, false)
		},
		OnPrompt: func(detection intercept.Detection) {
			prompt := session.PromptDetected{
				SessionID:   normalized.ID,
				Pattern:     detection.Pattern,
				Description: detection.Description,
				Match:       detection.Match,
				Sensitive:   detection.Sensitive,
			}
			if !managed.setPrompt(prompt) {
				m.emit(prompt, true)
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
		return terminal.Info{}, fmt.Errorf("vérification de la capture tmux: %w", err)
	}
	if !initialSnapshot.Running || !pipeActive {
		sessionCancel()
		return terminal.Info{}, errors.New("capture tmux inactive avant le démarrage de l'agent")
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
		return terminal.Info{}, fmt.Errorf("libération du helper tmux: %w", err)
	}
	cleanupFiles = false
	created = false
	return metadata, nil
}

func (m *Manager) emit(event session.Event, essential bool) bool {
	if essential {
		select {
		case m.events <- event:
			return true
		case <-m.ctx.Done():
			return false
		}
	}
	select {
	case m.events <- event:
		return true
	case <-m.ctx.Done():
		return false
	default:
		return false
	}
}

func (m *Manager) readOutput(target *managedSession, output *os.File) {
	defer m.wg.Done()
	err := target.interceptor.Run(target.ctx, output)
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
		deadSeen     bool
		deadSequence uint64
		deadSnapshot Snapshot
		pollFailures int
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
			pollFailures++
			if pollFailures >= 3 {
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
		pollFailures = 0
		target.updateState(snapshot)
		if snapshot.Running {
			deadSeen = false
			continue
		}
		sequence := target.outputSequenceValue()
		if !deadSeen || sequence != deadSequence {
			deadSeen = true
			deadSequence = sequence
			deadSnapshot = snapshot
			continue
		}
		m.finishSession(target, deadSnapshot)
		return
	}
}

func (m *Manager) handleMissingTarget(target *managedSession, cause error) {
	if !target.isPresent() {
		return
	}
	target.markRemoved()
	snapshot := Snapshot{ID: target.info.ID, Status: StatusFailed}
	target.updateState(snapshot)
	if target.finish() {
		missing := fmt.Errorf("%w: %v", ErrSessionNotFound, cause)
		m.emit(session.Error{SessionID: target.info.ID, Err: missing}, true)
		m.emit(session.Exited{SessionID: target.info.ID, Err: missing}, true)
	}
	target.closeTransport()
}

func (m *Manager) finishSession(target *managedSession, snapshot Snapshot) {
	target.updateState(snapshot)
	if !target.finish() {
		return
	}
	var exitErr error
	if snapshot.ExitCode != nil && *snapshot.ExitCode != 0 {
		exitErr = &ExitError{Code: *snapshot.ExitCode}
	}
	m.emit(session.Exited{SessionID: target.info.ID, Err: exitErr}, true)
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
	if target.interceptor == nil {
		return "", nil
	}
	return target.interceptor.Output(), nil
}

// PendingPrompt returns the cached interception state without invoking tmux.
// The TUI uses it to discard prompt events queued before an interactive
// attachment was resynchronized.
func (m *Manager) PendingPrompt(ctx context.Context, id string) (*terminal.Prompt, error) {
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
	prompt := target.prompt()
	if prompt == nil {
		return nil, nil
	}
	return &terminal.Prompt{
		Pattern:     prompt.Pattern,
		Description: prompt.Description,
		Match:       prompt.Match,
		Sensitive:   prompt.Sensitive,
	}, nil
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
	if _, err := m.verifyPane(ctx, target); err != nil {
		return err
	}

	bufferName := target.tmuxName + "-" + shortHash(target.ownerToken) + "-input"
	wasBlocked := target.interceptor.IsBlocked()
	pendingPrompt := target.prompt()
	if wasBlocked {
		target.interceptor.Acknowledge()
		target.clearPrompt()
	}
	if _, err := m.runInput(ctx, data, "load-buffer", "-b", bufferName, "-"); err != nil {
		if wasBlocked {
			target.interceptor.Reblock()
			if pendingPrompt != nil {
				target.setPrompt(*pendingPrompt)
			}
		}
		return err
	}
	m.trackSecretBuffer(bufferName)
	if _, err := m.run(ctx, "paste-buffer", "-d", "-b", bufferName, "-t", target.paneID); err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), commandTimeout)
		cleanupErr := m.deleteSecretBuffer(cleanupCtx, bufferName)
		cleanupCancel()
		if wasBlocked {
			target.interceptor.Reblock()
			if pendingPrompt != nil {
				target.setPrompt(*pendingPrompt)
			}
		}
		if cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("suppression du buffer tmux privé: %w", cleanupErr))
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
	windowID, err := m.verifyPane(ctx, target)
	if err != nil {
		return err
	}
	_, err = m.run(ctx,
		"resize-window", "-t", windowID,
		"-x", strconv.Itoa(size.Columns), "-y", strconv.Itoa(size.Rows),
	)
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
	target.updateState(snapshot)
	return target.snapshot(), nil
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
		return terminal.Snapshot{}, fmt.Errorf("restauration de la capture tmux: %w", err)
	}
	snapshot, pipeActive, err = m.inspectRaw(ctx, target)
	if err != nil {
		return terminal.Snapshot{}, err
	}
	if !pipeActive {
		return terminal.Snapshot{}, errors.New("capture tmux inactive après restauration")
	}
	return snapshot, nil
}

func (m *Manager) inspectRaw(ctx context.Context, target *managedSession) (terminal.Snapshot, bool, error) {
	if target.ownerToken == "" || !validTmuxID(target.sessionID, '$') || !validTmuxID(target.paneID, '%') {
		return terminal.Snapshot{}, false, errors.New("ownership tmux absent ou invalide")
	}
	output, err := m.run(ctx,
		"display-message", "-p", "-t", target.paneID,
		"#{session_id}\t#{pane_id}\t#{@relayer_owner}\t#{pane_dead}\t#{pane_dead_status}\t#{session_attached}\t#{pane_pipe}",
	)
	if err != nil {
		return terminal.Snapshot{}, false, err
	}
	fields := strings.Split(strings.TrimSpace(string(output)), "\t")
	if len(fields) != 7 {
		return terminal.Snapshot{}, false, errors.New("inspection tmux invalide")
	}
	if fields[0] != target.sessionID || fields[1] != target.paneID || fields[2] != target.ownerToken {
		return terminal.Snapshot{}, false, errors.New("ownership tmux invalide")
	}
	snapshot, err := parseSnapshot(target.info.ID, strings.Join(fields[3:6], "\t"))
	if err != nil {
		return terminal.Snapshot{}, false, err
	}
	pipe, err := strconv.Atoi(strings.TrimSpace(fields[6]))
	if err != nil || (pipe != 0 && pipe != 1) {
		return terminal.Snapshot{}, false, errors.New("indicateur pane_pipe tmux invalide")
	}
	return snapshot, pipe == 1, nil
}

func parseSnapshot(id, output string) (terminal.Snapshot, error) {
	fields := strings.Split(strings.TrimSpace(output), "\t")
	if len(fields) != 3 {
		return terminal.Snapshot{}, fmt.Errorf("état tmux invalide")
	}
	dead, err := strconv.Atoi(strings.TrimSpace(fields[0]))
	if err != nil || (dead != 0 && dead != 1) {
		return terminal.Snapshot{}, fmt.Errorf("indicateur pane_dead tmux invalide")
	}
	attached, err := strconv.Atoi(strings.TrimSpace(fields[2]))
	if err != nil || attached < 0 {
		return terminal.Snapshot{}, fmt.Errorf("indicateur session_attached tmux invalide")
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
	code, err := strconv.Atoi(strings.TrimSpace(fields[1]))
	if err != nil {
		return terminal.Snapshot{}, fmt.Errorf("code de sortie tmux invalide")
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
	fields := strings.Split(strings.TrimSpace(output), "\t")
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
		return errors.New("ownership de la session tmux absent ou invalide")
	}
	output, err := m.run(ctx,
		"display-message", "-p", "-t", target.sessionID,
		"#{session_id}\t#{@relayer_owner}",
	)
	if err != nil {
		return err
	}
	fields := strings.Split(strings.TrimSpace(string(output)), "\t")
	if len(fields) != 2 || fields[0] != target.sessionID || fields[1] != target.ownerToken {
		return errors.New("ownership de la session tmux invalide")
	}
	return nil
}

func (m *Manager) verifyPane(ctx context.Context, target *managedSession) (string, error) {
	if target.ownerToken == "" || !validTmuxID(target.sessionID, '$') || !validTmuxID(target.paneID, '%') {
		return "", errors.New("ownership du pane tmux absent ou invalide")
	}
	output, err := m.run(ctx,
		"display-message", "-p", "-t", target.paneID,
		"#{session_id}\t#{window_id}\t#{pane_id}\t#{@relayer_owner}",
	)
	if err != nil {
		return "", err
	}
	fields := strings.Split(strings.TrimSpace(string(output)), "\t")
	if len(fields) != 4 || fields[0] != target.sessionID || fields[2] != target.paneID || fields[3] != target.ownerToken || !validTmuxID(fields[1], '@') {
		return "", errors.New("ownership du pane tmux invalide")
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
	if err := m.verifySession(operationCtx, target); err != nil {
		return nil, err
	}
	attachCtx, cancelAttach := context.WithCancel(attachParent)
	stopManagerCancellation := context.AfterFunc(m.ctx, cancelAttach)
	target.beginAttach(cancelAttach, stopManagerCancellation)
	command := m.runner.Command(attachCtx, CommandSpec{
		Path: m.tmuxPath,
		Args: []string{"attach-session", "-t", target.sessionID},
	})
	if command == nil {
		target.endAttach()
		return nil, errors.New("CommandRunner a retourné une commande attach nil")
	}
	return command, nil
}

// Resync suppresses prompt events while tea.ExecProcess owns the terminal,
// then scans only the current last pane line. A prompt answered inside tmux is
// therefore not re-emitted merely because it remains in scrollback.
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
	var current *session.PromptDetected
	if target.isPresent() {
		snapshot, inspectErr := m.inspect(ctx, target)
		if inspectErr != nil {
			return inspectErr
		}
		target.updateState(snapshot)
		if snapshot.Running {
			current, err = m.currentPrompt(ctx, target)
			if err != nil {
				return err
			}
		} else {
			m.finishSession(target, snapshot)
		}
	}
	target.interceptor.Acknowledge()
	target.clearPrompt()
	if current != nil {
		target.interceptor.Reblock()
		target.setPrompt(*current)
		m.emit(*current, true)
	}
	m.emit(session.OutputAvailable{SessionID: id}, true)
	return nil
}

func (m *Manager) currentPrompt(ctx context.Context, target *managedSession) (*session.PromptDetected, error) {
	if _, err := m.verifyPane(ctx, target); err != nil {
		return nil, err
	}
	output, err := m.run(ctx,
		"capture-pane", "-p", "-J", "-t", target.paneID, "-S", "-8",
	)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimRight(string(output), " \t\r\n")
	if trimmed == "" {
		return nil, nil
	}
	if index := strings.LastIndexByte(trimmed, '\n'); index >= 0 {
		trimmed = trimmed[index+1:]
	}
	var detection *intercept.Detection
	probe, err := intercept.New(m.patterns, 16*1024, intercept.Hooks{
		OnPrompt: func(found intercept.Detection) {
			copy := found
			detection = &copy
		},
	})
	if err != nil {
		return nil, err
	}
	probe.Consume([]byte(trimmed))
	if detection == nil {
		return nil, nil
	}
	return &session.PromptDetected{
		SessionID:   target.info.ID,
		Pattern:     detection.Pattern,
		Description: detection.Description,
		Match:       detection.Match,
		Sensitive:   detection.Sensitive,
	}, nil
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
	target.updateState(terminal.Snapshot{
		ID:      target.info.ID,
		Status:  terminal.StatusExited,
		Running: false,
	})
	if target.finish() {
		m.emit(session.Exited{SessionID: target.info.ID}, true)
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
		if (m.persistOnExit && !target.forceCleanup) || !target.isPresent() {
			continue
		}
		target := target
		tasks = append(tasks, func(cleanupCtx context.Context) error {
			return m.killSession(cleanupCtx, target)
		})
	}
	for _, bufferName := range m.secretBuffers() {
		bufferName := bufferName
		tasks = append(tasks, func(cleanupCtx context.Context) error {
			if err := m.deleteSecretBuffer(cleanupCtx, bufferName); err != nil {
				return fmt.Errorf("suppression du buffer tmux privé: %w", err)
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

func (m *Manager) killSession(ctx context.Context, target *managedSession) error {
	target.killMu.Lock()
	defer target.killMu.Unlock()
	if !target.isPresent() {
		return nil
	}
	if target.sessionID == "" {
		return errors.New("session tmux sans identifiant immuable")
	}
	if err := m.verifySession(ctx, target); err != nil {
		return err
	}
	if _, err := m.run(ctx, "kill-session", "-t", target.sessionID); err != nil {
		return err
	}
	target.markRemoved()
	return nil
}

func isMissingTargetProbe(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var exitError *exec.ExitError
	return errors.As(err, &exitError)
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
