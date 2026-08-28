package adapters

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestProcessorSendLineDeliversExactBytesAndClearsTransientCopy(t *testing.T) {
	processor := newGenericTestProcessor(t, 4096, Hooks{})
	var delivered, retained []byte
	if err := processor.SendLine(context.Background(), "hello é", func(data []byte) error {
		delivered = append([]byte(nil), data...)
		retained = data
		return nil
	}); err != nil {
		t.Fatalf("SendLine: %v", err)
	}
	if got, want := string(delivered), "hello é\r"; got != want {
		t.Fatalf("delivered bytes = %q, want %q", got, want)
	}
	for index, value := range retained {
		if value != 0 {
			t.Fatalf("retained transient byte %d = %d, want zero", index, value)
		}
	}
}

func TestProcessorSendLineValidationIsBoundedAndValueFree(t *testing.T) {
	processor := newGenericTestProcessor(t, 4096, Hooks{})
	invalidUTF8 := string([]byte{'s', 'k', '-', 0xff})
	tests := []struct {
		name string
		line string
	}{
		{name: "nul", line: "sk-fixture\x00value"},
		{name: "carriage-return", line: "sk-fixture\rvalue"},
		{name: "line-feed", line: "sk-fixture\nvalue"},
		{name: "escape", line: "sk-fixture\x1bvalue"},
		{name: "bell", line: "sk-fixture\avalue"},
		{name: "tab", line: "sk-fixture\tvalue"},
		{name: "delete", line: "sk-fixture\x7fvalue"},
		{name: "c1", line: "sk-fixture\u0085value"},
		{name: "invalid-utf8", line: invalidUTF8},
		{name: "over-byte-limit", line: "sk-fixture" + strings.Repeat("x", MaxLineBytes)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writes := 0
			err := processor.SendLine(context.Background(), test.line, func([]byte) error {
				writes++
				return nil
			})
			if !errors.Is(err, ErrInvalidLine) {
				t.Fatalf("error = %v, want ErrInvalidLine", err)
			}
			if writes != 0 {
				t.Fatalf("invalid line performed %d write(s)", writes)
			}
			if strings.Contains(err.Error(), "sk-fixture") {
				t.Fatalf("error exposed input value: %q", err)
			}
		})
	}

	exact := strings.Repeat("é", MaxLineBytes/2)
	var delivered []byte
	if err := processor.SendLine(context.Background(), exact, func(data []byte) error {
		delivered = append([]byte(nil), data...)
		return nil
	}); err != nil {
		t.Fatalf("exact multi-byte boundary: %v", err)
	}
	if len(delivered) != MaxLineBytes+1 || delivered[len(delivered)-1] != '\r' {
		t.Fatalf("exact boundary delivered %d bytes", len(delivered))
	}
	if err := processor.SendLine(context.Background(), exact+"x", func([]byte) error { return nil }); !errors.Is(err, ErrInvalidLine) {
		t.Fatalf("multi-byte overflow error = %v", err)
	}
}

func TestProcessorSendLinePendingDoesNoIOAndKeepsEvent(t *testing.T) {
	processor := newGenericTestProcessor(t, 4096, Hooks{})
	if err := processor.Consume([]byte("Overwrite current file? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	pending := processor.Pending()
	if pending == nil {
		t.Fatal("expected pending prompt")
	}
	writes := 0
	err := processor.SendLine(context.Background(), "sk-fixturevalue", func([]byte) error {
		writes++
		return nil
	})
	if !errors.Is(err, ErrEventPending) {
		t.Fatalf("error = %v, want ErrEventPending", err)
	}
	if writes != 0 {
		t.Fatalf("pending event performed %d write(s)", writes)
	}
	current := processor.Pending()
	if current == nil || current.ID != pending.ID {
		t.Fatalf("pending event changed: before %#v after %#v", pending, current)
	}
	if strings.Contains(err.Error(), "sk-fixturevalue") {
		t.Fatalf("pending error exposed line: %q", err)
	}
}

func TestProcessorSendLineContextTerminationAndUnsupportedDoNoIO(t *testing.T) {
	processor := newGenericTestProcessor(t, 4096, Hooks{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writes := 0
	write := func([]byte) error { writes++; return nil }
	if err := processor.SendLine(ctx, "secret-value", write); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
	processor.NewProcessExitEvent(nil, false)
	if err := processor.SendLine(context.Background(), "secret-value", write); !errors.Is(err, ErrProcessorTerminated) {
		t.Fatalf("terminated error = %v", err)
	}
	if err := processor.SendLine(context.Background(), "secret-value", nil); !errors.Is(err, ErrLineUnsupported) {
		t.Fatalf("nil writer error = %v", err)
	}
	if writes != 0 {
		t.Fatalf("non-deliverable lines performed %d write(s)", writes)
	}
}

func TestProcessorSendLineWriteFailureCanRetryWithoutStateMutation(t *testing.T) {
	processor := newGenericTestProcessor(t, 4096, Hooks{})
	const secret = "sk-fixturevalue123456"
	deliveryFailure := errors.New("transport unavailable for " + secret)
	if err := processor.SendLine(context.Background(), secret, func([]byte) error {
		return deliveryFailure
	}); !errors.Is(err, ErrLineDeliveryUncertain) {
		t.Fatalf("write error = %v, want delivery uncertainty", err)
	} else if errors.Is(err, deliveryFailure) {
		t.Fatal("write error retained the untrusted transport cause")
	} else if strings.Contains(err.Error(), secret) {
		t.Fatalf("write error exposed line: %q", err)
	}
	var delivered []byte
	if err := processor.SendLine(context.Background(), "retry-safe", func(data []byte) error {
		delivered = append([]byte(nil), data...)
		return nil
	}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got, want := string(delivered), "retry-safe\r"; got != want {
		t.Fatalf("retry bytes = %q, want %q", got, want)
	}
}

func TestProcessorSendLineSerializesConcurrentPromptDetection(t *testing.T) {
	processor := newGenericTestProcessor(t, 4096, Hooks{})
	writerEntered := make(chan struct{})
	releaseWriter := make(chan struct{})
	lineDone := make(chan error, 1)
	go func() {
		lineDone <- processor.SendLine(context.Background(), "ordinary", func(data []byte) error {
			close(writerEntered)
			<-releaseWriter
			if got := string(data); got != "ordinary\r" {
				return errors.New("unexpected encoded line")
			}
			return nil
		})
	}()
	<-writerEntered

	promptDone := make(chan error, 1)
	go func() { promptDone <- processor.Consume([]byte("Overwrite current file? [Y/n]")) }()
	select {
	case err := <-promptDone:
		t.Fatalf("prompt detection crossed active line delivery: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseWriter)
	if err := <-lineDone; err != nil {
		t.Fatalf("SendLine: %v", err)
	}
	if err := <-promptDone; err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if processor.Pending() == nil {
		t.Fatal("serialized prompt did not become pending after delivery")
	}
}

func TestProcessorProcessExitLinearizesWithActiveLineDelivery(t *testing.T) {
	processor := newGenericTestProcessor(t, 4096, Hooks{})
	writerEntered := make(chan struct{})
	releaseWriter := make(chan struct{})
	lineDone := make(chan error, 1)
	go func() {
		lineDone <- processor.SendLine(context.Background(), "ordinary", func([]byte) error {
			close(writerEntered)
			<-releaseWriter
			return nil
		})
	}()
	<-writerEntered

	exitDone := make(chan Event, 1)
	go func() { exitDone <- processor.MarkProcessExitEvent(nil, false) }()
	select {
	case event := <-exitDone:
		t.Fatalf("process exit crossed active line delivery: %#v", event)
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseWriter)
	if err := <-lineDone; err != nil {
		t.Fatalf("SendLine: %v", err)
	}
	select {
	case event := <-exitDone:
		if event.Type != EventProcessExit {
			t.Fatalf("terminal event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("process exit did not complete after line delivery")
	}

	writes := 0
	if err := processor.SendLine(context.Background(), "late", func([]byte) error {
		writes++
		return nil
	}); !errors.Is(err, ErrProcessorTerminated) {
		t.Fatalf("late SendLine error = %v", err)
	}
	if writes != 0 {
		t.Fatalf("late SendLine performed %d write(s)", writes)
	}
}

func TestProcessorSendLineContextCancelledWhileWaitingDoesNoIO(t *testing.T) {
	processor := newGenericTestProcessor(t, 4096, Hooks{})
	writerEntered := make(chan struct{})
	releaseWriter := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- processor.SendLine(context.Background(), "first", func([]byte) error {
			close(writerEntered)
			<-releaseWriter
			return nil
		})
	}()
	<-writerEntered

	ctx, cancel := context.WithCancel(context.Background())
	writes := 0
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- processor.SendLine(ctx, "second", func([]byte) error {
			writes++
			return nil
		})
	}()
	cancel()
	close(releaseWriter)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting cancellation error = %v", err)
	}
	if writes != 0 {
		t.Fatalf("cancelled waiter performed %d write(s)", writes)
	}
}
