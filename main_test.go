package main

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

func TestRingBufferRetainsNewestBytesInOrder(t *testing.T) {
	buffer := NewRingBuffer(5)

	writes := []struct {
		input string
		want  string
	}{
		{input: "abc", want: "abc"},
		{input: "de", want: "abcde"},
		{input: "f", want: "bcdef"},
		{input: "1234567", want: "34567"},
	}

	for _, test := range writes {
		written, err := buffer.Write([]byte(test.input))
		if err != nil {
			t.Fatalf("Write(%q) returned an error: %v", test.input, err)
		}
		if written != len(test.input) {
			t.Fatalf("Write(%q) reported %d bytes, want %d", test.input, written, len(test.input))
		}
		if got := buffer.String(); got != test.want {
			t.Fatalf("after Write(%q), buffer is %q, want %q", test.input, got, test.want)
		}
		if buffer.Len() > buffer.Capacity() {
			t.Fatalf("buffer length %d exceeds capacity %d", buffer.Len(), buffer.Capacity())
		}
	}
}

func TestRingBufferBytesReturnsIndependentCopy(t *testing.T) {
	buffer := NewRingBuffer(4)
	_, _ = buffer.Write([]byte("test"))

	snapshot := buffer.Bytes()
	snapshot[0] = 'b'

	if got := buffer.String(); got != "test" {
		t.Fatalf("mutating Bytes result changed buffer to %q", got)
	}
}

func TestRingBufferClampsNonPositiveCapacity(t *testing.T) {
	for _, capacity := range []int{0, -1} {
		buffer := NewRingBuffer(capacity)
		if got := buffer.Capacity(); got != 1 {
			t.Fatalf("NewRingBuffer(%d) capacity = %d, want 1", capacity, got)
		}
		_, _ = buffer.Write([]byte("ab"))
		if got := buffer.String(); got != "b" {
			t.Fatalf("NewRingBuffer(%d) retained %q, want %q", capacity, got, "b")
		}
	}
}

type recordedInterceptorEvent struct {
	message   tea.Msg
	essential bool
}

func TestInterceptorDetectsPromptAcrossChunksAndSplitANSI(t *testing.T) {
	var events []recordedInterceptorEvent
	interceptor, err := NewInterceptor(
		7,
		[]PromptPattern{{
			Name:        "overwrite",
			Description: "overwrite confirmation",
			Expression:  `(?i)overwrite.*\[y/n\]`,
		}},
		128,
		func(message tea.Msg, essential bool) bool {
			events = append(events, recordedInterceptorEvent{message: message, essential: essential})
			return true
		},
	)
	if err != nil {
		t.Fatalf("NewInterceptor returned an error: %v", err)
	}

	for _, chunk := range []string{
		"\x1b[3",
		"1mOver",
		"write? [Y",
		"/n]\x1b[0",
		"m",
	} {
		interceptor.Consume([]byte(chunk))
	}

	prompts := promptEvents(events)
	if len(prompts) != 1 {
		t.Fatalf("got %d prompt events, want 1: %#v", len(prompts), prompts)
	}
	if prompts[0].SessionID != 7 || prompts[0].Pattern != "overwrite" {
		t.Fatalf("unexpected prompt event: %#v", prompts[0])
	}
	if prompts[0].Match != "Overwrite? [Y/n]" {
		t.Fatalf("prompt match = %q, want %q", prompts[0].Match, "Overwrite? [Y/n]")
	}
	if !interceptor.IsBlocked() {
		t.Fatal("interceptor should be blocked after prompt detection")
	}
	if got := interceptor.output.String(); got != "Overwrite? [Y/n]" {
		t.Fatalf("sanitized output = %q, want %q", got, "Overwrite? [Y/n]")
	}

	for _, event := range events {
		switch event.message.(type) {
		case PromptDetectedMsg:
			if !event.essential {
				t.Error("PromptDetectedMsg was emitted as non-essential")
			}
		case SessionOutputMsg:
			if event.essential {
				t.Error("SessionOutputMsg was emitted as essential")
			}
		}
	}
}

func TestInterceptorDeduplicatesUntilAcknowledgedThenRearms(t *testing.T) {
	var events []recordedInterceptorEvent
	interceptor, err := NewInterceptor(
		11,
		[]PromptPattern{{
			Name:        "overwrite",
			Description: "overwrite confirmation",
			Expression:  `(?i)overwrite.*\[y/n\]`,
		}},
		128,
		func(message tea.Msg, essential bool) bool {
			events = append(events, recordedInterceptorEvent{message: message, essential: essential})
			return true
		},
	)
	if err != nil {
		t.Fatalf("NewInterceptor returned an error: %v", err)
	}

	interceptor.Consume([]byte("Overwrite first? [Y/n]"))
	interceptor.Consume([]byte("\nadditional output while still blocked\n"))
	if got := len(promptEvents(events)); got != 1 {
		t.Fatalf("got %d prompt events while blocked, want 1", got)
	}

	interceptor.Acknowledge()
	if interceptor.IsBlocked() {
		t.Fatal("interceptor remained blocked after Acknowledge")
	}

	interceptor.Consume([]byte("OVERWRITE second? [y/n]"))
	prompts := promptEvents(events)
	if len(prompts) != 2 {
		t.Fatalf("got %d prompt events after rearming, want 2", len(prompts))
	}
	if prompts[1].Match != "OVERWRITE second? [y/n]" {
		t.Fatalf("second prompt match = %q", prompts[1].Match)
	}
	if !interceptor.IsBlocked() {
		t.Fatal("interceptor should be blocked after the second prompt")
	}
}

func TestInterceptorBoundsMalformedANSICarryAndKeepsDetecting(t *testing.T) {
	var events []recordedInterceptorEvent
	interceptor, err := NewInterceptor(
		12,
		[]PromptPattern{{
			Name:        "password",
			Description: "password prompt",
			Expression:  `(?im)password:[[:space:]]*$`,
		}},
		256,
		func(message tea.Msg, essential bool) bool {
			events = append(events, recordedInterceptorEvent{message: message, essential: essential})
			return true
		},
	)
	if err != nil {
		t.Fatalf("NewInterceptor returned an error: %v", err)
	}

	interceptor.Consume([]byte("\x1b]" + strings.Repeat("x", maxANSICarrySize+32)))
	if got := len(interceptor.ansiCarry); got > maxANSICarrySize {
		t.Fatalf("malformed ANSI carry grew to %d bytes", got)
	}
	interceptor.Consume([]byte("\nPassword:"))
	if got := len(promptEvents(events)); got != 1 {
		t.Fatalf("detector did not recover after malformed ANSI input; got %d prompts", got)
	}
}

func promptEvents(events []recordedInterceptorEvent) []PromptDetectedMsg {
	result := make([]PromptDetectedMsg, 0)
	for _, event := range events {
		if prompt, ok := event.message.(PromptDetectedMsg); ok {
			result = append(result, prompt)
		}
	}
	return result
}

func TestModelPreservesAnImmediateSecondPrompt(t *testing.T) {
	application := newModelHarness(t)
	first := PromptDetectedMsg{
		SessionID:   application.panes[0].sessionID,
		Pattern:     "confirmation",
		Description: "first prompt",
	}
	application = updateModel(t, application, first)
	application.input.SetValue("yes")

	application = updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	if !application.writePending || application.inputTarget != -1 || application.panes[0].blocked {
		t.Fatalf("old prompt was not cleared before delivery: %+v", application)
	}

	second := PromptDetectedMsg{
		SessionID:   application.panes[0].sessionID,
		Pattern:     "confirmation",
		Description: "second prompt",
	}
	application = updateModel(t, application, second)
	if !application.panes[0].blocked || application.inputTarget != -1 {
		t.Fatal("second prompt should be queued while the first write is in flight")
	}

	application = updateModel(t, application, inputDeliveredMsg{
		SessionID: application.panes[0].sessionID,
	})
	if application.inputTarget != 0 || !application.panes[0].blocked {
		t.Fatal("second prompt was lost when the first delivery completed")
	}
	if application.panes[0].prompt.Description != "second prompt" {
		t.Fatalf("active prompt = %#v, want the second prompt", application.panes[0].prompt)
	}
}

func TestModelClearsPasswordBeforeAdvancingAfterExit(t *testing.T) {
	application := newModelHarness(t)
	password := PromptDetectedMsg{
		SessionID:   application.panes[0].sessionID,
		Pattern:     "password",
		Description: "password prompt",
	}
	confirmation := PromptDetectedMsg{
		SessionID:   application.panes[1].sessionID,
		Pattern:     "confirmation",
		Description: "next prompt",
	}
	application = updateModel(t, application, password)
	application.input.SetValue("top-secret")
	application = updateModel(t, application, confirmation)

	application = updateModel(t, application, SessionExitedMsg{
		SessionID: application.panes[0].sessionID,
	})
	if got := application.input.Value(); got != "" {
		t.Fatalf("password survived target exit: %q", got)
	}
	if application.input.EchoMode != textinput.EchoNormal {
		t.Fatalf("next non-password prompt has echo mode %v", application.input.EchoMode)
	}
	if application.inputTarget != 1 {
		t.Fatalf("input target = %d, want the second pane", application.inputTarget)
	}
}

func newModelHarness(t *testing.T) model {
	t.Helper()
	events := make(chan tea.Msg, 8)
	manager, err := NewSessionManager(
		context.Background(),
		events,
		defaultPromptPatterns,
		256,
	)
	if err != nil {
		t.Fatalf("NewSessionManager returned an error: %v", err)
	}
	t.Cleanup(manager.BeginShutdown)

	var sessions [2]*Session
	for index := range sessions {
		interceptor, interceptorErr := NewInterceptor(
			index,
			defaultPromptPatterns,
			256,
			nil,
		)
		if interceptorErr != nil {
			t.Fatalf("NewInterceptor returned an error: %v", interceptorErr)
		}
		session := &Session{
			ID:          index,
			Name:        "test agent",
			interceptor: interceptor,
		}
		manager.sessions[index] = session
		sessions[index] = session
	}
	return newModel(manager, events, sessions)
}

func updateModel(t *testing.T, application model, message tea.Msg) model {
	t.Helper()
	updated, _ := application.Update(message)
	result, ok := updated.(model)
	if !ok {
		t.Fatalf("Update returned model type %T", updated)
	}
	return result
}

func TestSessionManagerRelaysBlockedPromptAndClosesCleanly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creack/pty integration requires a Unix PTY")
	}
	if _, err := exec.LookPath("stty"); err != nil {
		t.Skipf("stty is required for the PTY resize assertion: %v", err)
	}

	events := make(chan tea.Msg, 128)
	manager, err := NewSessionManager(
		context.Background(),
		events,
		[]PromptPattern{{
			Name:        "overwrite",
			Description: "overwrite confirmation",
			Expression:  `(?i)overwrite.*\[y/n\]`,
		}},
		4096,
	)
	if err != nil {
		t.Fatalf("NewSessionManager returned an error: %v", err)
	}
	defer manager.Close()

	command := `printf 'Running...\n'; printf 'Overwrite? [Y/n]'; IFS= read -r answer; printf '\nDone: %s\n' "$answer"; stty size`
	session, err := manager.Start("integration", command, 40, 10)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	promptSeen := false
	latestOutput := ""
	for !promptSeen {
		select {
		case message := <-events:
			switch msg := message.(type) {
			case SessionOutputMsg:
				if msg.SessionID == session.ID {
					latestOutput, _ = manager.Output(session.ID)
				}
			case PromptDetectedMsg:
				if msg.SessionID == session.ID {
					promptSeen = true
				}
			case SessionExitedMsg:
				if msg.SessionID == session.ID {
					t.Fatalf("session exited before receiving input: %v", msg.Err)
				}
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for prompt; latest output: %q", latestOutput)
		}
	}

	if !session.interceptor.IsBlocked() {
		t.Fatal("session interceptor is not blocked after PromptDetectedMsg")
	}
	if err := manager.Resize(session.ID, 73, 19); err != nil {
		t.Fatalf("Resize returned an error: %v", err)
	}
	if err := manager.SendInput(session.ID, "yes"); err != nil {
		t.Fatalf("SendInput returned an error: %v", err)
	}
	if session.interceptor.IsBlocked() {
		t.Fatal("SendInput did not acknowledge and rearm the interceptor")
	}

	gotExit := false
	for !(gotExit && strings.Contains(latestOutput, "Done: yes") && strings.Contains(latestOutput, "19 73")) {
		select {
		case message := <-events:
			switch msg := message.(type) {
			case SessionOutputMsg:
				if msg.SessionID == session.ID {
					latestOutput, _ = manager.Output(session.ID)
				}
			case SessionExitedMsg:
				if msg.SessionID == session.ID {
					latestOutput, _ = manager.Output(session.ID)
					if msg.Err != nil {
						t.Fatalf("session exited with an error: %v; output: %q", msg.Err, latestOutput)
					}
					gotExit = true
				}
			case SessionErrorMsg:
				if msg.SessionID == session.ID {
					t.Fatalf("session emitted a PTY error: %v", msg.Err)
				}
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for response, resize, and exit; output: %q", latestOutput)
		}
	}

	manager.Close()
	select {
	case <-session.done:
	default:
		t.Fatal("session done channel is still open after manager.Close")
	}
	if err := session.Write("late input"); !errors.Is(err, errSessionClosed) {
		t.Fatalf("Write after manager.Close returned %v, want %v", err, errSessionClosed)
	}
}

func TestSessionManagerCloseKillsSignalIgnoringProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups require Unix")
	}

	events := make(chan tea.Msg, 32)
	manager, err := NewSessionManager(
		context.Background(),
		events,
		defaultPromptPatterns,
		1024,
	)
	if err != nil {
		t.Fatalf("NewSessionManager returned an error: %v", err)
	}
	defer manager.Close()

	command := `trap '' TERM HUP; (trap '' TERM HUP; while :; do sleep 30; done) & printf 'READY\n'; wait`
	session, err := manager.Start("stubborn group", command, 40, 10)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ready := false
	for !ready {
		select {
		case message := <-events:
			if output, ok := message.(SessionOutputMsg); ok && output.SessionID == session.ID {
				content, _ := manager.Output(session.ID)
				ready = strings.Contains(content, "READY")
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for the stubborn child process")
		}
	}

	started := time.Now()
	manager.Close()
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Close took %v", elapsed)
	}
	// A killed orphan can remain visible as a zombie until init reaps it.
	reapedDeadline := time.Now().Add(time.Second)
	for processGroupExists(session.cmd) && time.Now().Before(reapedDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processGroupExists(session.cmd) {
		killProcessGroup(session.cmd)
		t.Fatal("signal-ignoring descendants survived manager.Close")
	}
}
