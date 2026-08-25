package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

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
	sessions     map[int]*processSession
	nextID       int
	closed       bool
	patterns     []intercept.Pattern
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
	if parent == nil {
		parent = context.Background()
	}
	if events == nil {
		return nil, errors.New("canal d'événements de session nil")
	}
	if _, err := intercept.New(patterns, ringCapacity, intercept.Hooks{}); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(parent)
	return &Manager{
		ctx:          ctx,
		cancel:       cancel,
		events:       events,
		sessions:     make(map[int]*processSession),
		patterns:     append([]intercept.Pattern(nil), patterns...),
		ringCapacity: ringCapacity,
	}, nil
}

func (m *Manager) Context() context.Context {
	return m.ctx
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

// Start launches command through /bin/sh with a correctly sized PTY.
func (m *Manager) Start(name, command string, columns, rows int) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Info{}, ErrClosed
	}

	sessionCtx, sessionCancel := context.WithCancel(m.ctx)
	cmd := exec.CommandContext(sessionCtx, "/bin/sh", "-c", command)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	master, err := pty.StartWithSize(cmd, &pty.Winsize{
		Rows: uint16(clamp(rows, 1, 65535)),
		Cols: uint16(clamp(columns, 1, 65535)),
	})
	if err != nil {
		sessionCancel()
		return Info{}, fmt.Errorf("démarrage de %s: %w", name, err)
	}

	sessionID := m.nextID
	m.nextID++
	interceptor, err := intercept.New(
		m.patterns,
		m.ringCapacity,
		intercept.Hooks{
			OnOutput: func() {
				m.emit(OutputAvailable{SessionID: sessionID}, false)
			},
			OnPrompt: func(detection intercept.Detection) {
				m.emit(PromptDetected{
					SessionID:   sessionID,
					Pattern:     detection.Pattern,
					Description: detection.Description,
					Match:       detection.Match,
					Sensitive:   detection.Sensitive,
				}, true)
			},
		},
	)
	if err != nil {
		_ = master.Close()
		sessionCancel()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return Info{}, err
	}

	info := Info{ID: sessionID, Name: name, Command: command}
	session := &processSession{
		info:        info,
		cmd:         cmd,
		ctx:         sessionCtx,
		cancel:      sessionCancel,
		interceptor: interceptor,
		done:        make(chan struct{}),
		master:      master,
	}
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
	defer session.closePTY()

	err := session.interceptor.Run(session.ctx, master)
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

	close(session.done)
	if m.ctx.Err() == nil {
		m.emit(Exited{SessionID: session.info.ID, Err: err}, true)
	}
}

func (m *Manager) session(sessionID int) (*processSession, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, ok := m.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("session %d inconnue", sessionID)
	}
	return session, nil
}

func (m *Manager) SendInput(sessionID int, value string) error {
	session, err := m.session(sessionID)
	if err != nil {
		return err
	}

	// Rearm first so a second prompt emitted immediately after the write cannot
	// be discarded as a duplicate of the first one.
	session.interceptor.Acknowledge()
	if err := session.write(value + "\r"); err != nil {
		session.interceptor.Reblock()
		return err
	}
	return nil
}

func (m *Manager) Output(sessionID int) (string, error) {
	session, err := m.session(sessionID)
	if err != nil {
		return "", err
	}
	return session.interceptor.Output(), nil
}

// Done exposes lifecycle completion without leaking the underlying process or
// PTY descriptor. The channel is closed before the corresponding Exited event
// is emitted.
func (m *Manager) Done(sessionID int) (<-chan struct{}, error) {
	session, err := m.session(sessionID)
	if err != nil {
		return nil, err
	}
	return session.done, nil
}

func (m *Manager) Resize(sessionID, columns, rows int) error {
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
			session.waitForStop()
		}
		m.wg.Wait()
	})
}

func isExpectedPTYError(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, os.ErrClosed) ||
		platform.IsPTYCloseError(err)
}
