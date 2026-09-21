package session

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/platform"
)

// ErrClosed is returned when an operation targets a PTY that has already been
// closed or a manager that no longer accepts sessions.
var (
	ErrClosed = errors.New("PTY session closed")
	// ErrStopUncertain means Relayer requested termination but could not confirm
	// that both the command leader and its PTY process group disappeared. A
	// caller must not start a replacement process while this error is present.
	ErrStopUncertain = errors.New("PTY session stop not confirmed")
)

const (
	gracefulStopTimeout  = 1500 * time.Millisecond
	forcedStopTimeout    = 500 * time.Millisecond
	descendantGraceTime  = 250 * time.Millisecond
	finalOutputDrainTime = 100 * time.Millisecond
	groupCheckInterval   = 10 * time.Millisecond
)

// processSession is deliberately private: Manager remains the only owner of
// process handles, PTY descriptors, and adapter processor state.
type processSession struct {
	info      Info
	cmd       *exec.Cmd
	ctx       context.Context
	cancel    context.CancelFunc
	processor *adapters.Processor
	done      chan struct{}
	readDone  chan struct{}
	resultMu  sync.RWMutex
	exited    bool
	waitErr   error

	fileMu       sync.RWMutex
	device       ptyDevice
	closePTYOnce sync.Once
	stopOnce     sync.Once

	// These hooks default to the platform process-group primitives. Keeping
	// them per session makes the negative confirmation paths deterministic in
	// tests without mutating package globals used by concurrent sessions.
	killGroup   func(*exec.Cmd)
	groupExists func(*exec.Cmd) bool

	// recordInput and recordResize are nil unless a transcript is being
	// written. They are set once, before the session is published, so no lock
	// guards them. Both are called after the operation succeeded and outside
	// fileMu, for the reason the resize comment below spells out.
	recordInput  func(at time.Time, data []byte)
	recordResize func(at time.Time, columns, rows int)
}

func (s *processSession) setResult(err error) {
	s.resultMu.Lock()
	s.exited = true
	s.waitErr = err
	s.resultMu.Unlock()
}

func (s *processSession) result() (bool, *int, error) {
	s.resultMu.RLock()
	defer s.resultMu.RUnlock()
	if !s.exited {
		return false, nil, nil
	}
	var exitCode *int
	if s.cmd != nil && s.cmd.ProcessState != nil {
		code := s.cmd.ProcessState.ExitCode()
		exitCode = &code
	}
	return true, exitCode, s.waitErr
}

func (s *processSession) write(input []byte) error {
	s.fileMu.RLock()
	device := s.device
	s.fileMu.RUnlock()
	if device == nil {
		return ErrClosed
	}

	// The device permits Close concurrently with Write. Do not retain fileMu while
	// writing: a saturated PTY must be unblocked by Close during shutdown.
	_, err := device.Write(input)
	// Recorded only once the bytes reached the device, and on a copy: SendLine
	// and Resolve both pass buffers they continue to own.
	if err == nil && s.recordInput != nil {
		s.recordInput(time.Now(), append([]byte(nil), input...))
	}
	return err
}

func (s *processSession) resize(columns, rows int) error {
	columns = clamp(columns, 1, 65535)
	rows = clamp(rows, 1, 65535)

	// The rendered screen is told first, and outside fileMu.
	//
	// Holding fileMu here while taking the processor lock inverts the order the
	// rest of this file uses: Resolve and SendLine hold the processor lock and
	// then call write, which takes fileMu. With closePTY's writer waiting
	// between them, every later RLock queues behind it and the two paths wait
	// on each other. The comment below already states the rule for writing;
	// this is the same rule.
	if s.processor != nil {
		s.processor.Resize(columns, rows)
	}

	// Unlike write, the resize ioctl is issued while holding fileMu. Releasing
	// it first and resizing afterwards lets closePTY close the descriptor
	// underneath the ioctl, and a closed fd can already have been reused: the
	// resize would then land on an unrelated file. write can release the lock
	// because a saturated write has to be unblockable by Close; an ioctl never
	// blocks, so it has no such need.
	s.fileMu.RLock()
	if s.device == nil {
		s.fileMu.RUnlock()
		return ErrClosed
	}
	err := s.device.Resize(columns, rows)
	s.fileMu.RUnlock()

	// Recorded after the unlock: the recorder is another subsystem with its own
	// lock, and taking it under fileMu inverts the order closePTY's writer
	// depends on.
	if err == nil && s.recordResize != nil {
		s.recordResize(time.Now(), columns, rows)
	}
	return err
}

func (s *processSession) closePTY() {
	s.closePTYOnce.Do(func() {
		s.fileMu.Lock()
		device := s.device
		s.device = nil
		s.fileMu.Unlock()
		if device != nil {
			_ = device.Close()
		}
	})
}

func (s *processSession) requestStop() {
	s.stopOnce.Do(func() {
		select {
		case <-s.done:
			s.cancel()
			s.closePTY()
			return
		default:
		}

		platform.TerminateProcessGroup(s.cmd)
	})
}

func (s *processSession) waitForStop() error {
	return s.waitForStopWithin(gracefulStopTimeout, forcedStopTimeout)
}

func (s *processSession) waitForStopWithin(gracefulTimeout, forcedTimeout time.Duration) error {
	if waitForSignal(s.done, gracefulTimeout) {
		if s.cancel != nil {
			s.cancel()
		}
		s.closePTY()
		return s.confirmProcessGroupStopped(forcedTimeout)
	}

	if s.cancel != nil {
		s.cancel()
	}
	s.closePTY()
	s.killProcessGroup()
	if !waitForSignal(s.done, forcedTimeout) {
		return ErrStopUncertain
	}
	return s.confirmProcessGroupStopped(forcedTimeout)
}

func (s *processSession) confirmProcessGroupStopped(timeout time.Duration) error {
	if !s.processGroupExists() {
		return nil
	}
	s.killProcessGroup()
	if timeout <= 0 {
		if s.processGroupExists() {
			return ErrStopUncertain
		}
		return nil
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(groupCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if !s.processGroupExists() {
				return nil
			}
		case <-deadline.C:
			if s.processGroupExists() {
				return ErrStopUncertain
			}
			return nil
		}
	}
}

func (s *processSession) killProcessGroup() {
	if s.killGroup != nil {
		s.killGroup(s.cmd)
		return
	}
	platform.KillProcessGroup(s.cmd)
}

func (s *processSession) processGroupExists() bool {
	if s.groupExists != nil {
		return s.groupExists(s.cmd)
	}
	return platform.ProcessGroupExists(s.cmd)
}

func waitForSignal(done <-chan struct{}, timeout time.Duration) bool {
	if timeout <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func clamp(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}
