//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tmuxbackend

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/intercept"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
)

func TestNewManagerWithRegistryResolvesSpecAdapter(t *testing.T) {
	runner := newFakeRunner()
	events := make(chan session.Event, 64)
	registry, err := adapters.NewRegistry(intercept.DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	options := Options{
		Runner:        runner,
		TmuxPath:      "tmux",
		HelperPath:    "/test/bin/relayer",
		RuntimeDir:    t.TempDir(),
		RunID:         "registry",
		PollInterval:  minimumPollInterval,
		handoffWaiter: skipLaunchHandoff,
	}
	manager, err := NewManagerWithRegistry(context.Background(), events, registry, 4096, options)
	if err != nil {
		t.Fatalf("NewManagerWithRegistry: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	spec := testSpec(t, "auto-adapter")
	spec.Adapter = ""
	info, err := manager.Start(context.Background(), spec, terminal.Size{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info.Adapter != adapters.GenericID {
		t.Fatalf("resolved adapter = %q, want %q", info.Adapter, adapters.GenericID)
	}

	unknown := testSpec(t, "unknown-adapter")
	unknown.Adapter = "not-registered"
	startsBefore := len(runner.callsFor("new-session"))
	if _, err := manager.Start(context.Background(), unknown, terminal.Size{}); !errors.Is(err, adapters.ErrUnknownAdapter) {
		t.Fatalf("unknown adapter error = %v", err)
	}
	if got := len(runner.callsFor("new-session")); got != startsBefore {
		t.Fatalf("unknown adapter launched %d tmux sessions", got-startsBefore)
	}
}

func TestNewManagerWithRegistryRejectsNilRegistry(t *testing.T) {
	_, err := NewManagerWithRegistry(
		context.Background(),
		make(chan session.Event, 1),
		nil,
		1024,
		Options{Runner: newFakeRunner(), TmuxPath: "tmux"},
	)
	if err == nil || !strings.Contains(err.Error(), "registry") {
		t.Fatalf("nil registry error = %v", err)
	}
}

func TestManagerEventIdentitySnapshotAndExactResolution(t *testing.T) {
	runner := newFakeRunner()
	manager, events := newTestManager(t, runner, Options{
		RunID:         "typed-events",
		PersistOnExit: true,
		PollInterval:  minimumPollInterval,
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	info, err := manager.Start(context.Background(), testSpec(t, "typed-agent"), terminal.Size{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.session(info.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := target.processor.Consume([]byte("Overwrite? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	first := contractWaitForPrompt(t, events, info.ID)
	if first.Adapter != info.Adapter || first.Sequence != 1 || first.ID == "" {
		t.Fatalf("first typed event = %#v", first)
	}
	snapshot, err := manager.Snapshot(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Pending == nil || snapshot.Pending.ID != first.ID || snapshot.Revision != first.Sequence {
		t.Fatalf("snapshot = %#v, event = %#v", snapshot, first)
	}
	repeated, err := manager.Snapshot(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Pending == nil || repeated.Pending.ID != first.ID || repeated.Revision != snapshot.Revision {
		t.Fatalf("repeated snapshot changed occurrence: first %#v repeated %#v", snapshot, repeated)
	}
	commandsBeforeCachedPending := len(runner.allCommands())
	cachedPending, err := manager.PendingEvent(context.Background(), info.ID)
	if err != nil || cachedPending == nil || cachedPending.ID != first.ID {
		t.Fatalf("cached PendingEvent = event %#v error %v", cachedPending, err)
	}
	if commandsAfter := len(runner.allCommands()); commandsAfter != commandsBeforeCachedPending {
		t.Fatalf("PendingEvent launched %d tmux command(s)", commandsAfter-commandsBeforeCachedPending)
	}

	loadsBefore := len(runner.callsFor("load-buffer"))
	if err := manager.SendEvent(context.Background(), info.ID, "evt-stale", []byte("N\r")); !errors.Is(err, adapters.ErrEventMismatch) {
		t.Fatalf("stale event error = %v", err)
	}
	if got := len(runner.callsFor("load-buffer")); got != loadsBefore {
		t.Fatalf("stale event wrote %d tmux buffers", got-loadsBefore)
	}
	if err := manager.SendEvent(context.Background(), info.ID, first.ID, []byte("Y\r")); err != nil {
		t.Fatalf("resolve first event: %v", err)
	}
	if pending := target.processor.Pending(); pending != nil {
		t.Fatalf("resolved event remains pending: %#v", pending)
	}

	if err := target.processor.Consume([]byte("Overwrite? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	second := contractWaitForPrompt(t, events, info.ID)
	if second.ID == first.ID || second.Sequence != first.Sequence+1 || second.Signature != first.Signature {
		t.Fatalf("successive occurrence = %#v after %#v", second, first)
	}
}

func TestManagerAttachResyncDeduplicatesEventAndResize(t *testing.T) {
	runner := newFakeRunner()
	manager, events := newTestManager(t, runner, Options{
		RunID:         "attach-resync",
		PersistOnExit: true,
		PollInterval:  minimumPollInterval,
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	startSize := terminal.Size{Columns: 80, Rows: 24}
	info, err := manager.Start(context.Background(), testSpec(t, "attached-agent"), startSize)
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.session(info.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := manager.AttachCommand(context.Background(), info.ID); err != nil {
		t.Fatalf("AttachCommand: %v", err)
	}
	commandsBeforeDuplicate := len(runner.allCommands())
	if _, err := manager.AttachCommand(context.Background(), info.ID); err == nil {
		t.Fatal("duplicate AttachCommand succeeded")
	}
	if got := len(runner.allCommands()); got != commandsBeforeDuplicate {
		t.Fatalf("duplicate attach created %d commands", got-commandsBeforeDuplicate)
	}

	const promptText = "Overwrite during attach? [Y/n]"
	if err := target.processor.Consume([]byte(promptText)); err != nil {
		t.Fatal(err)
	}
	suppressed := target.processor.Pending()
	if suppressed == nil {
		t.Fatal("Processor lost event observed during attach")
	}
	assertNoActionableEvent(t, events, info.ID)
	runner.setCapture(promptText)
	if err := manager.Resync(context.Background(), info.ID, startSize.Columns, startSize.Rows); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	reconciled := contractWaitForPrompt(t, events, info.ID)
	if reconciled.ID != suppressed.ID {
		t.Fatalf("Resync replaced occurrence ID: live %#v snapshot %#v", suppressed, reconciled)
	}
	if got := len(runner.callsFor("resize-window")); got != 0 {
		t.Fatalf("same-size Resync issued %d resize commands", got)
	}

	if err := manager.Resync(context.Background(), info.ID, startSize.Columns, startSize.Rows); err != nil {
		t.Fatalf("repeated Resync: %v", err)
	}
	assertNoActionableEvent(t, events, info.ID)
	runner.setCapture("")
	if err := manager.Resync(context.Background(), info.ID, 100, 30); err != nil {
		t.Fatalf("Resync after direct answer: %v", err)
	}
	if pending := target.processor.Pending(); pending != nil {
		t.Fatalf("answered attached prompt remains pending: %#v", pending)
	}
	if got := len(runner.callsFor("resize-window")); got != 1 {
		t.Fatalf("changed-size Resync issued %d resize commands", got)
	}
	if err := manager.Resize(context.Background(), info.ID, terminal.Size{Columns: 100, Rows: 30}); err != nil {
		t.Fatal(err)
	}
	if got := len(runner.callsFor("resize-window")); got != 1 {
		t.Fatalf("repeated resize issued %d resize commands", got)
	}
}

func TestManagerProcessExitIsSingleTypedEvent(t *testing.T) {
	runner := newFakeRunner()
	manager, events := newTestManager(t, runner, Options{RunID: "typed-exit", PersistOnExit: true})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	info, err := manager.Start(context.Background(), testSpec(t, "exit-agent"), terminal.Size{})
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.session(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.processor.Consume([]byte("Overwrite before exit? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	prompt := contractWaitForPrompt(t, events, info.ID)
	if err := manager.Stop(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	exitEvent := waitForExitEvent(t, events, info.ID)
	if exitEvent.Type != adapters.EventProcessExit || exitEvent.Sequence != prompt.Sequence+1 || exitEvent.Adapter != info.Adapter {
		t.Fatalf("process_exit = %#v", exitEvent)
	}
	snapshot, err := manager.Snapshot(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != exitEvent.Sequence || snapshot.Pending != nil {
		t.Fatalf("stopped snapshot = %#v, exit %#v", snapshot, exitEvent)
	}
	if err := manager.Stop(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	assertNoProcessExitEvent(t, events, info.ID)
}

// The screens the resync tests read back. Answered inside tmux, a question is
// followed by the agent's work on it, which is what the pane then shows.
const (
	overwriteQuestion = "Overwrite notes.txt? [Y/n]"
	deleteQuestion    = "Delete the build directory? [y/N]"
	overwriteAnswered = overwriteQuestion + " y\nWrote notes.txt\n"
	deleteAnswered    = deleteQuestion + " y\nRemoved build\n"
)

// A repainting agent asks, then takes its question back off the screen, which
// the live output reports as a withdrawal.
const (
	continueAsked     = "\x1b[2J\x1b[1;1HDo you want to continue? [y/n]\x1b[2;1H  [y] yes\x1b[3;1H  [n] no"
	continueCancelled = "\x1b[2J\x1b[1;1Hrequest cancelled\x1b[2;1H$ "
)

// resyncFixture is a manager over the fake runner with one tmux session, the
// way the TUI holds one around a native attach.
type resyncFixture struct {
	manager *Manager
	events  chan session.Event
	runner  *fakeRunner
	info    terminal.Info
	target  *managedSession
	size    terminal.Size
}

func newResyncFixture(t *testing.T, runID string) *resyncFixture {
	t.Helper()
	runner := newFakeRunner()
	manager, events := newTestManager(t, runner, Options{
		RunID:         runID,
		PersistOnExit: true,
		PollInterval:  minimumPollInterval,
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	size := terminal.Size{Columns: 80, Rows: 24}
	info, err := manager.Start(context.Background(), testSpec(t, "resync-agent"), size)
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.session(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &resyncFixture{manager: manager, events: events, runner: runner, info: info, target: target, size: size}
}

// output feeds the session's Processor as if the agent had printed it.
func (f *resyncFixture) output(t *testing.T, text string) {
	t.Helper()
	if err := f.target.processor.Consume([]byte(text)); err != nil {
		t.Fatal(err)
	}
}

// publish has the agent print a question and returns the prompt the manager
// published for it.
func (f *resyncFixture) publish(t *testing.T, text string) adapters.Event {
	t.Helper()
	f.output(t, text)
	return contractWaitForPrompt(t, f.events, f.info.ID)
}

// attach builds the native client, which holds live prompts back until the
// resync.
func (f *resyncFixture) attach(t *testing.T) {
	t.Helper()
	if _, err := f.manager.AttachCommand(context.Background(), f.info.ID); err != nil {
		t.Fatalf("AttachCommand: %v", err)
	}
}

// resync reads screen back and returns what the resync emitted, in order.
// Its events are essential sends made before it returns, so they are all in
// the channel by then; what was there before is discarded first.
func (f *resyncFixture) resync(t *testing.T, screen string) []string {
	t.Helper()
	drainEvents(f.events)
	f.runner.setCapture(screen)
	if err := f.manager.Resync(context.Background(), f.info.ID, f.size.Columns, f.size.Rows); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	return f.emitted()
}

// emitted describes, in order, every event waiting in the channel.
func (f *resyncFixture) emitted() []string {
	var described []string
	for {
		select {
		case event := <-f.events:
			switch value := event.(type) {
			case session.AdapterEventWithdrawn:
				described = append(described, fmt.Sprintf("withdrawn %s/%s reason=%q",
					value.Event.SessionID, value.Event.ID, value.Reason))
			case session.AdapterEvent:
				described = append(described, fmt.Sprintf("prompt %s/%s", value.Event.SessionID, value.Event.ID))
			case session.OutputAvailable:
				described = append(described, "output "+value.SessionID)
			default:
				described = append(described, fmt.Sprintf("%T", event))
			}
		default:
			return described
		}
	}
}

func (f *resyncFixture) withdrawn(event adapters.Event, reason string) string {
	return fmt.Sprintf("withdrawn %s/%s reason=%q", f.info.ID, event.ID, reason)
}

func (f *resyncFixture) prompt(event adapters.Event) string {
	return fmt.Sprintf("prompt %s/%s", f.info.ID, event.ID)
}

func (f *resyncFixture) outputAvailable() string {
	return "output " + f.info.ID
}

func assertEmitted(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("emitted:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// A prompt answered inside tmux stayed pending with the consumer: Resync
// dropped it from the Processor and told nobody, so the operator was still
// offered an answer to a question the agent no longer asked.
func TestAResyncWithdrawsThePublishedPromptItDiscards(t *testing.T) {
	fixture := newResyncFixture(t, "resync-withdraws")
	published := fixture.publish(t, overwriteQuestion)
	fixture.attach(t)

	emitted := fixture.resync(t, overwriteAnswered)
	assertEmitted(t, emitted,
		fixture.withdrawn(published, session.WithdrawnByResync),
		fixture.outputAvailable(),
	)
	if pending := fixture.target.processor.Pending(); pending != nil {
		t.Fatalf("the answered prompt is still pending: %#v", pending)
	}

	// Once: a second resync of the same screen has nothing left to withdraw.
	assertEmitted(t, fixture.resync(t, overwriteAnswered), fixture.outputAvailable())
}

// The consumer answers a session's oldest prompt first. Published before the
// withdrawal of the prompt it replaced, the new prompt would wait behind a
// question the screen no longer shows.
func TestAResyncThatReplacesThePromptWithdrawsBeforeItPublishes(t *testing.T) {
	fixture := newResyncFixture(t, "resync-replaces")
	published := fixture.publish(t, overwriteQuestion)
	fixture.attach(t)

	emitted := fixture.resync(t, overwriteQuestion+" y\n"+deleteQuestion)
	current := fixture.target.processor.Pending()
	if current == nil || current.ID == published.ID {
		t.Fatalf("scenario broken: the resync did not take up the new question: %#v", current)
	}
	assertEmitted(t, emitted,
		fixture.withdrawn(published, session.WithdrawnByResync),
		fixture.prompt(*current),
		fixture.outputAvailable(),
	)
}

// A prompt held back during the attach is unknown to the consumer. Withdrawn,
// it would be journaled as the withdrawal of a prompt nobody was shown.
func TestAResyncWithdrawsNothingItNeverPublished(t *testing.T) {
	fixture := newResyncFixture(t, "resync-unpublished")
	fixture.attach(t)
	fixture.output(t, overwriteQuestion)
	if fixture.target.processor.Pending() == nil {
		t.Fatal("scenario broken: the question was not detected")
	}
	assertEmitted(t, ofKind("prompt", fixture.emitted()))

	assertEmitted(t, fixture.resync(t, overwriteAnswered), fixture.outputAvailable())
	if pending := fixture.target.processor.Pending(); pending != nil {
		t.Fatalf("the answered prompt is still pending: %#v", pending)
	}
}

// The question is still asked, so the consumer keeps the prompt it has:
// neither withdrawn nor published again, whatever else the screen shows.
func TestAResyncOnAnUnchangedScreenWithdrawsNothing(t *testing.T) {
	fixture := newResyncFixture(t, "resync-unchanged")
	published := fixture.publish(t, overwriteQuestion)
	fixture.attach(t)

	for _, screen := range []string{overwriteQuestion, "Checking notes.txt\n" + overwriteQuestion} {
		assertEmitted(t, fixture.resync(t, screen), fixture.outputAvailable())
		if pending := fixture.target.processor.Pending(); pending == nil || pending.ID != published.ID {
			t.Fatalf("screen %q: pending = %#v, want %s", screen, pending, published.ID)
		}
	}
}

// A prompt held back during the attach is published once the resync keeps it,
// since the live path detects nothing more while one is pending, and it is
// never withdrawn, since the consumer never saw it.
//
// Resync reads what the Processor holds rather than what the reconciliation
// returns: one of its paths, for a screen that repeats a question answered
// earlier, keeps the held prompt and returns nil. Every built-in adapter
// already drops the answered line itself, so that path is not reached: the
// held prompt goes with a screen that no longer shows it, and the consumer,
// told nothing, agrees with the Processor.
func TestAResyncPublishesTheHeldPromptOnlyWhileTheProcessorHoldsIt(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		answered bool
		screen   string
		kept     bool
	}{
		{name: "still asked", screen: deleteQuestion, kept: true},
		{name: "answered inside tmux", screen: deleteAnswered},
		{name: "the screen repeats a question answered earlier", answered: true, screen: overwriteQuestion},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newResyncFixture(t, "resync-held")
			if scenario.answered {
				earlier := fixture.publish(t, overwriteQuestion)
				if err := fixture.manager.SendEvent(context.Background(), fixture.info.ID, earlier.ID, []byte("Y\r")); err != nil {
					t.Fatalf("answering the earlier question: %v", err)
				}
				// The agent goes on to its next line.
				fixture.output(t, "\r\n")
			}
			fixture.attach(t)
			fixture.output(t, deleteQuestion)
			held := fixture.target.processor.Pending()
			if held == nil {
				t.Fatal("scenario broken: the question asked during the attach was not detected")
			}
			assertEmitted(t, ofKind("prompt", fixture.emitted()))

			emitted := fixture.resync(t, scenario.screen)
			pending := fixture.target.processor.Pending()
			if !scenario.kept {
				assertEmitted(t, emitted, fixture.outputAvailable())
				if pending != nil {
					t.Fatalf("the Processor holds %#v, which the consumer was never told of", pending)
				}
				return
			}
			if pending == nil || pending.ID != held.ID {
				t.Fatalf("pending = %#v, want the held prompt %s", pending, held.ID)
			}
			assertEmitted(t, emitted, fixture.prompt(*held), fixture.outputAvailable())
		})
	}
}

// Live output can take back a prompt held during the attach. The consumer
// never saw it, so the withdrawal is not published; a published prompt's is,
// with the agent's reason.
func TestALiveWithdrawalOfAnUnpublishedPromptIsNotEmitted(t *testing.T) {
	fixture := newResyncFixture(t, "live-withdrawal")
	published := fixture.publish(t, continueAsked)
	drainEvents(fixture.events)
	fixture.output(t, continueCancelled)
	assertEmitted(t, ofKind("withdrawn", fixture.emitted()), fixture.withdrawn(published, ""))

	fixture.attach(t)
	fixture.output(t, continueAsked)
	if fixture.target.processor.Pending() == nil {
		t.Fatal("scenario broken: the question asked during the attach was not detected")
	}
	fixture.output(t, continueCancelled)
	if pending := fixture.target.processor.Pending(); pending != nil {
		t.Fatalf("scenario broken: the agent's cancellation left %#v pending", pending)
	}
	// The output's hooks run on the caller's goroutine, so all it emitted is
	// in the channel already.
	assertEmitted(t, ofKind("withdrawn", fixture.emitted()))
}

// A repainting agent can take back a prompt published before the attach while
// the terminal is still attached. Its withdrawal is published there and then,
// as it would be unattached: the resync that ends the attach finds nothing
// pending, so a withdrawal held back until then would reach nobody, and the
// consumer would keep offering an answer to a question the agent no longer
// asks.
func TestAPublishedPromptTakenBackWhileAttachedIsWithdrawnLive(t *testing.T) {
	fixture := newResyncFixture(t, "withdrawn-while-attached")
	published := fixture.publish(t, continueAsked)
	fixture.attach(t)
	drainEvents(fixture.events)
	fixture.output(t, continueCancelled)
	if pending := fixture.target.processor.Pending(); pending != nil {
		t.Fatalf("scenario broken: the agent's cancellation left %#v pending", pending)
	}
	assertEmitted(t, ofKind("withdrawn", fixture.emitted()), fixture.withdrawn(published, ""))

	// The resync that ends the attach has nothing left to withdraw.
	assertEmitted(t, fixture.resync(t, "request cancelled\n$ "), fixture.outputAvailable())
}

// Once a resync's reconciliation has dropped the prompt the screen no longer
// shows, the output reader, which does not take the lock a resync reconciles
// under, can publish the agent's next question before the resync withdraws
// the first. The resync asked whether the first was the last prompt published
// only then, took it for one it never published, and withdrew nothing: the
// consumer kept a question the agent no longer asked beside the new one, and
// offered it to the operator and the policy until an answer was refused as
// stale.
func TestAResyncWithdrawsItsPromptEvenOnceTheOutputHasPublishedTheNext(t *testing.T) {
	fixture := newResyncFixture(t, "resync-overtaken")
	published := fixture.publish(t, overwriteQuestion)
	fixture.attach(t)
	fixture.manager.resyncReconciled = func() {
		fixture.output(t, "\r\nWrote notes.txt\r\n"+deleteQuestion)
	}

	emitted := fixture.resync(t, overwriteAnswered)
	next := fixture.target.processor.Pending()
	if next == nil || next.ID == published.ID {
		t.Fatalf("scenario broken: the output did not raise the next question: %#v", next)
	}
	assertEmitted(t, emitted,
		fixture.outputAvailable(),
		fixture.prompt(*next),
		fixture.withdrawn(published, session.WithdrawnByResync),
		fixture.outputAvailable(),
	)
}

// The same race the other way round. The output's withdrawal hook runs once
// the Processor has let go of its lock, and a resync in between finds nothing
// pending and publishes the question it finds on the screen. The output's
// prompt is then no longer the last one published: taken for a prompt nobody
// was shown, its withdrawal was dropped, and the consumer kept it beside the
// new one.
func TestTheOutputWithdrawsItsPromptEvenOnceAResyncHasPublishedTheNext(t *testing.T) {
	fixture := newResyncFixture(t, "output-overtaken")
	published := fixture.publish(t, continueAsked)
	fixture.attach(t)
	// Nothing stops the output between finding the prompt gone and its hook,
	// so the Processor drops the prompt here without calling the hook, and
	// the hook's claim is made directly once the resync has run.
	if _, _, err := fixture.target.processor.ReconcileSnapshot([]byte("request cancelled\n$ ")); err != nil {
		t.Fatal(err)
	}

	emitted := fixture.resync(t, deleteQuestion)
	next := fixture.target.processor.Pending()
	if next == nil {
		t.Fatal("scenario broken: the resync did not take up the new question")
	}
	assertEmitted(t, emitted, fixture.prompt(*next), fixture.outputAvailable())
	if !fixture.target.claimWithdrawal(published.ID) {
		t.Fatal("the output's withdrawal of a published prompt was dropped")
	}
}

// The output reader does not take the lock a resync reconciles under, so both
// can find one prompt gone: the output withdraws the prompt a resync has just
// read as pending, and the resync then finds it missing from the screen. The
// consumer journals every withdrawal it is told of, so only the first is
// published, whichever of the two it is.
func TestAPromptTheOutputAndAResyncBothFindGoneIsWithdrawnOnce(t *testing.T) {
	t.Run("the output first", func(t *testing.T) {
		fixture := newResyncFixture(t, "withdrawn-by-the-output-first")
		published := fixture.publish(t, continueAsked)
		fixture.attach(t)
		fixture.manager.resyncRead = func() { fixture.output(t, continueCancelled) }

		emitted := fixture.resync(t, "request cancelled\n$ ")
		assertEmitted(t, ofKind("withdrawn", emitted), fixture.withdrawn(published, ""))
	})
	t.Run("the resync first", func(t *testing.T) {
		fixture := newResyncFixture(t, "withdrawn-by-the-resync-first")
		published := fixture.publish(t, continueAsked)
		fixture.attach(t)

		emitted := fixture.resync(t, "request cancelled\n$ ")
		assertEmitted(t, ofKind("withdrawn", emitted), fixture.withdrawn(published, session.WithdrawnByResync))
		// The output's withdrawal hook runs once the Processor has let go of
		// its lock, so output that found the prompt gone while the resync was
		// reconciling can claim after it. Nothing stops the output between
		// the two, so its claim is made directly.
		if fixture.target.claimWithdrawal(published.ID) {
			t.Fatal("the output withdrew the prompt the resync had already withdrawn")
		}
	})
}

// A session whose process has ended withdraws nothing: its exit ends every
// prompt it had. The monitor publishes the exit a poll after the one that
// finds the pane dead, and a resync that claims in between withdraws nothing
// either.
func TestAResyncWithdrawsNothingOnceTheProcessHasEnded(t *testing.T) {
	fixture := newResyncFixture(t, "resync-after-the-exit")
	fixture.publish(t, overwriteQuestion)
	fixture.attach(t)
	fixture.manager.resyncReconciled = func() {
		// What the monitor does on the poll that first finds the pane dead,
		// done here so that it lands before the claim.
		fixture.runner.setDisplay("1|0|0|0\n")
		fixture.target.updateProcessExitState(Snapshot{ID: fixture.info.ID, Status: StatusExited, ExitCode: intPointer(0)})
	}

	assertEmitted(t, ofKind("withdrawn", fixture.resync(t, overwriteAnswered)))
	waitForExitEvent(t, fixture.events, fixture.info.ID)
}

// ofKind keeps the described events of one kind: "withdrawn" or "prompt".
func ofKind(kind string, described []string) []string {
	var kept []string
	for _, line := range described {
		if strings.HasPrefix(line, kind+" ") {
			kept = append(kept, line)
		}
	}
	return kept
}

func assertNoActionableEvent(t *testing.T, events <-chan session.Event, id string) {
	t.Helper()
	deadline := time.NewTimer(40 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if adapterEvent, ok := event.(session.AdapterEvent); ok &&
				adapterEvent.Event.SessionID == id && adapterEvent.Event.Actionable() {
				t.Fatalf("unexpected actionable event: %#v", adapterEvent.Event)
			}
		case <-deadline.C:
			return
		}
	}
}

func assertNoProcessExitEvent(t *testing.T, events <-chan session.Event, id string) {
	t.Helper()
	deadline := time.NewTimer(40 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if legacy, ok := event.(session.Exited); ok && legacy.SessionID == id {
				t.Fatalf("legacy Exited duplicated process_exit: %#v", legacy)
			}
			if adapterEvent, ok := event.(session.AdapterEvent); ok &&
				adapterEvent.Event.SessionID == id && adapterEvent.Event.Type == adapters.EventProcessExit {
				t.Fatalf("duplicate process_exit event: %#v", adapterEvent.Event)
			}
		case <-deadline.C:
			return
		}
	}
}

var _ terminal.EventSender = (*Manager)(nil)
var _ terminal.Backend = (*Manager)(nil)
