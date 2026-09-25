package tui

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// The stop and restart keys ask for a second, identical press before they do
// anything, since either ends the agent's process. These tests pin that
// confirmation and what refuses it, which nothing tested before.

func lifecycleAgent(backend string) Pane {
	return Pane{ID: scenarioAgent, Name: "Agent A", Command: "agent", Backend: backend, Adapter: adapters.AiderID}
}

// newLifecycleHarness is one agent under the Model, over a backend that can
// stop and restart it, with the keyboard on the agent's pane as an operator's
// is before pressing x or r.
func newLifecycleHarness(t *testing.T, pane Pane, configuration policy.Config) *legacyDriver {
	t.Helper()
	driver := newLegacyDriver(t, []Pane{pane}, configuration)
	driver.focusAgent(pane.ID)
	return driver
}

// hold hands a message to the Model without running the command it returns,
// so that a test can look at the Model while that command has not run yet.
func hold(t *testing.T, driver *legacyDriver, message tea.Msg) tea.Cmd {
	t.Helper()
	updated, command := updateModel(t, driver.model, message)
	driver.model = updated
	return command
}

func runeKey(text string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)}
}

func logCount(model *Model, text string) int {
	count := 0
	for _, line := range model.logs {
		if strings.Contains(line, text) {
			count++
		}
	}
	return count
}

// TestTheFirstLifecyclePressOnlyAsksForASecond: one press arms the action,
// says how to confirm it in the log and on the agent's pane, and runs nothing.
// Either case of the key does, and the log names it in lower case.
func TestTheFirstLifecyclePressOnlyAsksForASecond(t *testing.T) {
	for _, candidate := range []struct {
		key    string
		armed  string
		banner string
	}{
		{key: "x", armed: "Press x again within 5s to confirm stop of Agent A", banner: "PRESS X AGAIN TO CONFIRM"},
		{key: "X", armed: "Press x again within 5s to confirm stop of Agent A", banner: "PRESS X AGAIN TO CONFIRM"},
		{key: "r", armed: "Press r again within 5s to confirm restart of Agent A", banner: "PRESS R AGAIN TO CONFIRM"},
		{key: "R", armed: "Press r again within 5s to confirm restart of Agent A", banner: "PRESS R AGAIN TO CONFIRM"},
	} {
		t.Run(candidate.key, func(t *testing.T) {
			driver := newLifecycleHarness(t, lifecycleAgent("pty"), policy.Config{DefaultAction: policy.ActionAsk})

			if command := hold(t, driver, runeKey(candidate.key)); command != nil {
				t.Fatal("the first press returned a command")
			}
			if calls := driver.backend.lifecycleSnapshot(); len(calls) != 0 {
				t.Fatalf("the first press reached the backend: %v", calls)
			}
			if logCount(driver.model, candidate.armed) != 1 {
				t.Fatalf("the log does not say how to confirm (%q):\n%s", candidate.armed, strings.Join(driver.model.logs, "\n"))
			}
			if view := driver.model.View(); !strings.Contains(view, candidate.banner) {
				t.Fatalf("the pane does not show %q:\n%s", candidate.banner, view)
			}
		})
	}
}

// TestTheSecondLifecyclePressDispatchesExactlyOneAction: the second press runs
// the action once, outside Update, and spends the confirmation. While the
// action runs, the agent shows it, and neither key does anything more on that
// agent, not even a new confirmation. Once it has returned, one more press
// only asks for a second again: a confirmation confirms one action.
func TestTheSecondLifecyclePressDispatchesExactlyOneAction(t *testing.T) {
	for _, candidate := range []struct {
		key     string
		call    string
		armed   string
		running string
		started string
		done    string
	}{
		{key: "x", call: "stop agent-a", armed: "Press x again within 5s to confirm stop of Agent A", running: "STOPPING…", started: "Stopping Agent A…", done: "Agent A stopped by operator"},
		{key: "X", call: "stop agent-a", armed: "Press x again within 5s to confirm stop of Agent A", running: "STOPPING…", started: "Stopping Agent A…", done: "Agent A stopped by operator"},
		{key: "r", call: "restart agent-a", armed: "Press r again within 5s to confirm restart of Agent A", running: "RESTARTING…", started: "Restarting Agent A…", done: "Agent A restarted by operator"},
		{key: "R", call: "restart agent-a", armed: "Press r again within 5s to confirm restart of Agent A", running: "RESTARTING…", started: "Restarting Agent A…", done: "Agent A restarted by operator"},
	} {
		t.Run(candidate.key, func(t *testing.T) {
			driver := newLifecycleHarness(t, lifecycleAgent("pty"), policy.Config{DefaultAction: policy.ActionAsk})

			_ = hold(t, driver, runeKey(candidate.key))
			command := hold(t, driver, runeKey(candidate.key))
			if command == nil {
				t.Fatal("the second press returned no command")
			}
			if calls := driver.backend.lifecycleSnapshot(); len(calls) != 0 {
				t.Fatalf("the action ran inside Update: %v", calls)
			}
			if logCount(driver.model, candidate.started) != 1 {
				t.Fatalf("the log does not announce the action:\n%s", strings.Join(driver.model.logs, "\n"))
			}
			view := driver.model.View()
			if status, _ := paneTitle(t, view, "Agent A"); status != candidate.running {
				t.Fatalf("status while the action runs = %q, want %q", status, candidate.running)
			}
			if strings.Contains(view, "AGAIN TO CONFIRM") {
				t.Fatalf("the dispatched action is still armed:\n%s", view)
			}

			logged := len(driver.model.logs)
			for _, again := range []string{"x", "r", "x", "r"} {
				if extra := hold(t, driver, runeKey(again)); extra != nil {
					t.Fatalf("%s while the action runs returned a command", again)
				}
			}
			if len(driver.model.logs) != logged || strings.Contains(driver.model.View(), "AGAIN TO CONFIRM") {
				t.Fatalf("a press while the action runs was taken:\n%s", strings.Join(driver.model.logs, "\n"))
			}

			driver.run(command)
			if calls := driver.backend.lifecycleSnapshot(); !reflect.DeepEqual(calls, []string{candidate.call}) {
				t.Fatalf("backend calls = %v, want exactly %q", calls, candidate.call)
			}
			if logCount(driver.model, candidate.done) != 1 {
				t.Fatalf("the log does not report the result:\n%s", strings.Join(driver.model.logs, "\n"))
			}
			if status, _ := paneTitle(t, driver.model.View(), "Agent A"); status != "RUNNING" {
				t.Fatalf("status once the action returned = %q, want RUNNING", status)
			}

			if again := hold(t, driver, runeKey(candidate.key)); again != nil {
				t.Fatalf("one %s after the action returned dispatched another", candidate.key)
			}
			if calls := driver.backend.lifecycleSnapshot(); len(calls) != 1 {
				t.Fatalf("one press after the action reached the backend: %v", calls)
			}
			if logCount(driver.model, candidate.armed) != 2 {
				t.Fatalf("one press after the action did not ask for a second:\n%s", strings.Join(driver.model.logs, "\n"))
			}
		})
	}
}

// TestPastedLifecycleKeysArmAndConfirmNothing: several runes in one message,
// as a paste delivers them, are text rather than a press, so "xx" neither
// arms a stop nor confirms one already armed, and takes the armed one back as
// any other key does.
func TestPastedLifecycleKeysArmAndConfirmNothing(t *testing.T) {
	driver := newLifecycleHarness(t, lifecycleAgent("pty"), policy.Config{DefaultAction: policy.ActionAsk})

	for range 2 {
		if command := hold(t, driver, runeKey("xx")); command != nil {
			t.Fatal("a pasted xx returned a command")
		}
	}
	if logCount(driver.model, "Press x again") != 0 || strings.Contains(driver.model.View(), "AGAIN TO CONFIRM") {
		t.Fatalf("a pasted xx armed a stop:\n%s", strings.Join(driver.model.logs, "\n"))
	}

	_ = hold(t, driver, runeKey("x"))
	if command := hold(t, driver, runeKey("xx")); command != nil {
		t.Fatal("a pasted xx confirmed the armed stop")
	}
	if strings.Contains(driver.model.View(), "AGAIN TO CONFIRM") {
		t.Fatal("a pasted xx left the stop armed")
	}
	if calls := driver.backend.lifecycleSnapshot(); len(calls) != 0 {
		t.Fatalf("a pasted xx reached the backend: %v", calls)
	}
}

// TestAnyOtherKeyDisarmsTheConfirmation: a key other than the armed one,
// including the other lifecycle key, takes the confirmation back, so a stop
// is never the second half of two unrelated presses. The other lifecycle key
// takes it back even where it is refused itself, as x is on a finished agent,
// so r x r there asks for a restart twice and runs none.
func TestAnyOtherKeyDisarmsTheConfirmation(t *testing.T) {
	driver := newLifecycleHarness(t, lifecycleAgent("pty"), policy.Config{DefaultAction: policy.ActionAsk})
	const armedStop = "Press x again within 5s to confirm stop of Agent A"
	const armedRestart = "Press r again within 5s to confirm restart of Agent A"

	_ = hold(t, driver, runeKey("x"))
	if command := hold(t, driver, tea.KeyMsg{Type: tea.KeyDown}); command != nil {
		t.Fatal("scrolling the pane returned a command")
	}
	if strings.Contains(driver.model.View(), "PRESS X AGAIN TO CONFIRM") {
		t.Fatal("another key left the stop armed")
	}
	if command := hold(t, driver, runeKey("x")); command != nil {
		t.Fatal("x after another key confirmed the stop")
	}
	if logCount(driver.model, armedStop) != 2 {
		t.Fatalf("x after another key did not arm the stop again:\n%s", strings.Join(driver.model.logs, "\n"))
	}

	if command := hold(t, driver, runeKey("r")); command != nil {
		t.Fatal("r confirmed the armed stop")
	}
	view := driver.model.View()
	if strings.Contains(view, "PRESS X AGAIN TO CONFIRM") || !strings.Contains(view, "PRESS R AGAIN TO CONFIRM") {
		t.Fatalf("r did not replace the armed stop with a restart to confirm:\n%s", view)
	}
	if command := hold(t, driver, runeKey("x")); command != nil {
		t.Fatal("x confirmed the armed restart")
	}
	if calls := driver.backend.lifecycleSnapshot(); len(calls) != 0 {
		t.Fatalf("alternating keys reached the backend: %v", calls)
	}

	finished := newLifecycleHarness(t, lifecycleAgent("pty"), policy.Config{DefaultAction: policy.ActionAsk})
	finished.do(exits(scenarioExit("lk-disarm-exit", adapters.AiderID, 0, false)))
	_ = hold(t, finished, runeKey("r"))
	if command := hold(t, finished, runeKey("x")); command != nil {
		t.Fatal("x on a finished agent returned a command")
	}
	if logCount(finished.model, "Agent A already finished; press r to restart it") != 1 {
		t.Fatalf("x on a finished agent does not point to r:\n%s", strings.Join(finished.model.logs, "\n"))
	}
	if strings.Contains(finished.model.View(), "PRESS R AGAIN TO CONFIRM") {
		t.Fatal("a refused x left the restart armed")
	}
	if command := hold(t, finished, runeKey("r")); command != nil {
		t.Fatal("r after a refused x confirmed the restart armed before it")
	}
	if logCount(finished.model, armedRestart) != 2 {
		t.Fatalf("r after a refused x did not ask for a second press again:\n%s", strings.Join(finished.model.logs, "\n"))
	}
	if calls := finished.backend.lifecycleSnapshot(); !reflect.DeepEqual(calls, []string{"exited agent-a"}) {
		t.Fatalf("r x r on a finished agent reached the backend: %v", calls)
	}
}

// TestAnArmedStopDoesNotFollowTheFocusToAnotherAgent: a click moves the
// keyboard to another agent without a key, so nothing takes back the stop
// armed on the first. It stays that agent's: only its pane asks for the second
// press, and x on the other agent arms a stop of that agent instead of
// confirming the first one's.
func TestAnArmedStopDoesNotFollowTheFocusToAnotherAgent(t *testing.T) {
	agentB := Pane{ID: "agent-b", Name: "Agent B", Command: "agent", Backend: "pty", Adapter: adapters.AiderID}
	driver := newLegacyDriver(t, []Pane{lifecycleAgent("pty"), agentB}, policy.Config{DefaultAction: policy.ActionAsk})
	// Side by side at the driver's width, each pane cuts its title before
	// the banner.
	driver.update(tea.WindowSizeMsg{Width: 200, Height: 36})
	driver.focusAgent(scenarioAgent)
	bannerOn := func(name string) bool {
		return strings.Contains(paneTitleText(t, driver.model.View(), name), "AGAIN TO CONFIRM")
	}

	_ = hold(t, driver, runeKey("x"))
	if !bannerOn("Agent A") || bannerOn("Agent B") {
		t.Fatalf("the stop armed on Agent A is not shown on its pane alone:\n%s", driver.model.View())
	}

	cells := driver.model.layout.Cells
	if len(cells) != 2 || cells[1].AgentIndex != driver.model.paneIndex(agentB.ID) {
		t.Fatalf("Agent B is not the second of two cells: %+v", cells)
	}
	if command := hold(t, driver, mouseLeftClick(cells[1].Outer.X+1, cells[1].Outer.Y+1)); command != nil {
		t.Fatal("a click on Agent B returned a command")
	}
	if driver.model.focus.Kind != FocusAgent || driver.model.focus.AgentID != agentB.ID {
		t.Fatalf("the click did not move the keyboard to Agent B: %+v", driver.model.focus)
	}

	if command := hold(t, driver, runeKey("x")); command != nil {
		t.Fatal("x on Agent B confirmed the stop armed on Agent A")
	}
	if logCount(driver.model, "Press x again within 5s to confirm stop of Agent B") != 1 {
		t.Fatalf("x on Agent B did not arm a stop of Agent B:\n%s", strings.Join(driver.model.logs, "\n"))
	}
	if bannerOn("Agent A") || !bannerOn("Agent B") {
		t.Fatalf("the armed stop did not move to Agent B's pane alone:\n%s", driver.model.View())
	}
	if calls := driver.backend.lifecycleSnapshot(); len(calls) != 0 {
		t.Fatalf("a stop armed on one agent reached the backend through another: %v", calls)
	}
}

// TestAPressWithinFiveSecondsStillConfirms: the window is the five seconds the
// log promises, not a shorter one, so a second press a person makes a few
// seconds after the first still runs the action. The first press is dated a
// quarter of a second inside the window: the second press follows it at once,
// so the test's own time cannot close the window, and one more than a quarter
// of a second short of five seconds fails.
func TestAPressWithinFiveSecondsStillConfirms(t *testing.T) {
	driver := newLifecycleHarness(t, lifecycleAgent("pty"), policy.Config{DefaultAction: policy.ActionAsk})

	_ = hold(t, driver, runeKey("x"))
	driver.model.lifecycleConfirm.armedAt = time.Now().Add(-lifecycleConfirmTimeout + 250*time.Millisecond)
	stop := hold(t, driver, runeKey("x"))
	if stop == nil {
		t.Fatal("a press just under five seconds after the first did not confirm the stop")
	}
	if logCount(driver.model, "Press x again within 5s to confirm stop of Agent A") != 1 {
		t.Fatalf("a press within the window armed the stop again:\n%s", strings.Join(driver.model.logs, "\n"))
	}
	driver.run(stop)
	if calls := driver.backend.lifecycleSnapshot(); !reflect.DeepEqual(calls, []string{"stop agent-a"}) {
		t.Fatalf("backend calls = %v, want exactly %q", calls, "stop agent-a")
	}
}

// TestTheConfirmationLapsesAfterFiveSeconds: a second press later than the
// window arms the action again instead of running it.
func TestTheConfirmationLapsesAfterFiveSeconds(t *testing.T) {
	driver := newLifecycleHarness(t, lifecycleAgent("pty"), policy.Config{DefaultAction: policy.ActionAsk})

	_ = hold(t, driver, runeKey("x"))
	driver.model.lifecycleConfirm.armedAt = time.Now().Add(-lifecycleConfirmTimeout - time.Millisecond)
	if command := hold(t, driver, runeKey("x")); command != nil {
		t.Fatal("a press after the window confirmed the stop")
	}
	if logCount(driver.model, "Press x again within 5s to confirm stop of Agent A") != 2 {
		t.Fatalf("a press after the window did not arm the stop again:\n%s", strings.Join(driver.model.logs, "\n"))
	}
	if command := hold(t, driver, runeKey("x")); command == nil {
		t.Fatal("a press within the new window did not confirm the stop")
	}
}

// TestABusyAgentRefusesTheLifecycleKeys: while a line, an automatic decision
// or a tmux attach is under way on the agent, x says why it waits and arms
// nothing: the process must not end under a write whose outcome is journaled.
func TestABusyAgentRefusesTheLifecycleKeys(t *testing.T) {
	allowEdits := policy.Config{DefaultAction: policy.ActionAsk, Rules: []policy.Rule{allowAiderEdits()}}
	for _, candidate := range []struct {
		name    string
		backend string
		busy    func(t *testing.T, driver *legacyDriver) tea.Cmd
		want    string
	}{
		{
			name:    "a line is being written",
			backend: "pty",
			busy: func(t *testing.T, driver *legacyDriver) tea.Cmd {
				driver.runes("i")
				driver.runes("run the tests")
				return hold(t, driver, tea.KeyMsg{Type: tea.KeyEnter})
			},
			want: "Agent A is receiving a line; retry once it settles",
		},
		{
			name:    "an automatic decision is being written",
			backend: "pty",
			busy: func(t *testing.T, driver *legacyDriver) tea.Cmd {
				prompt := scenarioPrompt("lk-auto", adapters.AiderID)
				driver.backend.holdPrompt(t, prompt)
				return hold(t, driver, session.AdapterEvent{Event: prompt})
			},
			want: "Agent A is receiving an automatic decision; retry once it settles",
		},
		{
			name:    "a tmux attach is under way",
			backend: "tmux",
			busy: func(t *testing.T, driver *legacyDriver) tea.Cmd {
				return hold(t, driver, tea.KeyMsg{Type: tea.KeyEnter})
			},
			want: "Agent A is attaching to tmux; retry after it returns",
		},
	} {
		t.Run(candidate.name, func(t *testing.T) {
			driver := newLifecycleHarness(t, lifecycleAgent(candidate.backend), allowEdits)
			if pending := candidate.busy(t, driver); pending == nil {
				t.Fatal("nothing is under way on the agent")
			}
			for _, key := range []string{"x", "r"} {
				if command := hold(t, driver, runeKey(key)); command != nil {
					t.Fatalf("%s on a busy agent returned a command", key)
				}
			}
			if logCount(driver.model, candidate.want) != 2 {
				t.Fatalf("the log does not say why the agent waits (%q):\n%s", candidate.want, strings.Join(driver.model.logs, "\n"))
			}
			if strings.Contains(driver.model.View(), "AGAIN TO CONFIRM") {
				t.Fatal("a busy agent armed a lifecycle action")
			}
			for _, call := range driver.backend.lifecycleSnapshot() {
				if !strings.HasPrefix(call, "attach ") {
					t.Fatalf("a busy agent's key reached the backend: %v", call)
				}
			}
		})
	}
}

// TestAnAnswerBeingWrittenDoesNotHoldTheLifecycleKeysBack pins a gap. The
// guard for an answer in flight compares the agent with the prompt the input
// field answers, which the submission has already cleared, so x x stops the
// agent while its answer is being written. The shared core refuses that stop
// itself, with ErrDecisionInFlight (internal/supervise/session.go:57-59).
func TestAnAnswerBeingWrittenDoesNotHoldTheLifecycleKeysBack(t *testing.T) {
	driver := newLifecycleHarness(t, lifecycleAgent("pty"), policy.Config{DefaultAction: policy.ActionAsk})
	prompt := scenarioPrompt("lk-answer", adapters.AiderID)
	driver.do(raise(prompt))
	driver.runes("yes")
	if answer := hold(t, driver, tea.KeyMsg{Type: tea.KeyEnter}); answer == nil {
		t.Fatal("the answer returned no command")
	}
	driver.focusAgent(scenarioAgent)

	_ = hold(t, driver, runeKey("x"))
	stop := hold(t, driver, runeKey("x"))
	if stop == nil {
		t.Fatal("x x while an answer is being written dispatched nothing; the gap is closed, so this test must change")
	}
	if logCount(driver.model, "is receiving an answer") != 0 {
		t.Fatalf("the answer guard fired:\n%s", strings.Join(driver.model.logs, "\n"))
	}
}

// TestStopOnAFinishedAgentPointsToRestart: a finished agent has nothing to
// stop. x says so and arms nothing, the pane offers r, and r r restarts it.
func TestStopOnAFinishedAgentPointsToRestart(t *testing.T) {
	driver := newLifecycleHarness(t, lifecycleAgent("pty"), policy.Config{DefaultAction: policy.ActionAsk})
	driver.do(exits(scenarioExit("lk-exit", adapters.AiderID, 0, false)))
	if status, _ := paneTitle(t, driver.model.View(), "Agent A"); status != "FINISHED" {
		t.Fatalf("status after the exit = %q, want FINISHED", status)
	}
	if !strings.Contains(driver.model.View(), "R: restart") {
		t.Fatalf("a finished agent's pane does not offer a restart:\n%s", driver.model.View())
	}

	if command := hold(t, driver, runeKey("x")); command != nil {
		t.Fatal("x on a finished agent returned a command")
	}
	if logCount(driver.model, "Agent A already finished; press r to restart it") != 1 {
		t.Fatalf("x on a finished agent does not point to r:\n%s", strings.Join(driver.model.logs, "\n"))
	}
	if strings.Contains(driver.model.View(), "PRESS X AGAIN TO CONFIRM") {
		t.Fatal("x on a finished agent armed a stop")
	}

	_ = hold(t, driver, runeKey("r"))
	view := driver.model.View()
	if !strings.Contains(view, "PRESS R AGAIN TO CONFIRM") || strings.Contains(view, "R: restart") {
		t.Fatalf("an armed restart does not replace the restart hint:\n%s", view)
	}
	restart := hold(t, driver, runeKey("r"))
	if restart == nil {
		t.Fatal("r r on a finished agent dispatched nothing")
	}
	// While the restart runs, the pane shows it as its status and no longer
	// offers a restart the keys would refuse.
	view = driver.model.View()
	if status, _ := paneTitle(t, view, "Agent A"); status != "RESTARTING…" || strings.Contains(view, "R: restart") {
		t.Fatalf("a finished agent being restarted shows %q and still offers a restart:\n%s", status, view)
	}
	driver.run(restart)
	if calls := driver.backend.lifecycleSnapshot(); !reflect.DeepEqual(calls, []string{"exited agent-a", "restart agent-a"}) {
		t.Fatalf("backend calls = %v", calls)
	}
	if status, _ := paneTitle(t, driver.model.View(), "Agent A"); status != "RUNNING" || strings.Contains(driver.model.View(), "R: restart") {
		t.Fatalf("the restarted agent shows %q:\n%s", status, driver.model.View())
	}
}

// TestABackendWithoutLifecycleRefusesTheKeys: a backend that cannot stop an
// agent says so, and nothing is armed.
func TestABackendWithoutLifecycleRefusesTheKeys(t *testing.T) {
	application, _, _ := newModelHarness(t)
	for _, key := range []string{"x", "r"} {
		var command tea.Cmd
		application, command = updateModel(t, application, runeKey(key))
		if command != nil {
			t.Fatalf("%s returned a command", key)
		}
	}
	if logCount(application, "Per-agent lifecycle is not supported by this backend") != 2 {
		t.Fatalf("the log does not say the backend cannot:\n%s", strings.Join(application.logs, "\n"))
	}
	if strings.Contains(application.View(), "AGAIN TO CONFIRM") {
		t.Fatal("a backend without lifecycle armed an action")
	}
}
