package tmuxbackend

import (
	"context"
	"sync"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/terminal"
)

type managedSession struct {
	info       terminal.Info
	tmuxName   string
	sessionID  string
	windowID   string
	paneID     string
	ownerToken string
	ctx        context.Context
	cancel     context.CancelFunc
	processor  *adapters.Processor
	files      *launchFiles
	done       chan struct{}

	inputMu       sync.Mutex
	interactionMu sync.Mutex
	resizeMu      sync.Mutex
	killMu        sync.Mutex
	mu            sync.RWMutex
	state         Snapshot
	revision      uint64
	exists        bool
	// forceCleanup marks a session created during a failed Start. Such a
	// session is never eligible for persistence because it was not accepted.
	forceCleanup bool
	// outputSequence lets the status monitor allow the FIFO reader one quiet
	// interval to consume final bytes before lifecycle cleanup.
	outputSequence   uint64
	lastEmittedEvent string
	eventSequence    uint64
	attachActive     bool
	attachCancel     context.CancelFunc
	attachStop       func() bool
	exitEmitted      bool
	captureDisabled  bool
	appliedSize      terminal.Size
	sizeKnown        bool

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
	result := s.state
	if s.state.ExitCode != nil {
		code := *s.state.ExitCode
		result.ExitCode = &code
	}
	result.Revision = s.revision
	processor := s.processor
	s.mu.RUnlock()
	if processor != nil {
		result.Output = processor.Output()
		if result.Running {
			result.Pending = processor.Pending()
			if processorRevision := processor.Revision(); processorRevision > result.Revision {
				result.Revision = processorRevision
			}
		} else {
			result.Pending = nil
		}
	}
	return result
}

func (s *managedSession) pendingEvent() *adapters.Event {
	s.mu.RLock()
	running := s.state.Running && s.exists
	processor := s.processor
	s.mu.RUnlock()
	if !running || processor == nil {
		return nil
	}
	return processor.Pending()
}

func (s *managedSession) updateState(state Snapshot) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	state.Output = ""
	state.Pending = nil
	state.Revision = 0
	if sameLifecycleState(s.state, state) {
		return false
	}
	s.state = state
	return true
}

func sameLifecycleState(left, right Snapshot) bool {
	if left.ID != right.ID || left.Status != right.Status || left.Running != right.Running || left.Attached != right.Attached {
		return false
	}
	if left.ExitCode == nil || right.ExitCode == nil {
		return left.ExitCode == nil && right.ExitCode == nil
	}
	return *left.ExitCode == *right.ExitCode
}

// claimAdapterEvent suppresses duplicates by occurrence ID. Events discovered
// while the real terminal is attached remain authoritative in Processor but
// are emitted only once Resync has reconciled the visible tmux screen.
func (s *managedSession) claimAdapterEvent(event adapters.Event) bool {
	if event.ID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if event.Sequence > s.eventSequence {
		s.eventSequence = event.Sequence
	}
	if !s.state.Running || s.exitEmitted || s.attachActive || s.lastEmittedEvent == event.ID {
		return false
	}
	s.lastEmittedEvent = event.ID
	return true
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

func (s *managedSession) captureNeedsDisable() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.exists && !s.captureDisabled
}

func (s *managedSession) markCaptureDisabled() {
	s.mu.Lock()
	s.captureDisabled = true
	s.mu.Unlock()
}

func (s *managedSession) beginAttach(cancel context.CancelFunc, stop func() bool) bool {
	s.mu.Lock()
	if s.attachActive {
		s.mu.Unlock()
		return false
	}
	s.attachActive = true
	s.attachCancel = cancel
	s.attachStop = stop
	s.mu.Unlock()
	return true
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

func (s *managedSession) sizeApplied(size terminal.Size) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sizeKnown && s.appliedSize == size
}

func (s *managedSession) recordAppliedSize(size terminal.Size) {
	s.mu.Lock()
	s.appliedSize = size
	s.sizeKnown = true
	s.mu.Unlock()
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

func (s *managedSession) processExitEvent(snapshot Snapshot) adapters.Event {
	event := s.processor.NewProcessExitEvent(
		snapshot.ExitCode,
		snapshot.Status == terminal.StatusFailed,
	)
	s.mu.Lock()
	if event.Sequence > s.eventSequence {
		s.eventSequence = event.Sequence
	}
	if event.Sequence > s.revision {
		s.revision = event.Sequence
	}
	s.mu.Unlock()
	return event
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
