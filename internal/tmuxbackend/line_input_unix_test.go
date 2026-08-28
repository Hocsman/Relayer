//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tmuxbackend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/terminal"
)

func TestManagerSendLineUsesAtomicProcessorBoundary(t *testing.T) {
	runner := newFakeRunner()
	manager, _ := newTestManager(t, runner, Options{RunID: "safe-line"})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	info, err := manager.Start(context.Background(), testSpec(t, "line-agent"), terminal.Size{})
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.session(info.ID)
	if err != nil {
		t.Fatal(err)
	}

	loadCount := len(runner.callsFor("load-buffer"))
	callCount := len(runner.allCalls())
	secret := "sk-fixturevalue123456"
	if err := manager.SendLine(context.Background(), info.ID, secret+"\t"); !errors.Is(err, terminal.ErrInvalidLine) {
		t.Fatalf("invalid line error = %v", err)
	} else if strings.Contains(err.Error(), secret) {
		t.Fatalf("invalid line error exposed input: %q", err)
	}
	if got := len(runner.callsFor("load-buffer")); got != loadCount {
		t.Fatalf("invalid line performed %d load-buffer call(s)", got-loadCount)
	}
	if got := len(runner.allCalls()); got != callCount {
		t.Fatalf("invalid line performed %d tmux command(s)", got-callCount)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.SendLine(cancelled, info.ID, "cancelled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled line error = %v", err)
	}
	if got := len(runner.callsFor("load-buffer")); got != loadCount {
		t.Fatalf("cancelled line performed %d load-buffer call(s)", got-loadCount)
	}
	if got := len(runner.allCalls()); got != callCount {
		t.Fatalf("cancelled line performed %d tmux command(s)", got-callCount)
	}

	if err := manager.SendLine(context.Background(), info.ID, "ordinary input"); err != nil {
		t.Fatalf("SendLine: %v", err)
	}
	loads := runner.callsFor("load-buffer")
	if len(loads) != loadCount+1 || string(loads[len(loads)-1].Stdin) != "ordinary input\r" {
		t.Fatalf("load-buffer calls = %#v, want exact ordinary input + CR", loads)
	}
	loadCount = len(loads)
	callCount = len(runner.allCalls())
	if _, err := manager.AttachCommand(context.Background(), info.ID); err != nil {
		t.Fatalf("AttachCommand: %v", err)
	}
	loadCount = len(runner.callsFor("load-buffer"))
	if err := manager.SendLine(context.Background(), info.ID, "must not cross attach"); !errors.Is(err, terminal.ErrLineUnsupported) {
		t.Fatalf("attached line error = %v, want unsupported", err)
	}
	if got := len(runner.callsFor("load-buffer")); got != loadCount {
		t.Fatalf("attached line performed %d load-buffer call(s)", got-loadCount)
	}
	target.endAttach()
	callCount = len(runner.allCalls())
	target.updateState(Snapshot{ID: info.ID, Status: StatusAttached, Running: true, Attached: true})
	if err := manager.SendLine(context.Background(), info.ID, "must not cross external attach"); !errors.Is(err, terminal.ErrLineUnsupported) {
		t.Fatalf("externally attached line error = %v, want unsupported", err)
	}
	if got := len(runner.callsFor("load-buffer")); got != loadCount {
		t.Fatalf("externally attached line performed %d load-buffer call(s)", got-loadCount)
	}
	target.updateState(Snapshot{ID: info.ID, Status: StatusDetached, Running: true})
	callCount = len(runner.allCalls())

	if err := target.processor.Consume([]byte("Overwrite current file? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	pending := target.processor.Pending()
	if pending == nil {
		t.Fatal("expected pending prompt")
	}
	err = manager.SendLine(context.Background(), info.ID, secret)
	if !errors.Is(err, terminal.ErrEventPending) {
		t.Fatalf("pending line error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("pending line error exposed input: %q", err)
	}
	if got := len(runner.callsFor("load-buffer")); got != loadCount {
		t.Fatalf("pending line performed %d load-buffer call(s)", got-loadCount)
	}
	if got := len(runner.allCalls()); got != callCount {
		t.Fatalf("pending line performed %d tmux command(s)", got-callCount)
	}
	if current := target.processor.Pending(); current == nil || current.ID != pending.ID {
		t.Fatalf("pending event changed: %#v", current)
	}
	if err := target.processor.Acknowledge(pending.ID); err != nil {
		t.Fatal(err)
	}

	deliveryFailure := errors.New("planned safe transport failure")
	runner.setFailure("load-buffer", deliveryFailure)
	if err := manager.SendLine(context.Background(), info.ID, "retry input"); !errors.Is(err, terminal.ErrLineDeliveryUncertain) {
		t.Fatalf("delivery failure = %v, want uncertainty", err)
	} else if errors.Is(err, deliveryFailure) {
		t.Fatal("delivery failure retained untrusted transport cause")
	}
	runner.setFailure("load-buffer", nil)
	if err := manager.SendLine(context.Background(), info.ID, "retry input"); err != nil {
		t.Fatalf("retry SendLine: %v", err)
	}
	loads = runner.callsFor("load-buffer")
	if string(loads[len(loads)-1].Stdin) != "retry input\r" {
		t.Fatalf("retry bytes = %q", loads[len(loads)-1].Stdin)
	}
	loadCount = len(loads)
	callCount = len(runner.allCalls())

	target.processor.NewProcessExitEvent(nil, false)
	if err := manager.SendLine(context.Background(), info.ID, "after exit"); !errors.Is(err, terminal.ErrClosed) {
		t.Fatalf("terminated line error = %v", err)
	}
	if got := len(runner.callsFor("load-buffer")); got != loadCount {
		t.Fatalf("terminated line performed %d load-buffer call(s)", got-loadCount)
	}
	if got := len(runner.allCalls()); got != callCount {
		t.Fatalf("terminated line performed %d tmux command(s)", got-callCount)
	}
}

func TestManagedSessionProcessExitLinearizesWithLineDelivery(t *testing.T) {
	runner := newFakeRunner()
	manager, _ := newTestManager(t, runner, Options{RunID: "line-exit"})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	info, err := manager.Start(context.Background(), testSpec(t, "line-exit-agent"), terminal.Size{})
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.session(info.ID)
	if err != nil {
		t.Fatal(err)
	}

	deliveryStarted := make(chan struct{})
	releaseDelivery := make(chan struct{})
	lineDone := make(chan error, 1)
	go func() {
		lineDone <- target.sendLine(context.Background(), "ordinary", func([]byte) error {
			close(deliveryStarted)
			<-releaseDelivery
			return nil
		})
	}()
	<-deliveryStarted

	exitDone := make(chan struct{})
	go func() {
		target.updateProcessExitState(Snapshot{
			ID:      info.ID,
			Status:  terminal.StatusExited,
			Running: false,
		})
		close(exitDone)
	}()
	select {
	case <-exitDone:
		t.Fatal("dead snapshot crossed an admitted line delivery")
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseDelivery)
	if err := <-lineDone; err != nil {
		t.Fatalf("admitted SendLine: %v", err)
	}
	select {
	case <-exitDone:
	case <-time.After(time.Second):
		t.Fatal("dead snapshot did not publish after line delivery")
	}

	writes := 0
	if err := target.sendLine(context.Background(), "late", func([]byte) error {
		writes++
		return nil
	}); !errors.Is(err, ErrClosed) {
		t.Fatalf("late SendLine error = %v", err)
	}
	if writes != 0 {
		t.Fatalf("late SendLine performed %d write(s)", writes)
	}
}

var _ terminal.LineSender = (*Manager)(nil)
