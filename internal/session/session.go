package session

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/platform"
	"github.com/creack/pty"
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
	master       *os.File
	closePTYOnce sync.Once
	stopOnce     sync.Once

	// These hooks default to the platform process-group primitives. Keeping
	// them per session makes the negative confirmation paths deterministic in
	// tests without mutating package globals used by concurrent sessions.
	killGroup   func(*exec.Cmd)
	groupExists func(*exec.Cmd) bool
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
	master := s.master
	s.fileMu.RUnlock()
	if master == nil {
		return ErrClosed
	}

	// os.File permits Close concurrently with Write. Do not retain fileMu while
	// writing: a saturated PTY must be unblocked by Close during shutdown.
	_, err := master.Write(input)
	return err
}

func (s *processSession) resize(columns, rows int) error {
	columns = clamp(columns, 1, 65535)
	rows = clamp(rows, 1, 65535)

	s.fileMu.RLock()
	defer s.fileMu.RUnlock()
	if s.master == nil {
		return ErrClosed
	}

	// The descriptor stays protected for the short TIOCSWINSZ ioctl so Close
	// cannot recycle it underneath the operation.
	return pty.Setsize(s.master, &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(columns),
	})
}

func (s *processSession) closePTY() {
	s.closePTYOnce.Do(func() {
		s.fileMu.Lock()
		master := s.master
		s.master = nil
		s.fileMu.Unlock()
		if master != nil {
			_ = master.Close()
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
		s.cancel()
		s.closePTY()
	})
}

func (s *processSession) waitForStop() error {
	return s.waitForStopWithin(gracefulStopTimeout, forcedStopTimeout)
}

func (s *processSession) waitForStopWithin(gracefulTimeout, forcedTimeout time.Duration) error {
	if waitForSignal(s.done, gracefulTimeout) {
		return s.confirmProcessGroupStopped(forcedTimeout)
	}

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
