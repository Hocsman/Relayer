package session

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
)

func TestManagerSendLineUsesProcessorBoundaryAndExactEncoding(t *testing.T) {
	manager, process, reader := newLineInputManager(t)
	defer reader.Close()
	defer process.closePTY()

	if err := manager.SendLine(context.Background(), "line-session", "hello"); err != nil {
		t.Fatalf("SendLine: %v", err)
	}
	data := make([]byte, len("hello\r"))
	if _, err := io.ReadFull(reader, data); err != nil {
		t.Fatalf("read encoded line: %v", err)
	}
	if got, want := string(data), "hello\r"; got != want {
		t.Fatalf("encoded line = %q, want %q", got, want)
	}

	if err := process.processor.Consume([]byte("Overwrite current file? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	pending := process.processor.Pending()
	if pending == nil {
		t.Fatal("expected actionable event")
	}
	if err := manager.SendLine(context.Background(), "line-session", "not-a-decision"); !errors.Is(err, adapters.ErrEventPending) {
		t.Fatalf("pending SendLine error = %v", err)
	}
	if current := process.processor.Pending(); current == nil || current.ID != pending.ID {
		t.Fatalf("SendLine changed pending event: %#v", current)
	}
	assertNoPipeOutput(t, reader)
}

func TestManagerSendLineMapsTerminationAndWriteFailureWithoutAcknowledgement(t *testing.T) {
	manager, process, reader := newLineInputManager(t)
	defer reader.Close()
	process.closePTY()
	if err := manager.SendLine(context.Background(), "line-session", "safe"); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed writer error = %v", err)
	}

	manager, process, reader = newLineInputManager(t)
	defer reader.Close()
	defer process.closePTY()
	process.processor.NewProcessExitEvent(nil, false)
	if err := manager.SendLine(context.Background(), "line-session", "safe"); !errors.Is(err, ErrClosed) {
		t.Fatalf("terminated processor error = %v", err)
	}

	manager, process, reader = newLineInputManager(t)
	defer reader.Close()
	defer process.closePTY()
	process.setResult(nil)
	if err := manager.SendLine(context.Background(), "line-session", "after-wait"); !errors.Is(err, ErrClosed) {
		t.Fatalf("known exited process error = %v", err)
	}
	assertNoPipeOutput(t, reader)
}

func newLineInputManager(t *testing.T) (*Manager, *processSession, *os.File) {
	t.Helper()
	adapter, err := adapters.NewGenericRegexAdapter(adapters.DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	processor, err := adapters.NewProcessor(
		adapter,
		adapters.NewDetectionState("line-session", "line-agent", adapters.GenericID),
		4096,
		adapters.Hooks{},
	)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	process := &processSession{processor: processor, device: &testPipeDevice{File: writer}}
	manager := &Manager{sessions: map[string]*processSession{"line-session": process}}
	return manager, process, reader
}

type testPipeDevice struct {
	*os.File
}

func (d *testPipeDevice) Resize(cols, rows int) error { return nil }

func assertNoPipeOutput(t *testing.T, reader *os.File) {
	t.Helper()
	if err := reader.SetReadDeadline(time.Now().Add(10 * time.Millisecond)); err == nil {
		if count, err := reader.Read(make([]byte, 1)); count != 0 || err == nil {
			t.Fatalf("pipe wrote count=%d error=%v", count, err)
		}
		return
	}
	checkPipeEmpty(t, reader)
}
