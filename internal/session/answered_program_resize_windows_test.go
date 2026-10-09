//go:build windows

package session

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/terminal"
	"golang.org/x/sys/windows"
)

// answeredThenProgramHelperEnv turns the test binary into an agent that asks
// one question at the bottom of a full screen and, once it is answered, runs a
// full-screen program — an editor, a pager — until it reads a line, as a pager
// quits on q. The value names the kind of question.
const answeredThenProgramHelperEnv = "RELAYER_TEST_ANSWERED_THEN_PROGRAM"

var answeredThenProgramPrompts = map[string]string{
	"aider":   "Create new file? (Y)es/(N)o [Yes]: ",
	"generic": "Overwrite file probe.txt? [y/n] ",
}

func runAnsweredThenProgramHelper(kind string) {
	prompt, known := answeredThenProgramPrompts[kind]
	if !known {
		os.Exit(9)
	}
	fmt.Print("agent ready\r\n")
	for index := range 45 {
		fmt.Printf("agent output line %d\r\n", index)
	}
	// The read that waits for the answer is already pending when the question
	// is asked, as an agent's is once it has been waiting for input. Started
	// after the question instead, it began within about a millisecond of the
	// answer, which the test sends as soon as it sees the question. Reproduced
	// locally (CPU limited to four cores, the test ending at the first paint as
	// it does where ConPTY does not pass the screen switch through), a read
	// started that close to the answer did not wake for ten seconds, and a
	// later keystroke released it with the answer queued behind it. A windows-2022
	// run failed the same way, with a program that never painted.
	input := bufio.NewReader(os.Stdin)
	answered := make(chan struct{})
	go func() {
		_, _ = input.ReadString('\n')
		close(answered)
	}()
	// The question in a write of its own, as an agent that stops to ask it.
	time.Sleep(300 * time.Millisecond)
	fmt.Print(prompt)
	<-answered

	stdout := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if windows.GetConsoleMode(stdout, &mode) == nil {
		_ = windows.SetConsoleMode(stdout, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	}
	fmt.Print("\x1b[?1049h\x1b[H\x1b[2Jeditor contents\r\n~\r\n~")
	_, _ = input.ReadString('\n')
	fmt.Print("\x1b[?1049l")
	// Long enough for ConPTY's repaint of the primary screen to be read, and
	// for the question to be asked again if it is going to be.
	time.Sleep(time.Second)
	os.Exit(0)
}

// programRepaints counts the times the program's screen was painted, so the
// test can wait for ConPTY to have redrawn it after each resize. The recorder
// sees a write once the Processor has read it. What is counted is everything
// read so far, not each write: ConPTY cuts its output wherever it likes, and
// "editor contents" or the switch to the alternate screen split between two
// reads was never found in either.
type programRepaints struct {
	mu     sync.Mutex
	read   strings.Builder
	output strings.Builder
	// helperState says whether the helper process is still running, for the
	// dump of a run that failed. Set before the first wait, read by the test's
	// own goroutine only.
	helperState func() string
}

func (r *programRepaints) StartSession(terminal.Info, terminal.Size, time.Time) {}
func (r *programRepaints) RecordOutput(_ terminal.SessionID, _ time.Time, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.read.Write(data)
	fmt.Fprintf(&r.output, "%q\n", data)
}

// paints is how many times the program's screen has been painted.
func (r *programRepaints) paints() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Count(r.read.String(), "editor contents")
}

// passedTheSwitchThrough reports whether ConPTY handed the program's switch to
// the alternate screen on. The ConPTY of Windows 11 does; that of Windows
// Server 2022, and so presumably of Windows 10, paints the program over the
// primary screen and paints the primary screen back when it exits, which
// Relayer cannot tell from the agent asking its question again.
func (r *programRepaints) passedTheSwitchThrough() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Contains(r.read.String(), "\x1b[?1049h")
}

// conptyRepaintsFullScreenPrograms is why a full-screen case is skipped where
// ConPTY does not pass the switch through: docs/adapters.md lists it among the
// cases that still ask an answered question again.
const conptyRepaintsFullScreenPrograms = "this ConPTY paints a full-screen program over the primary screen instead of switching screens; " +
	"the answered question is then asked again when the program exits (a known gap, see docs/adapters.md)"

// RecordInput keeps what was typed in the dump, in order with what was read, so
// a run that fails shows whether the answer was written before the silence.
func (r *programRepaints) RecordInput(_ terminal.SessionID, _ time.Time, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(&r.output, "typed %q\n", data)
}
func (r *programRepaints) RecordResize(terminal.SessionID, time.Time, terminal.Size) {}
func (r *programRepaints) FinishSession(terminal.SessionID, time.Time, *int)         {}

func (r *programRepaints) waitFor(t *testing.T, count int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	seen := 0
	for time.Now().Before(deadline) {
		seen = r.paints()
		if seen >= count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	state := ""
	if r.helperState != nil {
		state = r.helperState() + "\n"
	}
	t.Fatalf("the program's screen was painted %d time(s), want %d\n%s%s", seen, count, state, r.written())
}

func (r *programRepaints) written() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.output.String()
}

// A window resized while a full-screen program runs after the answer, on real
// ConPTY. ConPTY leaves the primary buffer alone while the alternate one is
// shown, resizes it once when the program exits, and repaints it. The screen
// resized the parked primary screen at every step: a shrink pushed its top rows
// into the history, the grow that followed did not pull them back, and ConPTY's
// repaint drew the answered question on a row the memory did not know. It was
// asked again; under an automatic policy the desktop typed a second answer into
// the agent. In the desktop that is the window dragged, or the agent grid
// rearranged, while an editor or a pager is open.
func TestAWindowResizedWhileAProgramRunsAfterTheAnswerDoesNotAskItAgainOnConPTY(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		kind    string
		answer  string
		heights []int
	}{
		{name: "aider/shrink then grow back", kind: "aider", answer: "y\r", heights: []int{20, 30}},
		{name: "aider/shrink, then grow past the old height", kind: "aider", answer: "y\r", heights: []int{20, 40}},
		{name: "aider/a window dragged smaller and back", kind: "aider", answer: "y\r", heights: []int{28, 25, 22, 26, 30}},
		{name: "generic/shrink then grow back", kind: "generic", answer: "nope\r", heights: []int{20, 30}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			registry, err := adapters.NewRegistry(adapters.DefaultPatterns())
			if err != nil {
				t.Fatal(err)
			}
			events := make(chan Event, 256)
			ctx, cancel := context.WithCancel(context.Background())
			manager, err := NewManagerWithRegistry(ctx, events, registry, 64*1024)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			t.Cleanup(func() {
				manager.Close()
				cancel()
			})
			// Every occurrence and withdrawal, in order; the rest is drained so
			// that no essential event can hold the reader up.
			raised := make(chan Event, 64)
			go func() {
				for {
					select {
					case event := <-events:
						switch event.(type) {
						case AdapterEvent, AdapterEventWithdrawn:
							raised <- event
						}
					case <-ctx.Done():
						return
					}
				}
			}()
			repaints := &programRepaints{}
			manager.SetRecorder(repaints)

			info, err := manager.Start(agent.Spec{
				ID:      "answered-then-program",
				Name:    "answered then program",
				Command: []string{os.Args[0], "-test.run=^$"},
				Env:     map[string]string{answeredThenProgramHelperEnv: testCase.kind},
				Adapter: testCase.kind,
				Backend: agent.BackendPTY,
			}, 120, 30)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			repaints.helperState = func() string {
				exited, waitErr, exitCode, _ := manager.Result(info.ID)
				code := "none"
				if exitCode != nil {
					code = strconv.Itoa(*exitCode)
				}
				return fmt.Sprintf("the helper has exited: %t (wait error %v, exit code %s)", exited, waitErr, code)
			}
			next := func() Event {
				t.Helper()
				select {
				case event := <-raised:
					return event
				case <-time.After(20 * time.Second):
					t.Fatalf("nothing happened\n%s", repaints.written())
					return nil
				}
			}

			question, ok := next().(AdapterEvent)
			if !ok || !question.Event.Actionable() {
				t.Fatalf("the first thing raised is not the question: %#v", question)
			}
			if err := manager.SendDataForEvent(info.ID, question.Event.ID, []byte(testCase.answer)); err != nil {
				t.Fatalf("answering: %v", err)
			}
			repaints.waitFor(t, 1)
			if !repaints.passedTheSwitchThrough() {
				t.Skip(conptyRepaintsFullScreenPrograms)
			}
			for index, height := range testCase.heights {
				if err := manager.Resize(info.ID, 120, height); err != nil {
					t.Fatalf("Resize: %v", err)
				}
				repaints.waitFor(t, index+2)
			}
			// The program quits, and ConPTY gives the primary screen back.
			if err := manager.SendRaw(context.Background(), info.ID, []byte("q\r")); err != nil {
				t.Fatalf("quitting the program: %v", err)
			}
			for {
				switch event := next().(type) {
				case AdapterEventWithdrawn:
					t.Fatalf("an occurrence was withdrawn: %q\n%s", event.Event.Match, repaints.written())
				case AdapterEvent:
					if event.Event.Type == adapters.EventProcessExit {
						return
					}
					if event.Event.Actionable() {
						t.Fatalf("the answered question was asked again: %q\n%s", event.Event.Match, repaints.written())
					}
				}
			}
		})
	}
}
