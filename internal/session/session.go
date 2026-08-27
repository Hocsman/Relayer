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
var ErrClosed = errors.New("session PTY fermée")

const (
	gracefulStopTimeout = 1500 * time.Millisecond
	forcedStopTimeout   = 500 * time.Millisecond
	descendantGraceTime = 250 * time.Millisecond
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
	resultMu  sync.RWMutex
	exited    bool
	waitErr   error

	fileMu       sync.RWMutex
	master       *os.File
	closePTYOnce sync.Once
	stopOnce     sync.Once
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

func (s *processSession) waitForStop() {
	select {
	case <-s.done:
		return
	case <-time.After(gracefulStopTimeout):
	}

	platform.KillProcessGroup(s.cmd)
	select {
	case <-s.done:
	case <-time.After(forcedStopTimeout):
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
