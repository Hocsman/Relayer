// Package ptybackend adapts Relayer's established PTY session manager to the
// context-aware terminal.Backend contract.
package ptybackend

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/intercept"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
)

type Manager struct {
	inner *session.Manager

	mu       sync.RWMutex
	ownedIDs map[string]struct{}
}

var _ terminal.Backend = (*Manager)(nil)
var _ terminal.EventSender = (*Manager)(nil)
var _ terminal.LineSender = (*Manager)(nil)
var _ terminal.PendingEventProvider = (*Manager)(nil)

func New(
	parent context.Context,
	events chan<- session.Event,
	patterns []intercept.Pattern,
	ringCapacity int,
) (*Manager, error) {
	registry, err := adapters.NewRegistry(patterns)
	if err != nil {
		return nil, err
	}
	return NewWithRegistry(parent, events, registry, ringCapacity)
}

// NewWithRegistry constructs a PTY backend using the application adapter
// registry. New remains the source-compatible legacy-pattern wrapper.
func NewWithRegistry(
	parent context.Context,
	events chan<- session.Event,
	registry *adapters.Registry,
	ringCapacity int,
) (*Manager, error) {
	inner, err := session.NewManagerWithRegistry(parent, events, registry, ringCapacity)
	if err != nil {
		return nil, err
	}
	return &Manager{inner: inner, ownedIDs: make(map[string]struct{})}, nil
}

func (m *Manager) Name() string { return agent.BackendPTY }

func (m *Manager) Start(ctx context.Context, spec agent.Spec, size terminal.Size) (terminal.Info, error) {
	if err := contextError(ctx); err != nil {
		return terminal.Info{}, err
	}
	size = size.Normalize()
	info, err := m.inner.Start(spec, size.Columns, size.Rows)
	if err != nil {
		return terminal.Info{}, &terminal.OperationError{Backend: m.Name(), Operation: "start", SessionID: spec.ID, Err: err}
	}
	m.mu.Lock()
	m.ownedIDs[strings.ToLower(info.ID)] = struct{}{}
	m.mu.Unlock()
	return terminal.Info{
		ID:             info.ID,
		Name:           info.Name,
		DisplayCommand: info.DisplayCommand,
		Backend:        agent.BackendPTY,
		Adapter:        info.Adapter,
		Shell:          info.Shell,
	}, nil
}

func (m *Manager) Send(ctx context.Context, id terminal.SessionID, data []byte) error {
	return m.SendEvent(ctx, id, "", data)
}

// SendLine uses the Processor's ordinary-input CAS. It never falls back to
// SendDataForEvent and therefore cannot resolve or acknowledge a prompt.
func (m *Manager) SendLine(ctx context.Context, id terminal.SessionID, line string) error {
	if err := m.check(ctx, id); err != nil {
		return err
	}
	if err := m.inner.SendLine(ctx, id, line); err != nil {
		if errors.Is(err, session.ErrClosed) {
			err = terminal.ErrClosed
		}
		return &terminal.OperationError{Backend: m.Name(), Operation: "send_line", SessionID: id, Err: err}
	}
	return nil
}

// SendEvent resolves one pending adapter event and transmits data exactly as
// supplied. An empty eventID preserves the historical Send behavior.
func (m *Manager) SendEvent(ctx context.Context, id terminal.SessionID, eventID string, data []byte) error {
	if err := m.check(ctx, id); err != nil {
		return err
	}
	if err := m.inner.SendDataForEvent(id, eventID, data); err != nil {
		return &terminal.OperationError{Backend: m.Name(), Operation: "send", SessionID: id, Err: err}
	}
	return nil
}

func (m *Manager) Resize(ctx context.Context, id terminal.SessionID, size terminal.Size) error {
	if err := m.check(ctx, id); err != nil {
		return err
	}
	size = size.Normalize()
	if err := m.inner.Resize(id, size.Columns, size.Rows); err != nil {
		return &terminal.OperationError{Backend: m.Name(), Operation: "resize", SessionID: id, Err: err}
	}
	return nil
}

func (m *Manager) Snapshot(ctx context.Context, id terminal.SessionID) (terminal.Snapshot, error) {
	if err := m.check(ctx, id); err != nil {
		return terminal.Snapshot{}, err
	}
	output, err := m.inner.Output(id)
	if err != nil {
		return terminal.Snapshot{}, &terminal.OperationError{Backend: m.Name(), Operation: "snapshot", SessionID: id, Err: err}
	}
	exited, waitErr, exitCode, err := m.inner.Result(id)
	if err != nil {
		return terminal.Snapshot{}, &terminal.OperationError{Backend: m.Name(), Operation: "snapshot", SessionID: id, Err: err}
	}
	status := terminal.StatusRunning
	if exited {
		status = terminal.StatusExited
		if waitErr != nil {
			status = terminal.StatusFailed
		}
	}
	pending, err := m.inner.PendingEvent(id)
	if err != nil {
		return terminal.Snapshot{}, &terminal.OperationError{Backend: m.Name(), Operation: "snapshot", SessionID: id, Err: err}
	}
	if exited {
		// A completed process cannot accept a human decision even if its final
		// output happened to look like a prompt while the PTY was draining.
		pending = nil
	}
	revision, err := m.inner.Revision(id)
	if err != nil {
		return terminal.Snapshot{}, &terminal.OperationError{Backend: m.Name(), Operation: "snapshot", SessionID: id, Err: err}
	}
	return terminal.Snapshot{
		ID:       id,
		Status:   status,
		Running:  !exited,
		ExitCode: exitCode,
		Output:   output,
		Pending:  pending,
		Revision: revision,
	}, nil
}

// PendingEvent returns cached adapter state without performing terminal I/O.
func (m *Manager) PendingEvent(ctx context.Context, id terminal.SessionID) (*adapters.Event, error) {
	if err := m.check(ctx, id); err != nil {
		return nil, err
	}
	return m.inner.PendingEvent(id)
}

// Output returns the processor's in-memory ring without any process query.
func (m *Manager) Output(id string) (string, error) {
	if err := m.check(context.Background(), id); err != nil {
		return "", err
	}
	return m.inner.Output(id)
}

func (m *Manager) AttachCommand(context.Context, terminal.SessionID) (*exec.Cmd, error) {
	return nil, fmt.Errorf("%w: backend %s", terminal.ErrNotAttachable, m.Name())
}

func (m *Manager) Stop(ctx context.Context, id terminal.SessionID) error {
	if err := m.check(ctx, id); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- m.inner.Stop(id) }()
	select {
	case err := <-done:
		if err != nil {
			return &terminal.OperationError{Backend: m.Name(), Operation: "stop", SessionID: id, Err: err}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.inner.BeginShutdown()
	done := make(chan struct{})
	go func() {
		m.inner.Close()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Context exposes the owner cancellation to the narrow Bubble Tea adapter.
func (m *Manager) Context() context.Context { return m.inner.Context() }

// BeginShutdown is intentionally non-blocking so Bubble Tea can quit before
// Close joins process and reader goroutines.
func (m *Manager) BeginShutdown() { m.inner.BeginShutdown() }

func (m *Manager) check(ctx context.Context, id string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	m.mu.RLock()
	_, exists := m.ownedIDs[strings.ToLower(id)]
	m.mu.RUnlock()
	if !exists {
		return fmt.Errorf("%w: %s", terminal.ErrSessionNotFound, id)
	}
	select {
	case <-m.inner.Context().Done():
		return terminal.ErrClosed
	default:
		return nil
	}
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func IsClosed(err error) bool {
	return errors.Is(err, terminal.ErrClosed) || errors.Is(err, session.ErrClosed)
}
