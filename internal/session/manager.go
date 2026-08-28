package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/intercept"
	"github.com/Hocsman/Relayer/internal/platform"
	"github.com/creack/pty"
)

// Manager is the sole owner of process lifecycles and PTY descriptors.
type Manager struct {
	ctx    context.Context
	cancel context.CancelFunc
	events chan<- Event

	mu           sync.RWMutex
	sessions     map[string]*processSession
	closed       bool
	registry     *adapters.Registry
	ringCapacity int
	wg           sync.WaitGroup
	closeOnce    sync.Once
}

// NewManager validates every prompt pattern before any process can start.
func NewManager(
	parent context.Context,
	events chan<- Event,
	patterns []intercept.Pattern,
	ringCapacity int,
) (*Manager, error) {
	registry, err := adapters.NewRegistry(patterns)
	if err != nil {
		return nil, err
	}
	return NewManagerWithRegistry(parent, events, registry, ringCapacity)
}

// NewManagerWithRegistry creates a PTY owner around the application adapter
// registry. The registry is validated before any process can start and creates
// independent adapter state for every session.
func NewManagerWithRegistry(
	parent context.Context,
	events chan<- Event,
	registry *adapters.Registry,
	ringCapacity int,
) (*Manager, error) {
	if parent == nil {
		parent = context.Background()
	}
	if events == nil {
		return nil, errors.New("nil session event channel")
	}
	if registry == nil {
		return nil, errors.New("nil adapter registry")
	}
	if _, _, err := registry.Resolve(adapters.GenericID, ""); err != nil {
		return nil, fmt.Errorf("invalid adapter registry: %w", err)
	}

	ctx, cancel := context.WithCancel(parent)
	return &Manager{
		ctx:          ctx,
		cancel:       cancel,
		events:       events,
		sessions:     make(map[string]*processSession),
		registry:     registry,
		ringCapacity: ringCapacity,
	}, nil
}

func (m *Manager) Context() context.Context {
	return m.ctx
}

// Name identifies the concrete backend without exposing PTY implementation
// details to the application or TUI.
func (m *Manager) Name() string {
	return agent.BackendPTY
}

func (m *Manager) emit(event Event, essential bool) bool {
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

// Start validates and launches one direct or explicitly requested shell
// command with a correctly sized PTY.
func (m *Manager) Start(spec agent.Spec, columns, rows int) (Info, error) {
	normalized, err := agent.ValidateSpec(spec, ".", agent.BackendPTY)
	if err != nil {
		return Info{}, fmt.Errorf("invalid agent: %w", err)
	}
	if normalized.Backend != agent.BackendPTY {
		return Info{}, fmt.Errorf("backend %q not supported by the PTY manager", normalized.Backend)
	}
	executable := ""
	if normalized.Shell == "" && len(normalized.Command) > 0 {
		executable = normalized.Command[0]
	}
	selectedAdapter, descriptor, err := m.registry.Resolve(normalized.Adapter, executable)
	if err != nil {
		return Info{}, fmt.Errorf("resolving the adapter for %s: %w", normalized.Name, err)
	}
	normalized.Adapter = descriptor.ID

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Info{}, ErrClosed
	}
	for existingID := range m.sessions {
		if strings.EqualFold(existingID, normalized.ID) {
			return Info{}, fmt.Errorf("session %q already started", normalized.ID)
		}
	}

	sessionCtx, sessionCancel := context.WithCancel(m.ctx)
	cmd, shell, err := newCommand(sessionCtx, normalized)
	if err != nil {
		sessionCancel()
		return Info{}, fmt.Errorf("preparing %s: %w", normalized.Name, err)
	}
	sessionID := normalized.ID
	info := Info{
		ID:             sessionID,
		Name:           normalized.Name,
		DisplayCommand: displayCommand(normalized),
		Backend:        agent.BackendPTY,
		Adapter:        descriptor.ID,
		Shell:          shell,
	}
	session := &processSession{
		info:     info,
		cmd:      cmd,
		ctx:      sessionCtx,
		cancel:   sessionCancel,
		done:     make(chan struct{}),
		readDone: make(chan struct{}),
	}
	processor, err := adapters.NewProcessor(
		selectedAdapter,
		adapters.NewDetectionState(sessionID, normalized.ID, descriptor.ID),
		m.ringCapacity,
		adapters.Hooks{
			OnOutput: func() {
				m.emit(OutputAvailable{SessionID: sessionID}, false)
			},
			OnEvent: func(event adapters.Event) {
				m.emit(AdapterEvent{Event: event.Clone()}, true)
			},
		},
	)
	if err != nil {
		sessionCancel()
		return Info{}, err
	}
	session.processor = processor

	master, err := pty.StartWithSize(cmd, &pty.Winsize{
		Rows: uint16(clamp(rows, 1, 65535)),
		Cols: uint16(clamp(columns, 1, 65535)),
	})
	if err != nil {
		sessionCancel()
		return Info{}, fmt.Errorf("starting %s: %w", normalized.Name, err)
	}
	session.master = master
	m.sessions[sessionID] = session

	m.wg.Add(2)
	// Capture the descriptor before publishing the reader. Close may set the
	// field to nil while closing this same os.File to unblock the read.
	go m.readSession(session, master)
	go m.waitSession(session)
	return info, nil
}

func (m *Manager) readSession(session *processSession, master *os.File) {
	defer m.wg.Done()

	err := session.processor.Run(session.ctx, master)
	// Publish the drain barrier before sending the final invalidation. The
	// output bytes are already retained in Processor; a full UI event channel
	// must not prevent waitSession from closing Done and publishing lifecycle.
	session.closePTY()
	close(session.readDone)
	// This essential final invalidation exposes a last chunk even when earlier
	// non-essential output events were coalesced.
	m.emit(OutputAvailable{SessionID: session.info.ID}, true)
	if err != nil && !isExpectedPTYError(err) && session.ctx.Err() == nil {
		m.emit(Error{SessionID: session.info.ID, Err: err}, true)
	}
}

func (m *Manager) waitSession(session *processSession) {
	defer m.wg.Done()
	err := session.cmd.Wait() // The sole Wait call for a successfully started command.
	// Wait is the first authoritative proof that the process can no longer
	// consume input. Mark the Processor terminated immediately, under the same
	// lock as SendLine, before publishing Result or spending time cleaning up
	// descendants. This closes the check-then-write window at process exit.
	exitEvent := markProcessExitEvent(session, err)
	session.setResult(err)

	// The shell may exit while descendants still own the slave PTY.
	platform.TerminateProcessGroup(session.cmd)
	if platform.ProcessGroupExists(session.cmd) {
		timer := time.NewTimer(descendantGraceTime)
		select {
		case <-timer.C:
		case <-m.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		platform.KillProcessGroup(session.cmd)
	}

	// cmd.Wait may win before the PTY reader has consumed the final kernel
	// buffer. Give the normal EOF path a short bounded drain window, then close
	// the master to guarantee progress if a detached descendant retained the
	// slave outside the original process group.
	if !waitForSignal(session.readDone, finalOutputDrainTime) {
		session.closePTY()
		<-session.readDone
	}
	close(session.done)
	// An actionable event detected before Wait must still reach its hook before
	// process_exit. Waiting here cannot delay descendant termination or Done.
	session.processor.WaitSemanticEvents()
	if m.ctx.Err() == nil {
		m.emit(AdapterEvent{Event: exitEvent}, true)
	}
}

// Result returns a stable lifecycle snapshot once Wait has completed.
func (m *Manager) Result(sessionID string) (exited bool, waitErr error, exitCode *int, err error) {
	session, err := m.session(sessionID)
	if err != nil {
		return false, nil, nil, err
	}
	exited, exitCode, waitErr = session.result()
	return exited, waitErr, exitCode, nil
}

func (m *Manager) session(sessionID string) (*processSession, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for existingID, session := range m.sessions {
		if strings.EqualFold(existingID, sessionID) {
			return session, nil
		}
	}
	return nil, fmt.Errorf("unknown session %q", sessionID)
}

func (m *Manager) SendInput(sessionID string, value string) error {
	return m.SendData(sessionID, []byte(value+"\r"))
}

// SendLine is the ordinary-input path. Processor.SendLine atomically confirms
// that the process is live and no actionable event is pending, then appends
// exactly one carriage return without acknowledging semantic state.
func (m *Manager) SendLine(ctx context.Context, sessionID, line string) error {
	session, err := m.session(sessionID)
	if err != nil {
		return err
	}
	knownClosed := false
	err = session.processor.SendLine(ctx, line, func(data []byte) error {
		// Wait publishes the result before descendant cleanup. Once that fact is
		// known, ordinary input must perform zero PTY I/O even if the master file
		// has not been closed yet.
		if exited, _, _ := session.result(); exited {
			knownClosed = true
			return ErrClosed
		}
		writeErr := session.write(data)
		if errors.Is(writeErr, ErrClosed) {
			knownClosed = true
		}
		return writeErr
	})
	if knownClosed {
		return ErrClosed
	}
	if errors.Is(err, adapters.ErrProcessorTerminated) {
		return ErrClosed
	}
	return err
}

// SendData is the compatibility path for callers that do not yet retain the
// actionable event ID. It still uses Processor.Resolve so acknowledgement only
// happens after the exact bytes have been delivered successfully.
func (m *Manager) SendData(sessionID string, data []byte) error {
	return m.SendDataForEvent(sessionID, "", data)
}

// SendDataForEvent atomically validates the pending occurrence, delivers the
// exact bytes and acknowledges only after a successful write.
func (m *Manager) SendDataForEvent(sessionID, eventID string, data []byte) error {
	session, err := m.session(sessionID)
	if err != nil {
		return err
	}
	err = session.processor.Resolve(eventID, func() error { return session.write(data) })
	if errors.Is(err, adapters.ErrProcessorTerminated) {
		return ErrClosed
	}
	return err
}

// Stop terminates one owned session without affecting its siblings.
func (m *Manager) Stop(sessionID string) error {
	session, err := m.session(sessionID)
	if err != nil {
		return err
	}
	session.requestStop()
	return session.waitForStop()
}

func (m *Manager) Output(sessionID string) (string, error) {
	session, err := m.session(sessionID)
	if err != nil {
		return "", err
	}
	return session.processor.Output(), nil
}

// PendingEvent returns an independent copy of the actionable occurrence still
// awaiting a decision, if any.
func (m *Manager) PendingEvent(sessionID string) (*adapters.Event, error) {
	session, err := m.session(sessionID)
	if err != nil {
		return nil, err
	}
	return session.processor.Pending(), nil
}

// Revision is the latest semantic event occurrence sequence for the session.
func (m *Manager) Revision(sessionID string) (uint64, error) {
	session, err := m.session(sessionID)
	if err != nil {
		return 0, err
	}
	return session.processor.Revision(), nil
}

// Done exposes lifecycle completion without leaking the underlying process or
// PTY descriptor. The channel is closed before the corresponding process_exit
// AdapterEvent is emitted.
func (m *Manager) Done(sessionID string) (<-chan struct{}, error) {
	session, err := m.session(sessionID)
	if err != nil {
		return nil, err
	}
	return session.done, nil
}

func (m *Manager) Resize(sessionID string, columns, rows int) error {
	session, err := m.session(sessionID)
	if err != nil {
		return err
	}
	return session.resize(columns, rows)
}

// BeginShutdown immediately unblocks essential event senders. Close performs
// descriptor/process cleanup and joins every owned goroutine.
func (m *Manager) BeginShutdown() {
	m.cancel()
}

func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		sessions := make([]*processSession, 0, len(m.sessions))
		for _, session := range m.sessions {
			sessions = append(sessions, session)
		}
		m.mu.Unlock()

		m.cancel()
		for _, session := range sessions {
			session.requestStop()
		}
		for _, session := range sessions {
			_ = session.waitForStop()
		}
		m.wg.Wait()
	})
}

func isExpectedPTYError(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, os.ErrClosed) ||
		platform.IsPTYCloseError(err)
}

func newCommand(ctx context.Context, spec agent.Spec) (*exec.Cmd, bool, error) {
	var (
		command *exec.Cmd
		err     error
		shell   bool
	)
	if spec.Shell != "" {
		command, err = platform.NewShellCommand(ctx, spec.Shell)
		shell = true
	} else {
		command = exec.CommandContext(ctx, spec.Command[0], spec.Command[1:]...)
	}
	if err != nil {
		return nil, false, err
	}
	command.Dir = spec.Cwd
	command.Env = mergedEnvironment(spec.Env)
	return command, shell, nil
}

func mergedEnvironment(overrides map[string]string) []string {
	values := make(map[string]string, len(os.Environ())+len(overrides)+1)
	for _, assignment := range os.Environ() {
		name, value, found := strings.Cut(assignment, "=")
		if found {
			values[name] = value
		}
	}
	values["TERM"] = "xterm-256color"
	for name, value := range overrides {
		values[name] = value
	}

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	environment := make([]string, 0, len(names))
	for _, name := range names {
		environment = append(environment, name+"="+values[name])
	}
	return environment
}

func displayCommand(spec agent.Spec) string {
	if spec.Shell != "" {
		// Shell scripts commonly embed credentials or substitutions. Consumers
		// only need to know that interpreted mode is active; never expose the
		// script through session metadata or persistent TUI history.
		return "[explicit shell]"
	}
	parts := make([]string, len(spec.Command))
	for index, argument := range spec.Command {
		parts[index] = strconv.Quote(argument)
	}
	return strings.Join(parts, " ")
}

func markProcessExitEvent(session *processSession, waitErr error) adapters.Event {
	var exitCode *int
	if session.cmd != nil && session.cmd.ProcessState != nil {
		code := session.cmd.ProcessState.ExitCode()
		exitCode = &code
	}
	return session.processor.MarkProcessExitEvent(exitCode, waitErr != nil)
}
