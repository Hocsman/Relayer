package tmuxbackend

import (
	"context"
	"sync"

	"github.com/Hocsman/Relayer/internal/intercept"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
)

type managedSession struct {
	info        terminal.Info
	tmuxName    string
	sessionID   string
	windowID    string
	paneID      string
	ownerToken  string
	ctx         context.Context
	cancel      context.CancelFunc
	interceptor *intercept.Interceptor
	files       *launchFiles
	done        chan struct{}

	inputMu sync.Mutex
	killMu  sync.Mutex
	mu      sync.RWMutex
	state   Snapshot
	exists  bool
	// forceCleanup marks a session created during a failed Start. Such a
	// session is never eligible for persistence because it was not accepted.
	forceCleanup bool
	// outputSequence lets the status monitor allow the FIFO reader one quiet
	// interval to consume final bytes before lifecycle cleanup.
	outputSequence uint64
	pendingPrompt  *session.PromptDetected
	attachActive   bool
	attachCancel   context.CancelFunc
	attachStop     func() bool
	exitEmitted    bool

	doneOnce      sync.Once
	transportOnce sync.Once
}

func (s *managedSession) outputObserved() {
	s.mu.Lock()
	s.outputSequence++
	s.mu.Unlock()
}

func (s *managedSession) outputSequenceValue() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.outputSequence
}

func (s *managedSession) snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := s.state
	if s.state.ExitCode != nil {
		code := *s.state.ExitCode
		result.ExitCode = &code
	}
	if s.interceptor != nil {
		result.Output = s.interceptor.Output()
	}
	if s.pendingPrompt != nil {
		result.Prompt = &terminal.Prompt{
			Pattern:     s.pendingPrompt.Pattern,
			Description: s.pendingPrompt.Description,
			Match:       s.pendingPrompt.Match,
			Sensitive:   s.pendingPrompt.Sensitive,
		}
	}
	return result
}

func (s *managedSession) updateState(state Snapshot) {
	s.mu.Lock()
	state.Output = ""
	s.state = state
	s.mu.Unlock()
}

func (s *managedSession) markRemoved() {
	s.mu.Lock()
	s.exists = false
	s.mu.Unlock()
}

func (s *managedSession) isPresent() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.exists
}

func (s *managedSession) setPrompt(prompt session.PromptDetected) (attachActive bool) {
	s.mu.Lock()
	copy := prompt
	s.pendingPrompt = &copy
	attachActive = s.attachActive
	s.mu.Unlock()
	return attachActive
}

func (s *managedSession) clearPrompt() {
	s.mu.Lock()
	s.pendingPrompt = nil
	s.mu.Unlock()
}

func (s *managedSession) prompt() *session.PromptDetected {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.pendingPrompt == nil {
		return nil
	}
	copy := *s.pendingPrompt
	return &copy
}

func (s *managedSession) beginAttach(cancel context.CancelFunc, stop func() bool) {
	s.mu.Lock()
	previousCancel := s.attachCancel
	previousStop := s.attachStop
	s.attachActive = true
	s.attachCancel = cancel
	s.attachStop = stop
	s.mu.Unlock()
	if previousStop != nil {
		previousStop()
	}
	if previousCancel != nil {
		previousCancel()
	}
}

func (s *managedSession) endAttach() {
	s.mu.Lock()
	s.attachActive = false
	cancel := s.attachCancel
	stop := s.attachStop
	s.attachCancel = nil
	s.attachStop = nil
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
	if cancel != nil {
		cancel()
	}
}

func (s *managedSession) finish() bool {
	emitted := false
	s.mu.Lock()
	if !s.exitEmitted {
		s.exitEmitted = true
		emitted = true
	}
	s.mu.Unlock()
	s.doneOnce.Do(func() { close(s.done) })
	return emitted
}

func (s *managedSession) closeTransport() {
	s.transportOnce.Do(func() {
		s.endAttach()
		if s.cancel != nil {
			s.cancel()
		}
		if s.files != nil {
			s.files.close()
			s.files.remove()
		}
	})
}
