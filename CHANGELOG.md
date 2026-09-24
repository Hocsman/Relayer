# Changelog

All notable user-visible changes are documented here. This file follows the structure of [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

Patch release that moves the web gateway onto the supervision core the desktop has run on since v0.8.8, and closes the security weaknesses of the gateway's own copy. `relayer serve` supervised prompts with a second state machine that had drifted from the desktop's: it never delivered the policy's automatic decisions, went on writing to agents after its audit journal failed, showed viewers a prompt's raw text, a backend's raw error and a tool call's arguments, sent a typed "y" as the adapter's allow, let any operator connection type into a terminal nobody held, and carried the previous run's prompts and terminals into the run "Save and restart" started. The gateway now decides, delivers, journals, refuses and freezes as the desktop does; keystrokes need the terminal's hand and take their turn with answers and lines; every request that acts on a run names it; and a run ends only once what it was writing is journaled, or is reported failed. A review of the core found faults the desktop had too, among them an uncertain write that left its session writable, a deny held back by a limit that offered Allow, and webhooks that received a password prompt's text; they are fixed for both. The web interface, which the desktop also serves, no longer opens the decision modal over a prompt the policy is answering, and says why each prompt was asked. The TUI keeps its own state machine and is unchanged. Each fix has a test that fails without it; the gateway's tests drive a real gateway through its websocket, against real agent processes. The gaps that remain are listed at the end of "Changed".

### Security

- **The web gateway kept writing to agents after its audit journal failed**: every journal write's result was discarded, so an answer whose `decision` entry could not be written was delivered anyway, lines and keystrokes went on, and the gateway went on reporting the journal ready. The journal now fails closed as on the desktop: an entry it refuses stops every answer, the policy's included, every line, every keystroke and every new attach, each pending prompt is shown failed (`audit_unavailable`), and every client is told the journal failed. An agent can still be stopped. `TestAWebJournalThatFailsStopsEveryAnswer` fails against v0.8.8.
- **Viewers received the gateway's prompt cards unredacted**: a card carried the adapter's raw summary of the prompt, and was marked confidential only when the adapter had flagged it, so the text of a credential prompt, and a tool call's arguments, a token passed as `auth:ghp_…` included, reached every client, viewers included, and every webhook. A backend stream error was broadcast as its raw text, which can name a host path or quote a credential, although viewers are promised no host path "not in an error message either". Cards, tool-call badges and notifications are now the core's display-safe views: one line, bounded and redacted, no text for a prompt whose text must not be shown and no badge for it, and a tool call's values redacted one by one. A stream error is journaled `backend_error` and shown as a fixed message. `TestTheWebGatewayShowsAToolCallOnlyAsItMayBeShown` and `TestTheWebGatewayReportsAStreamErrorByAFixedMessage` fail against v0.8.8. Terminal output is not redacted, for viewers either, which `docs/web-gateway.md` now says plainly.
- **An answer on the gateway could be lost, or followed by a second one**: the gateway removed a prompt before writing its answer and handled every failure alike. A refused, stale or failed answer took the prompt away on the server while every screen went on offering it; an answer whose write was uncertain, which may have reached the agent in part, froze nothing, so another answer could be written after it; and nothing kept two answers to one agent from being written at once. A prompt now stays until its answer's outcome is journaled, shown delivering meanwhile; an uncertain write freezes its session; an answer the adapter cannot encode goes back to the operator; and each session takes one write at a time. `TestAnUncertainWebWriteFreezesTheSession`, `TestTheWebGatewayKeepsAPromptOfferedUntilItsAnswerIsWritten` and `TestTheWebGatewayWritesOneAnswerAtATimeToASession` fail against v0.8.8.
- **The gateway read a typed answer as a button**: "y", "yes" and "allow" typed into the answer field were sent as the adapter's allow and journaled as a person allowing the prompt, and "n", "no" and "deny" as its deny. "Y" typed into an Aider prompt sent the adapter's own `y`, and "y" typed into a generic prompt was refused. Any other text was journaled `allow`, and an empty answer was taken, which the generic adapter sends as a bare Enter: the prompt's default answer. The call behind the Allow and Deny buttons took any string and sent it as typed. A typed answer is now sent as typed and journaled `ask`, an empty one is refused, and the buttons send only allow or deny, and only one the prompt offers. `TestAWebTypedAnswerReachesTheAgentAsTyped`, `TestAWebChosenAnswerIsOnlyOneThePromptOffers`.
- **Any operator connection could type into a terminal nobody held**, through the RPC and through binary frames, with no connection named at all, whatever else was being written to it. Keystrokes never resolve the prompt Relayer is waiting on, so once the policy answers on its own, keystrokes typed without the terminal land beside its answer to the same question, and nothing in the journal said anybody had been at the terminal. A keystroke is now admitted only from the connection that holds the terminal, whose taking is journaled first, and through the session's one write slot: it is refused while an answer or a line is being written, and those wait for it or are refused; once the journal has failed; on a frozen, stopped, stopping or starting session; and while the run ends. Keystrokes are still never journaled. `TestAWebTerminalNobodyHoldsTakesNoKeystrokes`.
  - A prompt its terminal's holder had answered by typing stayed answerable under the same ID, and a click on its card, by the holder or any operator, wrote a second answer into whatever the agent asked next. Every prompt shown while the holder's keystrokes are written is now the terminal's (`typed_at_terminal`): it offers no answer and takes nobody's, the policy's included, until the agent withdraws it or its process ends. `TestAPromptItsHolderTypedIntoIsTheTerminals`.
  - A typed answer, which needs no terminal and is taken from any operator while another holds it, could carry carriage returns, line feeds, escape sequences and control keys of any length: several answers, or keystrokes of any kind. It is now held to a line's rule, one line of at most 4096 bytes with no control character, on the desktop too. `TestATypedAnswerIsOneLineOfTextAsALineIs`.
  - An attach was granted before its record was written, and a failed write ignored, so a holder could type before the journal said who held the terminal, or with no record at all. `attach_started` is now written first, and the attach refused when it cannot be. A terminal given a new holder by a request, a grant or a forced takeover is journaled before the hand moves, and a record the journal refuses freezes the run before any keystroke. `TestAWebAttachIsJournaledBeforeTheTerminalIsHeld`.
- **A tab left on a replaced run acted on the run that replaced it**: "Save and restart" carried the previous run's prompts and terminals into the new run, so a connection went on typing into a process it had never attached to, with no attach of it in the new run's journal. Lines, a Stop, Start or Restart of an agent, stopping the run, "Save and restart" and the terminal verbs ignored the run their request named: a tab still showing the previous run sent lines to, stopped, started and restarted the new run's agents, which it had never shown, and took their terminals. An answer naming no run was taken for the current one. Each run now has its own core and holds no terminal when it starts; ending a run lets go of its terminals, journaled `control_released_run_end`; and every request that acts on a run names it, and is refused for another run or for none, with nothing changed. `TestAWebRequestActsOnlyOnTheRunItNames`, `TestAWebRunStartedAgainKeepsNothingOfThePreviousRun`, `TestAWebTerminalIsTakenOrHandedOverOnlyForTheRunItNames`.
- **Ending a run cut off the answer being written**: stopping the run, "Save and restart" and shutdown closed the runtime at once, under the lock every other request waits on, so an answer being written lost the entry that says how it ended, the journal's last word on it being that it was being delivered. A run now ends in the desktop's order: nothing new is admitted, the agents are stopped, what was being written reaches its journaled outcome, the terminals are let go, and only then is the runtime closed, each step within 12 seconds. A run that did not end cleanly is reported failed, and no other run is started beside processes that may still run until Relayer is restarted. `TestEndingAWebRunWaitsForTheAnswerBeingWritten`.
- **Lines on the gateway bypassed supervision**: a line was written with no check for a prompt waiting on the operator, a held terminal, a frozen session or a failed journal, and journaled only afterwards, its errors ignored, so a line typed while a prompt waited answered it outside the policy and the journal. A line now goes through the core as on the desktop: journaled before it is written, refused in each of those cases, and written one at a time with answers and keystrokes. `TestAWebLineIsJournaledFirstAndWaitsForTheQuestionToBeAnswered`.
- **The shared core let a write follow or overlap one it should have waited for**, on the desktop as on the gateway:
  - an answer whose write was uncertain froze nothing when the agent withdrew its prompt during the write, which is when an answer's echo is read, so the next prompt's automatic answer was written after the uncertain one (`TestAnUncertainWriteFreezesTheSessionEvenWhenItsPromptWasWithdrawn`);
  - that withdrawal also released the session, and the next automatic answer was written into the terminal while the first still was (`TestWithdrawingTheDeliveringPromptKeepsTheSessionUntilItsWriteReturns`);
  - a process that exited during a write released the session at once, and a Start could launch the replacement, whose first automatic answer was written beside the old write. A Start and a Restart are now refused while anything is still being written to the session (`TestAnExitLeavesTheSessionToTheWriteStillInProgress`);
  - an answer, a person's or the policy's, could be journaled and written after the agent withdrew the question, and a withdrawal that arrived while its prompt was still being taken in was lost: the prompt waited on the operator for a question no longer asked and held up every automatic answer of its session (`TestAPromptBeingWithdrawnIsNobodysToActOn`, `TestAWithdrawalWhileItsPromptIsTakenInIsNotLost`).
- **A deny the policy was kept from delivering became a question with Allow on offer**: when a limit, a repeat, a held terminal or an adapter that could not encode the deny sent a prompt the policy denies to the operator, the prompt offered every answer, and any operator could allow what a deny rule refuses, by the button or by typing the adapter's accept. It now offers Deny alone, and Allow and any typed answer are refused with nothing journaled. An answer the adapter had failed to encode, which the prompt no longer offered, was also accepted again from a stale screen or a second operator; an answer the prompt does not offer is now refused. `TestADenyTheCoreHoldsBackOffersOnlyDeny`, `TestAnAnswerThePromptNoLongerOffersIsRefused`.
- **`max_consecutive_auto_decisions` could be exceeded**: two prompts queued on one session were both evaluated automatic when detected, before either decision was journaled, and both were answered, since the limit counts journaled decisions. The policy is now asked again just before its decision is journaled, and a prompt it no longer answers goes to a person, notified, with a second `policy_evaluated` entry that says why. `TestThePolicyIsAskedAgainJustBeforeItsDecision`.
- **The policy answered a question it had just answered** whenever an adapter read the question again, from its answer's echo or a repaint, under a new ID: a second answer, which the agent took for whatever it asked next. v0.8.8 fixed the adapters for the cases known; the core now has a backstop of its own. A prompt the policy would answer, repeating one of its session whose answer, a person's or the policy's, is being written or was written less than two seconds earlier, is asked instead (`repeat_after_delivery`) and notified. On the gateway the holder's keystrokes count as such an answer. The cost is that a genuine question asked twice within two seconds goes to a person.
- **Webhooks received the text of a prompt that asked for a password**: the desktop's notifications, and the gateway's, carried the adapter's raw summary of the prompt, which the webhook notifiers post as it is to Slack, Discord or any configured URL, even for a prompt marked sensitive. A notification now carries the summary the prompt is shown with: one line, redacted, and a fixed text for a prompt whose text must not be shown. `TestANotificationCarriesThePromptsSafeSummary`.

### Fixed

- **The web gateway never delivered the policy's automatic decisions**: every prompt waited on an operator, whose click was journaled as a person's decision, and `max_consecutive_auto_decisions` and `rate_limit_per_minute` counted nothing. The gateway now delivers them as the desktop does. `TestTheWebGatewayAnswersAnAutomaticQuestionOnce` puts a real Aider question to a real gateway and fails against v0.8.8.
- **The gateway's journal left out most of what happened**: no detection, no policy evaluation, no withdrawal, no `session_finished` for a process that exited on its own, and no `backend_error` for a stream failure. It now writes the same entries as the desktop. `TestTheWebJournalSaysWhatBecameOfEachPrompt` fails against v0.8.8.
- **The gateway's metrics measured nothing of its prompts**: with no detection journaled, `relayer_events_pending` stayed at zero, the decision-duration histogram stayed empty, `relayer_decisions_total` counted people only, and `relayer_sessions_active` never went down for an agent that exited on its own. They now measure what they measure on the desktop.
- **Every automatic decision was counted twice, and prompts nobody had answered as decided**: `relayer_decisions_total` counted each `policy_evaluated` entry as well as each `decision`, so a prompt a person answered counted once as the policy's `ask` and once as theirs, and a prompt still waiting counted as decided. The Metrics panel of the desktop and the web interface, which reads the same counter, showed every prompt the policy handed to a person as an automatic deny. The counter now counts decision entries only, the policy's under the rule that made them. `TestEachDecisionIsCountedOnce`.
- **`relayer_sessions_active` read one short, or zero, while agents ran**: an operator Stop and the exit of the process it stopped, a Restart whose previous process's exit arrived after the replacement started, and a run that ended after an exit each ended the session twice. A session is now counted once, from its start to its first end (`TestEachSessionIsCountedActiveOnce`). The late exit of a replaced process is journaled with its real outcome, failed when it failed, under the reason `process_exit_stale`, rather than as a clean end of the running session.
- **Notifications**: the gateway notified every prompt as awaiting a decision, the policy's own included, under the agent's ID. It now notifies, by the agent's name, only a prompt that waits on a person, and a guardrail as critical (`TestTheWebGatewayNotifiesOnlyWhatWaitsOnAPerson`). On both front ends, a prompt the policy was to answer that went to a person after it was detected, at the policy's last check or because no adapter encoded its answer, waited in silence: a limit, which exists to bring a person in, stalled the agent. It is now notified (`TestAPromptHandedBackAfterItsDetectionIsNotified`).
- **Gateway screens fell behind the server**: a prompt the agent withdrew stayed on every screen until a reload; an agent was shown running after an answer while other prompts waited, and waiting once it was dead; a Stop, Start or Restart showed no stopping or starting, and left writable a session whose stop had failed; and a finished recording was offered for replay only after a reload, unless Relayer had lost its tmux session. Each is now shown as on the desktop, and a recording is announced when its process ends. An output snapshot or an attach status read just before a status change no longer has the last word over it.
- **An exit read while a Restart was between its stop and its start was applied after the replacement started**: the agent was shown failed while its new process ran, a `session_finished` followed the new process's `session_started`, the new process's prompts were dropped as a stopped agent's, and a Stop or a Start was refused. It is now judged once the Restart ends. The desktop escaped it by timing; on Linux the gateway met it on every restart once it ran on the core. A replacement that dies during its own start is now shown exited, not running. `TestAnExitThatArrivesDuringARestartIsJudgedOnceItEnds`.
- **Faults of the core the desktop shared**: a person's answer the adapter cannot encode, which reached nothing, froze the session as if it might have, and now goes back to the operator without that answer; an agent could not be stopped once the journal had failed, and now can; a failed journal showed one prompt failed, and now shows every pending one; a Start that kept a prompt its new process raised showed the agent running, and a prompt raised during a Stop showed it waiting; a prompt could be shown delivering after it was shown delivered, and stay on screen, and an agent running after it was shown exited; and a failed Start, Restart or Stop was shown under the ID as its caller spelled it, which left a start asked in another case showing "starting" on every screen.
- **Keystrokes to an agent that reads nothing blocked for good**: once its terminal's input buffer was full, about 20 KiB, a keystroke write blocked in the kernel whatever its five-second context said, and the connection that typed was no longer read. On Linux the write now gives up after five seconds, having written part of what was typed or none of it, and a Stop or the run's end cuts it short. A Stop is taken while keystrokes are being written, since it writes nothing and is what ends such an agent, and the run's end waits at most 12 seconds for them.
- **A detach from a terminal nobody held** journaled `attach_finished` each time the interface closed a terminal view; only the end of an attach that began is journaled now.
- **The web interface, shared by the desktop**:
  - a prompt's answers were not locked while it was being delivered: Allow, Deny, the text field, `Ctrl+Enter` and `Esc` stayed live, and `Esc` pressed on a prompt the policy was answering raced the server's own answer. They are now locked while any answer to the prompt is being delivered, while the policy is answering it, and once keys were typed at its terminal; `Esc` then only minimizes the modal;
  - after any refused answer the prompt was locked as "delivery indeterminate", even when nothing had been sent, with nothing the operator could do to clear it. The interface now shows a fixed message for the refusal, never the raw error, and reads the state again;
  - a tab kept whatever it had missed while disconnected, a lost "delivered" included, and showed a ghost prompt; nor did it follow another tab's "Save and restart". It now reads the state again on every reconnection and every run-level status;
  - an answer that timed out while the socket was down stayed queued and went out on reconnection, possibly minutes after the operator was told it had failed. It is now dropped;
  - a late frame could bring back a prompt already shown delivered; it no longer can.

### Changed

- **A web terminal nobody holds takes no keystrokes**: an operator takes it first, which the bundled interface does when the interactive session is opened. A client that typed without attaching must now attach (`setInteractiveSession`).
- **While somebody holds an agent's web terminal, the policy answers nothing on it**: each automatic prompt of that agent, and each one raised meanwhile, is asked with the reason `operator_attached`, and stays asked once the terminal is released. A line is refused while anybody holds the terminal. Answers are still taken from any operator, as `docs/sharing.md` says.
- **A prompt shown while its holder typed is the terminal's**, and any keystroke counts, including the focus and mouse reports a browser terminal sends by itself to an agent that asked for them: clicking into such an agent's terminal is enough.
- **Typed answers and the buttons on the gateway**: a typed answer is sent as typed and journaled `ask`, and `submitAutomaticDecision` takes only `allow` or `deny`, and only one the prompt offers; anything else is refused with nothing journaled.
- **A prompt that offers Deny alone takes no typed answer**, on the desktop as on the gateway. The generic and Claude adapters encode no deny, so a prompt of theirs that the policy would deny on its own, as a deny rule or `default_action: deny` does for a valid non-sensitive confirmation, can no longer be answered through Relayer on either: its web terminal's holder answers it by typing, on the gateway, or its agent is stopped. The TUI still asks it without restriction.
- **Every request that acts on a run names it**: answers, lines, a Stop, Start or Restart, stopping the run, "Save and restart" (`expectedRunID`, which the gateway used to ignore) and asking for, handing over or seizing a terminal. A script that calls the gateway's API must send the `runID` that `getState` returns; a request naming none is refused.
- **A Start or a Restart is refused while something is still being written to the agent** ("a decision is already in progress for this agent"). A Stop is refused while an answer or a line is, whose journaled outcome would be lost with the process, and taken while the holder's keystrokes are.
- **The journal**: a person's `decision` and `delivery` on the gateway carry the connection (`conn_id`) beside the operator and role, as metadata that only the `detailed` mode keeps; the default `metadata` mode keeps the `operator` field alone. A line's two `operator_input` entries name the operator and nothing else. The policy's `decision` entries carry the rule that made them. The gateway's lines are journaled before they are written as well as after.
- **Decision totals drop** on the bundled Grafana dashboard and in the Metrics panel: "Total Decisions", decisions by action and decisions per minute read about half what they did for automatic decisions, and no longer count prompts nobody answered.
- **The web interface lists a prompt the policy is answering instead of opening it**, as "Policy · Allow" or "Policy · Deny", queued or delivering, and opens it only if it comes back to a person. The pending counts and the top bar leave out such a prompt and any prompt being delivered; the agent card's waiting badge still counts them, since a line is refused while any prompt waits. Every prompt says in one line why it was asked, the web's equivalent of the TUI's `LIMIT • ASK` tag.
- **What remains**:
  - keystrokes are never journaled, and a web terminal's holder types past detection and policy by design; after the terminal changes hands, at most one write already admitted, a keystroke or a paste, can still land;
  - the control records and `attach_finished` are best effort: the action goes ahead when its record cannot be written, though the run is frozen first;
  - a terminal is not let go when its agent's process exits, only when its holder releases it or disconnects, or the run ends;
  - a "Save and restart" whose new run fails to start is not rolled back on the gateway, and a run that did not end cleanly blocks every further run until Relayer is restarted;
  - a Stop, Start or Restart is journaled as a person's without saying whose, on the gateway too; the core refuses a viewer on answers and lines, and only the gateway's allowlist keeps viewers off the rest;
  - terminal output reaches every client verbatim, viewers included;
  - a client whose frames the gateway dropped for being slow is not resynchronized until it reconnects or a run-level status arrives, and two state reads can arrive out of order;
  - output is broadcast on every change, without the desktop's coalescing;
  - on Unix systems other than Linux, a keystroke write to an agent that reads nothing may still block until the agent reads again or exits; the run's end waits at most 12 seconds for it;
  - the TUI has none of the core's guards: no repeat guard and no Deny-alone rule, and a line there resets the count of consecutive automatic decisions.

### Documentation

- `docs/web-gateway.md` never said how the gateway supervised prompts; it now says it runs the desktop's core, that requests name their run, and how a run ends. It said a named token's identity was recorded for every decision: the policy's decisions, and a Stop, Start or Restart, name nobody.
- `docs/audit.md` listed a subset of the kinds, said `session_finished` came only from a process exit and `supervision_finished` from the TUI, and did not document the `operator` field. It now says which front end writes which kind, every reason `session_finished` carries, which reasons the core adds, and that the role and connection are kept only in `detailed` mode. `docs/architecture.md` no longer says every lifecycle transition attributes the operator.
- `docs/configuration.md` said a direct line resets the count of consecutive automatic decisions, true of the TUI only, and that the limits are tagged `LIMIT • ASK`, which only the TUI shows.
- `docs/observability.md` gave `relayer_decisions_total` and `relayer_guardrail_violations_total` labels they never had and called every detection a sensitive event; it now gives the real labels, and says two panels of the bundled dashboard group by labels their metric does not carry and that the metrics need the audit journal enabled.
- `README.md` and `docs/security-model.md` said a stop interrupts a blocked write to an agent's terminal on Unix. With the terminal in blocking mode, the write ended only once every process holding the terminal had exited; on Linux stopping the agent now ends it. `docs/sharing.md` said a resize from anyone but the holder is ignored; a free terminal takes any operator's.

## [0.8.8] - 2026-09-24

Patch release that stops Relayer answering one prompt twice. Once an answer was delivered, the answered question stayed painted on the rendered screen, which every Windows session and every agent that repaints uses, and the next write raised it again as a new prompt with a new ID. Under an automatic policy the desktop typed a second answer into the agent; a human was shown a card for a question already answered, and while it was pending the agent's next real question stayed hidden. Adapters now remember an answered question on its row, across a clear, a full-screen program and a resize. The desktop's supervision now runs on a shared core, which the web gateway adopts in v0.8.9. The gaps that remain are listed below.

### Security

- **The desktop answered one prompt twice under an automatic policy**: after the answer, the question's row still showed the question, with the answer echoed after it. Aider, Goose and Open Interpreter kept no memory of an answered question at all, and the generic and Claude adapters compared the whole line, which the echo changes, so the next write raised the same question again. With `default_action: allow`, an Aider prompt received two `y`: `TestTheDesktopAnswersAnAutomaticQuestionOnce` drives the real desktop runtime and fails against v0.8.7 on Windows and on Linux. Codex was not affected. For a human the repeat was a card for a question already answered, and Aider's detector, which waits while a prompt is pending, hid the agent's next real question behind it.
  - An answered question is now remembered on its row: a line on that row that still begins with the question is that question with its echo. The same words on another row are a new question, and are asked; the generic and Claude adapters used to swallow them while the answered row still showed the question.
  - The same second answer came from a full-screen program run after the answer (an editor, a pager): the memory was dropped while the program had the alternate screen, and the question still painted underneath was asked again when it exited. It came from a terminal that lost height, where the screen kept its top rows instead of the cursor's, and from a window resized while such a program was open. It came from an answer given on the program's own screen in the same words, which took over the primary screen's memory. Each is covered by a test that fails against v0.8.7.

### Fixed

- **Aider, Goose and Open Interpreter ask again an identical question after a clear**: once the echo was remembered, a question drawn above the answered row after `clear`, a command that clears the screen or Ctrl+L was taken for the answered one moved, and the agent waited for an answer nobody was asked for. These adapters remember an answered question on its own row only.
- **A tmux resync no longer discards a pending question** it took for the answered one (TUI).
- **The TUI keeps the cursor's row in view when the terminal loses height**, as xterm and conhost do; it used to drop that row and every row below it.
- **Still asked twice on some Windows versions**: the full-screen program fix needs the switch to the alternate screen to reach Relayer. The ConPTY of Windows 11 passes it on; that of Windows Server 2022, and presumably of Windows 10, paints the program over the screen and paints the screen back when it exits, and the answered question is then asked again, as in v0.8.7. Under an automatic policy that is still a second answer typed into the agent there. The tests for it are skipped on such a ConPTY, with the reason.
- What remains, described in `docs/adapters.md`: an answer an agent rejects and asks again on the same row, with the rejected input still showing, is taken for the echo and not asked; the generic and Claude adapters still lose an identical question drawn above a blank answered row after a clear; the tmux snapshot compares whole lines; and a question moved off its row, or whose line changes after it was detected, is asked again.

### Changed

- The desktop's supervision state machine moved to a new package, `internal/supervise`, with no change of behaviour: the desktop's tests pin it, and new tests drive the core through a fake engine. A mutation run that broke its invariants one by one found 47 of 101 that no test caught; with the tests this release adds, 3 remain, each equivalent to a guard that stays in place.
- **Known gap**: the web gateway (`relayer serve`) does not deliver automatic policy decisions, so every prompt waits for an operator, and its journal has no detection, evaluation, withdrawal or process exit entries. v0.8.9 moves it onto the shared core.

## [0.8.7] - 2026-09-23

Patch release that fixes the cause of the ghost prompts v0.8.6 worked around. A prompt's occurrence ID did not tell two processes of an agent apart, so an answer given to a prompt before a restart could be delivered to a different question after it, whenever a short pattern gave both the same signature. Every occurrence ID now carries a token of its own process, a decision on a previous process's prompt is refused, and the front ends no longer need to forget answered prompts at each start. `relayer_events_pending` and the decision-duration histogram now measure what their documentation says. The gaps that remain are listed below.

### Security

- **A decision on a prompt of a previous process could be delivered to its replacement**: a prompt's occurrence ID was its signature and its number within the process, and the number starts again with each process. A signature can be as little as the `[y/n]` a pattern captured, so a restarted agent's first prompt could carry exactly the ID of the previous process's, whatever each one asked: "Run 'npm test'? [y/n]" and "Run 'rm -rf build' as root? [y/n]" had the same ID, and the processor accepted an answer given to the first as the answer to the second. Every occurrence ID now carries a random token drawn when the agent's process starts, as process exits already did, and a decision naming the previous process's prompt is refused. The token is never recorded, so the IDs of sensitive prompts, withheld from the journal because they derive from the match, stay unguessable.
  - All six adapters build their IDs through one helper, including the five that rebuild the ID after the generic detection, and a test fails if code in the adapters package builds an ID without it. `TestEveryAdapterGivesEachProcessItsOwnPromptIDs` and `TestAProcessRefusesADecisionOnItsPredecessorsPrompt` fail against v0.8.6.
  - An occurrence ID is no longer the same across restarts of an agent. Nothing in Relayer relied on that; a consumer of the journal or of webhook payloads that matched a prompt's `event_id` across restarts sees a new value for each process.

### Fixed

- **The ghost prompts v0.8.6 described no longer collide with the replacement's**: a late prompt of the previous process, or one it raised while a Restart stopped it, carried the ID of the replacement's first prompt, which was then refused as a duplicate before it was shown or journaled. With IDs of their own, the replacement's prompts are no longer mistaken for them, and answering a late prompt of the previous process is refused instead of reaching the new process.
  - The desktop and the TUI no longer forget a session's answered prompts at each start, which only existed because IDs repeated; a late copy of an answered prompt of the previous process is now refused.
  - Desktop: a prompt the new process raises while its Start is still in progress is kept, and its automatic decision taken once the start completes; it used to be dropped as the prompt of a stopped agent. In exchange, a late prompt of the stopped process that reaches the desktop during a Start is shown until the start completes, and cannot be answered.
  - What remains: a prompt the previous process raised while a Restart was stopping it can still be offered on the replacement; its decision, automatic or not, is now refused by the new process instead of being delivered to it. The TUI still drops a prompt the replacement raises between the old process's exit and the end of the restart. And within one process, two different questions that a pattern reduces to the same signature are still one question, as `docs/adapters.md` describes.
- **`relayer_events_pending` counted nothing, and the decision duration measured the policy engine**: the registry closed a prompt at its policy evaluation, which the desktop and the TUI journal right after every detection, so the gauge stayed at zero while an operator had a question in front of them, and `relayer_decision_duration_seconds` measured microseconds of policy evaluation instead of the time to a decision. A prompt now stays pending until it is decided, by the operator or by the policy, or withdrawn, and the histogram measures detection to decision, labelled by `decision_by`. The prompts of a process that ends or is replaced leave the gauge, sensitive ones included, whose ID the journal withholds. `docs/observability.md` gives the histogram's real labels. The web gateway journals no detection, so its runs report no pending prompts, as before.
  - The TUI no longer keeps the detection time of a prompt whose process ended, nor of an exit, which is never decided.

## [0.8.6] - 2026-09-22

Patch release that makes the v0.8.4 and v0.8.5 fixes true. A verification of those two releases found each of their eight fixes partial, two of them critical: a web page could take over a tokenless gateway through DNS rebinding, and on Windows stopping an agent could kill an unrelated process. Both are closed. The settings editor no longer weakens the policy it saves, every PTY stop path gives agents their grace period, an agent that exits on its own can be restarted any number of times, and viewer tokens no longer receive command lines or file paths. The entries below say only what the code does, and the overstatements in earlier entries are corrected at the end of this section.

### Security

- **A DNS-rebinding page still got the operator role on a tokenless gateway**: v0.8.4 added an Origin check, but it compared `Origin` with `Host`, and a page that rebinds its own domain to `127.0.0.1` controls both — its browser sends `Origin: http://rebind.example:8080` and `Host: rebind.example:8080`, they match, and the socket is loopback. Such a page could still read `getAgentProfiles`, write an agent command line of its choice and restart the agent: command execution from a web page, on the default `relayer serve` invocation.
  - A tokenless gateway now answers only to requests addressed to `127.0.0.1:PORT`, `localhost:PORT` or `[::1]:PORT`, on every path including the static interface and `/api/health`, and refuses any other `Host` with `403`. That header is the one a rebinding page cannot choose.
  - The Origin check no longer trusts any loopback origin on any port. A page on another local port — a dev server, the telemetry stack's anonymous Grafana on `:3000` — is another origin, and v0.8.4's tests asserted the opposite. `Origin` must now name exactly the host and port the request was sent to.
  - Behind a reverse proxy the proxy must preserve `Host`, port included: `proxy_set_header Host $http_host;` for nginx, since `$host` drops the port. One that rewrites it to the upstream address makes every browser request look cross-origin. A tokenless gateway also refuses a request whose `Host` names another port or name, such as one through an SSH tunnel to another local port. A forward that keeps `localhost` and the same port looks exactly like a local browser and is accepted anonymously, and so is every request without an `Origin` through a proxy that rewrites `Host` to the gateway's own address, which is nginx's default; any forward, tunnel or proxy needs `--token`. `docs/web-gateway.md` now says so, and no longer claims there is no origin check at all.
  - `TestAnonymousGatewayRefusesDNSRebinding` drives the rebinding request shape against a real gateway on every kind of path and over the WebSocket; it fails against v0.8.5.
- **The Recordings panel described transcripts as safer than they are**: it said recordings held "terminal output only" with intercepted credentials "redacted before a frame is written". A transcript holds the terminal output verbatim, including anything an agent printed, and redaction only masks recorded input. The panel now says so. Its "Redacted" badge, shown whenever redaction was configured even when no input was recorded, read as if the output were redacted; the input badge now says "Input masked" or "Input recorded verbatim", and appears for no tmux transcript, since tmux sessions record no input.
- **"Save and restart" could keep changes it said it had undone**: when the security or notification tab had changed, the panel saved them through a separate call before starting the restart transaction, so the transaction took its rollback snapshot of a file those tabs had already changed. A failed restart then kept them, and the panel said "the previous YAML was restored". Every tab now travels in the one transactional request, written after the snapshot, and on the desktop a failed restart restores the file byte for byte. The web gateway, which has no rollback, now writes every tab in the same atomic write as the agents; a failed restart there leaves that file in place.
  - After a failure, and after a rollback, the panel reloads every tab from the file, not only the agents, and says it did only if the reload succeeded. After a rollback the security and notification tabs kept the changes the notice said were undone, and after an error the panel said it had reloaded even when it had not.
- **Every settings save erased every webhook's headers**: the editor never receives header values — they are usually an `Authorization` credential — so each webhook it sent back had none, and the save replaced the whole notifications block with that. On the desktop the notifier was rebuilt at once, so the next alert went out unauthenticated. Headers are now carried through a save by the webhook's URL and name. Webhooks that share both, unnamed ones included, are paired in order while their number is unchanged by the save; otherwise none of them keeps its headers. A webhook is matched by its URL alone, as a rename, only when that URL has a single webhook before and after the save. Anything else carries nothing: a webhook moved to a new URL does not take the old endpoint's credential, a webhook with another name at the same URL never takes one, and a webhook renamed beside another at the same URL, or one of several identical webhooks when one is added or removed, loses its headers, which then have to be set again in the YAML. A credential never moves to another URL. Pairing identical webhooks in order can still hand one's headers to its twin when the editor removes the first and adds a new one in the same save. The editor shows that a webhook has headers, never their values.
  - A webhook that could not be delivered left no trace. `relayer serve` now writes one line per failure to its diagnostics, with the webhook's name and the HTTP status and never its URL — for Slack and Discord the URL is the credential — nor the transport error, which quotes it. The desktop writes the same line to its standard error, which is visible only when the application is started from a terminal: the released Windows build has no console. The TUI still reports none: it owns the terminal.
  - The notifier was replaced on save while alerts read it without a lock, on the desktop and the gateway; the desktop also read agent names without the lock. Both are guarded now.
  - The test button reported success with notifications switched off on the gateway, and on the desktop, where no test exists, said "test simulated" as if it had passed. The gateway now refuses a test while notifications are off, and the desktop says the test is unavailable.
- **A viewer token still received every agent's command line, credentials included** — v0.8.4 said viewers were given agent configurations "without exposing secrets". `getAgentProfiles` returned each agent's full argument vector and the configuration's path; a token planted in an agent's `--api-key` came back verbatim to a viewer. A viewer now gets each agent's executable name and argument count, and no configuration path. Operators, who can already rewrite and restart agents, are unchanged; the desktop never sends argument vectors to its interface at all.
  - A viewer also no longer receives a host path: not the configuration's, the audit journal's, the recordings' or an agent's working directory. `getState`'s startup notices named the configuration and the journal, the audit summary and the journal check returned the journal's path, `getAgentProfiles` returned each agent's working directory, and a recording error quoted the recording directory and the transcript's file. An error that would quote a path reaches a viewer as a fixed message. `getState`'s agent list was already safe: its display command is only the executable's name, and viewers keep it.
  - The settings panel showed a viewer security and notification values that were not the engine's: it filled those tabs from the form's initial drafts. For a viewer, or an engine that does not report them, those tabs now say the settings are not available.
  - `TestViewerNeverReceivesAnAgentsArguments` plants a credential in an agent's argv and the audit journal in the test's directory, and checks every viewer method that returned either; it fails against v0.8.5. The recordings test that should have caught a viewer deleting a transcript rendered before any recording existed, and passed with the guard removed; it now checks the rule and the rendered rows.
- **Windows: stopping a running agent could kill an unrelated process** — a regression in v0.8.5, and the opposite of what v0.8.4 announced. When the grace period ran out, the stop path closed the pseudo console, which ends the agent, and then ran `taskkill /F /T` on its PID. The leader could be reaped in between, and Windows gives a freed PID to the next process that asks: a probe saw seven unrelated processes killed out of eight that had received the number. The graceful request, `taskkill` without `/F`, never reaches a console program under ConPTY, so that forced path was the normal path of every Stop and Restart.
  - Relayer now holds a handle to each agent's process until the session is released, by `Remove` or at the end of `Close`. Windows never reuses a PID while a handle to the old process is open, so the number cannot name another process while Relayer might still use it. v0.8.5 closed that handle as soon as the wait returned.
  - A session latches once its process group is settled — the leader reaped and its descendants terminated — and nothing probes or kills by the leader's number after that, on any platform. The forced path no longer kills a leader that has already been reaped, and a stop request that arrives after the reap sends nothing.
  - The exit state is kept by the session under its own lock. On Windows `cmd.ProcessState` was written by the wait while the stop path read it from another goroutine, a data race the platform guards of v0.8.4 depended on.
  - Whether a process still runs is now read from its handle, which Windows signals when the process ends, rather than from its exit code. With the handle held, an agent that exited with 259 — the value of `STILL_ACTIVE` — would otherwise read as running forever. `TestAnAgentThatExitsWith259IsConfirmedStopped` covers it.
  - `TestExitedSessionKeepsItsPIDReserved` proves the PID still names the exited agent until `Remove`; against v0.8.5 the same `OpenProcess` fails because the number is already free.
- **Windows: every Stop waited out a grace period meant for Unix**: v0.8.5 applied the 1.5-second SIGTERM grace on Windows too, where the polite request reaches nothing, so each Stop and Restart took about 1.7 s instead of 0.2 s. Closing the pseudo console — which sends `CTRL_CLOSE_EVENT` to every attached process — is the graceful stop there, and it now happens immediately. `Close` stops the sessions in parallel, so closing several agents takes about as long as closing one. An agent that handles `CTRL_CLOSE_EVENT` has up to 5 seconds, Windows' own limit for a close handler, before it is killed; `TestAnAgentGetsTimeToHandleItsConsoleClosing` gives one a 3-second handler. Every budget for a Stop, a Restart or a shutdown is now built on a stop's worst case, `session.StopBudget` (about 7.4 seconds on Windows and 3.9 on Unix): the web gateway gave five seconds, so an agent the console close never reaches, such as one that left its console, ended as `stop_uncertain`, and `relayer serve` could exit with it still running. `session.StopBudget` bounds a PTY stop; a tmux stop is bounded by the tmux backend's own command timeouts. Actions on different agents no longer wait for one another: a Stop queued behind another agent's slow stop spent its budget waiting and locked its agent as `stop_uncertain`. A whole-run stop, restart or shutdown still waits for the actions in flight, and its budget now counts from when it can begin, so a shutdown can take that budget plus the longest action it waited for — about 10 seconds measured, where before it gave up early and agents outlived the gateway. `relayer serve` reports a shutdown that did not close every agent instead of discarding the error. The desktop's own whole-run stop and close keep their fixed 12 seconds, which covers the budget on both platforms.
  - The pseudo console's pipes are no longer closed under a read or a write still using them. conpty closes its handles at once, and a write racing a stop, or the reader blocked on the output, then used a number Windows could already have given to another pipe or file. The device now reads and writes through its own copies of the handles, closed only when nothing uses them; `TestStoppingAnAgentReportsNoStreamError` checks that a stop reports no stream failure.
- **Unix: shutting down still killed every PTY agent with SIGKILL first**: v0.8.5 gave a single `Stop` a SIGTERM grace period, but an agent's command is an `exec.CommandContext` whose default cancellation is an immediate SIGKILL of the leader, and every path that cancels a context before stopping the sessions reached it first: the desktop and TUI shutting down, a signal to `relayer serve`, a web "Save and restart". This covers the PTY backend. A tmux agent is still stopped with `tmux kill-session`, which sends SIGHUP to its pane with no grace period.
  - Cancelling an agent's context now makes the same request as `Stop`: SIGTERM to its process group, and SIGKILL from os/exec only if the leader is still running after the grace period.
  - The cancellation and a later `Stop` or `Close` are one request, so an agent receives a single SIGTERM however many paths ask. Cancelling and then closing sent two, and an agent whose handler runs once, or that takes a second TERM to mean "now", was killed by the second.
  - `Close` stops every session in parallel.
  - An agent's children get their 250 ms grace after the agent exits even during a shutdown; a shutdown used to skip it and kill them at once.
  - The PTY reader no longer stops because a context was cancelled. It used to close the PTY at its next read, which sends SIGHUP, so an agent that printed anything while handling SIGTERM was killed partway through its own shutdown. Every stop path closes the PTY itself, so the reader still ends, and it now keeps the agent's last output.
  - An agent that exits cleanly after a cancellation keeps its real exit status; os/exec reported the context's error instead, and the exit was recorded as a failure.
  - `TestEveryStopPathGivesTheAgentItsGracePeriod` runs a leader that traps SIGTERM to write a marker, through Stop, Close, and a cancelled parent context. `TestShutdownSendsASingleSIGTERM` counts the signals a cancel followed by a close delivers.
- **Saving the security tab weakened the policy on disk** — a regression in v0.8.5, whose entry said settings were saved "without loss". The save guessed a profile from the file and rebuilt the policy from that preset. A save that only toggled dry-run then dropped every custom deny rule and every guardrail `blocked_patterns` entry, and the built-in default — which the guess called "strict" — had its limits of 0 and 0 rewritten to 10 and 1. Limits could never be set back to 0, and a workspace root could be written as the relative path `.`, which no absolute path is inside, so the workspace guardrail refused everything.
  - Both front ends now save through one helper, `policy.ApplySettings`. It starts from the policy on disk, carries rules and blocked patterns through, and replaces rules only when the user explicitly switches to a different preset; blocked patterns are kept even then, since they only ever block more. Limits are taken as given, zero included. A workspace root is filled in only when the outside-workspace guardrail needs one, and is always made absolute against the configuration's directory; a relative `workspace_root` in the file now resolves against that directory too.
  - A profile is no longer guessed: the editor names a preset only when the policy is exactly that preset, and shows `custom` otherwise.
  - The form's presets now come from the engine. The form kept its own copy, which disagreed with what the engine loads for the same name: strict was 20/5 in the form and 10/1 in the engine, and "permissive" turned every guardrail off and enabled dry-run in the form while the engine keeps the guardrails on and approves live. The label now says what permissive does.
  - **Behaviour change**: choosing "permissive" now gives the engine's permissive profile, automatic approval with the guardrails on, where v0.8.5's form gave dry-run. Choosing any preset replaces the default action, the guardrails, the limits and the policy rules with the preset's when saved, custom deny rules included; dry-run, the workspace root and blocked patterns are kept. The form now says so under the preset selector.
  - A web save now refuses a form loaded before the file last changed. It compared only a token that changes when the gateway itself writes, and accepted a save that sent none, so a deny rule added to the YAML by hand while the form was open was dropped by a save that only toggled dry-run. `TestWebSaveRefusesAFormOlderThanTheFile` covers both.
  - The workspace-root field said an empty value meant the current directory. It keeps the root already in the file, or uses the configuration file's folder when the guardrail needs one, and the placeholder now says so.
  - "Save and restart" no longer sends an untouched security tab along with a notification change.
  - `TestWebSecuritySaveKeepsWhatTheFormDoesNotShow` and its desktop counterpart drive the real save path from a file on disk; both fail against v0.8.5.
- **The desktop announced policy changes as "applied immediately" while nothing enforced them**: the policy engine is built once per run and has no way to swap its configuration, but the desktop's save treated a run as current whenever the agents were unchanged. After switching on dry-run, the top bar showed DRY RUN and the panel said the change was applied, while automatic approvals kept being delivered by the old engine. v0.8.5 fixed this for the web gateway only, and its test for the desktop asserted the wrong behaviour.
  - Both front ends now report a restart as required whenever a save changes the agents or the policy. While a run is active, a save that only changes notifications, which both apply at once, needs none; the web gateway used to ask for one. With no run active, the desktop still says the configuration has to be started.
  - The desktop top bar keeps showing the policy the running engine enforces, not the one just saved, and a test now checks it.
  - `README.md`, `docs/architecture.md`, `docs/configuration.md` and `docs/gui.md` promised a hot reload of policies and guardrails that does not exist; they now say what applies at once and what waits for a restart. Real hot reload remains possible later; until then the product no longer claims it.
- **An agent that exited on its own could only be restarted once** — v0.8.5 announced the fix, but it held for a single cycle and only on the desktop.
  - Desktop: a fresh process numbers its events from 1 again and event IDs derived from that sequence, so the second process's exit carried the same ID as the first one's. The desktop kept that ID as already handled and dropped the second exit; the agent then looked running forever and Start was refused as "still running". A start now forgets the previous process's handled events, and each process's exit carries an ID of its own, so a late exit from the previous process can no longer mark the replacement's as handled.
  - Web gateway: an exit was broadcast as `stopped`, a status the interface does not clear "running" on, so the card kept a Stop button that could only fail; a start never set the agent back to running, so the card offered Start for a live agent. Exits are now `exited` or `failed`, a lost tmux session is `failed` as on the desktop, and a start marks the agent running and tells every client. An agent that ended loses its pending prompts, as on the desktop, and neither answering one nor its late withdrawal marks a dead agent running again. The `running` status sent after a delivered answer now goes out only while the agent still runs; an exit handled in the microseconds between that check and the broadcast can still show the dead agent running until the next status.
  - TUI: exits never reached the lifecycle, so restarting a dead agent first "stopped" it and journaled a second `session_finished`. The TUI now reports exits, off its update loop, and a restart no longer stops a process the backend reports gone. A restart also forgets the agent's answered prompts: a fresh process numbers its prompts from 1 again, so the restarted agent's first prompt carried an ID the TUI had recorded as answered, was dropped, and could not be answered at all.
  - An exit names only the agent, and the previous process's exit can be emitted after a replacement started. It marked the replacement stopped in the lifecycle, which let a second Start run beside it. The lifecycle now applies an exit only when the backend reports the current process gone, and an exit that arrives while a Start is under way waits, up to 5 seconds, for the Start to finish before the backend is asked: the previous process's exit is emitted exactly when its session settles, which is what a Restart waits for. An exit is stale only if the replacement is running: one that sees a Start begin waits for it too, and a Start that fails leaves the exit current. The desktop then journals a stale exit as the previous process's and keeps the agent running on screen; the web gateway ignores it. On both, the previous process's pending prompts are dropped when its replacement's start completes, and on the desktop already when the Start or Restart begins, together with the IDs it had answered; they used to stay offered on the replacement, and on the desktop they blocked its automatic decisions and hid its first prompt, which carries the same ID. Two gaps remain, because a prompt's identifier is still its signature and its number within the process, where an exit now also carries a token of its own process. A prompt of the previous process that the event pump delivers after the start completes lands on the replacement, and so does one the previous process raises while a Restart is stopping it, when its exit is then judged stale; if such a prompt is answered or decided automatically before the replacement asks the same question, the replacement's own prompt carries the same identifier and is refused before it is shown or journaled, and stays hidden until the agent is started again. On the desktop, a prompt the replacement raises while its Start is still in progress, before the agent is marked running, is dropped. Whether a Start began while an exit was judged is told by a start counter, not by the agent's state: an agent that exited on its own is still "running" then, and a Start over it ends "running" again. The interface drops only the prompts the backend says it dropped, so a stream error on a live agent no longer hides its prompts. The TUI still applies every exit: a late exit of the previous process marks the replacement's pane exited, and the TUI then ignores that agent's prompts until it is restarted again.
  - A backend reports a process gone as soon as its leader is reaped, but releases the session only once the leader's group is cleaned up. A Restart in between waits up to 3 seconds for that release instead of reporting the stop as uncertain.
  - Telemetry: an exit is journaled as a detected event but is never decided, so each one stayed in `relayer_events_pending` for good. Exits no longer count as pending.
  - A tmux pane whose exit status tmux never fills in counts as exited once its exit is published; the status query kept failing, which would have kept such an agent "running" forever.
  - `TestRestartAfterASecondNaturalExit` and `TestWebAgentCanBeStoppedAndStartedAgainAndAgain` run several stop and start cycles through the real event handlers, with a synthesized exit on the desktop and an operator stop on the web; both fail against v0.8.5.
- **Starting a running tmux agent killed it and started another**: to restart an agent that had exited on its own, v0.8.5 made Start call the backend's `Remove` whenever the agent was believed running, and took success as proof the process was gone. The tmux backend's `Remove` kills a live session and reports success, so Start on a running tmux agent — tmux is the default backend on Unix, and the web interface offered Start on running agents — killed it and launched a second one with no stop on record. Start now asks the backend, read-only, whether the process still runs, refuses if it does, and asks outside the lifecycle lock so a slow backend cannot hold up an exit notification.
- **Unix: the process-group guards v0.8.4 announced were not in the v0.8.4 release**: they were removed by the commit the `v0.8.4` tag was moved to, because skipping descendant cleanup after the leader exits broke termination of signal-ignoring descendants. The settled-group latch above is the replacement: descendant cleanup still runs, right after the reap and while the kernel still reserves the group's number for any member left, and the group is never addressed after it.

### Documentation

- **The configuration examples did not load**: they used keys the strict loader rejects — `os_notifications`, `terminal_bell` and `type` for notifications, `workspace_only` for the workspace guardrail, `interval` for OTLP — so a copied example was a configuration Relayer refused. This held for the reference and for README's example of "every top-level section". Both now use `desktop`, `bell`, `format`, `block_outside_workspace` with `workspace_root`, and `export_interval`, and README's example gains the `recording` section it lacked. `TestConfigurationReferenceExamplesLoad` and `TestEveryDocumentedConfigurationLoads` load every top-level YAML example in `README.md` and `docs/` with the strict loader, so the documents cannot drift from the loader again; they fail against the v0.8.5 documents.
- The reference said omitted `notifications` default to disabled; they default to enabled, with the bell and desktop notifications on. It called the audit journal tamper-evident; it is unsigned and is not.
- `docs/web-gateway.md` gives the nginx `Host` line that keeps the port, and says which tunnels, forwards and proxies a tokenless gateway refuses and which it accepts anonymously, that `--bind localhost` is anonymous like `127.0.0.1`, who receives agent command lines, and that a container needs an init process (`docker run --init`), or every Stop of an agent that leaves child processes behind is reported unconfirmed. `docs/architecture.md` no longer calls the audit journal cryptographic. `docs/architecture.md` no longer says Relayer never signals a process by a reusable number on Unix, where the cleanup right after the reap still addresses the group by number, and it says the graceful stop covers PTY agents and not tmux.

### Corrections to earlier entries

Some older entries below describe more than those releases did. They are left as written, with each overstatement corrected here:

- **v0.3.0, hot reload**: security policies and guardrails were never updated without a restart; only notifications, webhooks included, apply at once. The "visual configuration hot-reload" and "zero-downtime hot-reload" of that entry describe the editor saving the file, not the running engine picking it up.
- **v0.8.4, origin check**: the check accepted any loopback origin on any port and compared `Origin` with `Host`, which a DNS-rebinding page controls; a tokenless gateway stayed exposed to web pages. Fixed above.
- **v0.8.4, PID reuse**: the `ProcessState` guards protect Windows only, and read a field another goroutine was writing. The session-level guards the entry lists (`requestStop`, `waitForStopWithin`, `confirmProcessGroupStopped`, `waitSession`) are not in the v0.8.4 release: the `v0.8.4` tag was moved from `01177f9`, whose release build failed and published nothing, to `8fed073`, which removed them. The published v0.8.4 artefacts are built from `8fed073`. v0.8.5 then reintroduced a PID kill on Windows. Fixed above.
- **v0.8.4, viewer allowlist**: viewers were not given agent configurations "without exposing secrets": `getAgentProfiles` returned every argument vector. Replaying a recording remains available to viewers, as `docs/recording.md` says; only export and deletion are operator actions. Fixed above.
- **v0.8.5, settings**: security settings were not saved "without loss" — a save dropped deny rules and blocked patterns and rewrote limits — and the "restart required" fix applied to the web gateway only. The graceful Unix stop covered a single agent's Stop and not shutdown; on Windows it made every Stop 1.5 seconds slower. Webhook headers were erased by every save. All fixed above.
- **v0.8.5, agent lifecycle**: restarting an agent that exited on its own worked once, on the desktop. The `Remove` that Start used to prove an agent gone killed a live tmux agent and started a second one. Both fixed above.

## [0.8.5] - 2026-09-21

Patch release entitled "réglages honnêtes" ("honest settings"), resolving settings fidelity, configuration lifecycle transitions, notification enforcement, and process termination handling. Several of its fixes were partial; see "Corrections to earlier entries" under 0.8.6.

### Fixed

- **Security Settings Saved and Reloaded Without Loss**:
  - The Web Gateway previously returned hardcoded fictitious values (`Profile: "default"`, `BlockDestructive: true`, etc.) in `extractSecuritySettings`, ignoring `cfg.Policies.Guardrails`. When saving via `SaveFullSettings`, security guardrails (`BlockDestructive`, `BlockExfiltration`, `BlockSensitivePaths`, `BlockOutsideWorkspace`, `WorkspaceRoot`), profiles, and rate limits were discarded.
  - Aligned `internal/server/controller.go` with `settings_panel.go`: `extractSecuritySettings` faithfully extracts active guardrails and detects profiles (`strict`, `developer-friendly`, `custom`), and `SaveFullSettings` applies `buildPolicyConfig` to persist all security fields to disk.

- **Truthful "Restart Required" Status and Functional "Save and Restart"**:
  - `loadAgentProfilesLocked` previously called `getOrGenerateToken` which updated `c.revisionHash` before evaluating `RestartRequired`, causing it to always evaluate to `false` even after settings changed on disk.
  - `Controller` now tracks `activeConfigRevision`, displaying `RestartRequired: true` whenever configuration on disk differs from the active running engine's revision.
  - In `AgentSettingsPanel.tsx`, `saveAndRestart` now captures the fresh revision returned by `saveFullSettings` (`expectedRevision = fullResult.revision`) before calling `onSaveAndRestart`, resolving the stale revision rejection error (`errStaleRevision`).

- **Enforced Notification Disabling**:
  - When notifications were disabled in configuration (`notifications.enabled: false`), the supervisor runtime previously kept its default active notifier instance, continuing to process alerts. Furthermore, `relayer:notification` WebSocket events were broadcast unconditionally.
  - `controller.go` and `cmd/relayer-gui/app.go` now unconditionally initialize notifiers via `notify.New(metadata.Notifications, ...)`, which returns a `noopNotifier` when disabled.
  - WebSocket `eventNotification` broadcasts are gated by `notificationConfig.Enabled` and minimum severity thresholds, silencing browser toasts and chimes when notifications are disabled.

- **Clean Agent Process Group Termination under Unix & Restart of Self-Exited Agents**:
  - `session.requestStop` previously canceled the command context and closed the master PTY immediately, forcing an instant SIGKILL via Go's `os/exec` context handler before the process could handle SIGTERM. It now issues `TerminateProcessGroup` (SIGTERM) first, allowing child processes a 1.5s graceful timeout to exit cleanly before fallback kill and PTY closure.
  - `internal/platform/process_unix.go` guards against `command.Process.Pid <= 0` to prevent unintended signaling of the supervisor's own process group (PID 0).
  - `agentLifecycle` and `DesktopRuntime` now expose `MarkProcessExited`, transitioning self-exited processes from `agentStateRunning` to `agentStateStopped`. Additionally, `StartAgent` validates route availability with `router.Remove`, allowing agents that exited naturally to be restarted cleanly without `errAgentRunning` errors.

## [0.8.4] - 2026-09-21

Patch release addressing three critical security and process-lifecycle vulnerabilities across the Relayer Web Gateway, the session supervisor, and role-based access control. Its origin check and PID-reuse guard were incomplete and its tag was moved after a failed release build; see "Corrections to earlier entries" under 0.8.6.

### Security

- **Cross-Site WebSocket Hijacking and Rogue Cross-Origin Gateway Access**:
  - The Web Gateway previously accepted WebSocket connections from any `Origin` unconditionally (`upgrader.CheckOrigin` returned `true`), and granted unauthenticated operator privileges to local requests without validating request origin. A malicious webpage visited in an operator's browser could silently open a WebSocket or send requests to `localhost:8080` to seize control of running agent sessions (CSWSH).
  - All incoming browser connections to `/api/ws` and `/api/state` now strictly validate the `Origin` header (`checkSameOriginOrLocal`), requiring it to match the host or originate from a loopback address. Cross-origin requests from external domains are rejected immediately with HTTP 403 Forbidden.
  - Anonymous local operator access (`allowAnonymousLocal`) is restricted to verified loopback remotes with matching or loopback origins.
  - Non-browser clients (CLI, curl, native GUI) without an `Origin` header continue to operate normally.

- **Windows PID Reuse Hazard during Process Group Termination**:
  - Stopping or reaping an agent process previously issued `taskkill` and queried OS process handles using the command's PID even after Go's `cmd.Wait()` had reaped the process. Under Windows, PIDs are recycled quickly, meaning Relayer could inadvertently kill or signal an unrelated operating system or user process that was assigned the recycled PID.
  - `platform.TerminateProcessGroup`, `platform.KillProcessGroup`, and `platform.ProcessGroupExists` now verify `command.ProcessState == nil` before targeting any PID. If the process has already terminated and been reaped, process manipulation functions return immediately as safe no-ops.
  - `session.requestStop`, `session.waitForStopWithin`, `session.confirmProcessGroupStopped`, and `manager.waitSession` no longer target process groups after process exit.

- **Explicit Viewer Role Allowlist**:
  - Replaced the mutating-method blacklist with an explicit, strict allowlist (`isViewerAllowed`) defining the exact read-only operations permitted for the Viewer role.
  - Formally blocks session recording export (`exportRecording`), preventing read-only viewers from dumping raw `.cast` terminal transcripts. The frontend Recordings panel disables the download action accordingly.
  - Blocks sensitive configuration retrieval (`getFullSettings`), preventing viewers from viewing notification webhooks and secret tokens. The web settings panel automatically falls back to `getAgentProfiles` in read-only mode to display agent configurations without exposing secrets.
  - Any unlisted or future method is denied by default with `permission denied: viewer role is read-only`.

## [0.8.3] - 2026-09-21

Patch release making the audit journal record what the documentation said it recorded. Hand-overs of a terminal between operators, forced takeover attempts, and each session recording's opening and closing are now journaled; until now none of them were. Recording and hand-over events also reach telemetry, and the Audit panel can filter on every kind the journal holds.

### Fixed

- **Seven of the nine recording and control audit kinds were never written**: v0.7.0 declared `control_requested`, `control_granted`, `control_declined`, `control_released`, `control_forced`, `recording_started` and `recording_finished`, and `docs/sharing.md` and `docs/recording.md` described each as journaled — "every use is journaled as `control_forced`" — but nothing emitted any of them. A hand-over between operators, and a forced takeover attempt, left no trace in the journal. Only `recording_exported` and `recording_deleted` were real.
  - Every control verb now journals the transition it made, captured under the same lock as the change so the record describes the state the verb acted on. Where the transfer has another party — the holder asked, the operator handed to or refused, the holder displaced — the record names them in `target_operator` and `target_conn_id`, keys the audit allowlist had already reserved.
  - One action writes one record. Releasing through the attach control writes `attach_finished` and not `control_released` too; asking again for a terminal you are already waiting on is not journaled twice; a request that expires or is withdrawn moves no terminal and is not journaled.
  - A holder whose connection drops is journaled as `control_released` by the system, so a journal never shows somebody taking a terminal and then simply stops.
  - Every force-takeover attempt is journaled, refused ones with outcome `failed`. Force takeover cannot currently be enabled — the documentation said a deployment could opt in, but no setting does that, and the documentation now says so — so today every such record is a refusal, which is exactly the event worth finding.
  - The session recorder now reports each transcript opening and closing, and the run journals them as `recording_started` and `recording_finished` with the transcript's identity and final size. A transcript the store could not open is journaled as a failure, so a session that went unrecorded says so in the journal as well as in the startup diagnostics.
  - `TestEveryHandoverIsJournaled` and `TestRecordingLifecycleIsJournaled` drive a real run and read the journal back, then verify it; both fail against the previous code.
- **The control verbs accepted a session that does not exist**: taking or asking for a terminal on an unknown session ID created a write-lock entry for it, so a client could grow that table without bound. They now refuse it, as the attach verb already did.
- **A recording named its agent by display name**: transcripts stored `info.Name` as their agent ID where every other audit record uses the agent's identifier, so filtering the journal by agent missed the recording export and delete records. New transcripts store the identifier; the display name keeps its own field, which is what the Recordings panel shows.
- **Recording and control events were invisible to telemetry**: the metric registry fell through all eleven kinds. `relayer_control_events_total` and `relayer_recording_events_total` now count them by `action` and `outcome`, in both the Prometheus and OTLP exporters.
- **The Audit panel's kind filter offered nine of the twenty-four kinds**: it was a hand-written list that never gained the attach, recording, control, cleanup or error kinds. It is now derived from the journal's own kind counts, like the agent filter beside it.

## [0.8.2] - 2026-09-20

Patch release closing every free-form field on the audit kinds that sit closest to raw terminal bytes. The recording, control and attach records now drop `Summary`, `EventID` and `Rule` by a rule in the sanitizer rather than by each emitter remembering to leave them empty.

### Fixed

- **The recording, control and attach audit kinds could carry free-form text**: `SanitizeEntry` cleared `Summary` for a human decision but left it open for anything else, so the nine kinds added for session recording and multi-operator control would have carried an arbitrary caller-supplied summary the moment one was emitted by the system rather than by a person. The comment beside those constants already promised that no captured stream and no typed key is a field of `Entry`; `Summary` is such a field, and nothing enforced it.
  - Closed by a `closedFreeFormKind` predicate rather than by asking every future emitter to remember. `recording_started` and `recording_finished` are naturally system-emitted, so the first caller to wire them would otherwise have opened the field by default.
  - `EventID` and `Rule` were the same hole and worse: they reach the journal through `sanitizeText` alone, which redacts credential shapes but keeps ordinary prose, `Rule` is not length-bounded, and unlike `Summary` both survive `ModeMetadata`. They are now cleared for those kinds before the mode is even consulted.
  - `safeCode` bounded a reason's *shape* but never its *content*: an API token is a single lowercase word of allowed characters, so `sk-…` and `ghp_…` were journaled verbatim where the same string in a metadata value would have been redacted. A reason is a closed vocabulary, so anything a credential pattern recognises is no longer accepted as one. Verified against all 41 reason codes the product emits; none changed.
  - `VerifyJournal` mirrors the rule. It already shared the metadata allowlist with the sanitizer so the two could not disagree; it now shares this rule too, and reports a line carrying free-form text on a closed kind — a shape the sanitizer cannot produce.
  - The attach kinds are closed by the same rule. Their two emitters, in `internal/server` and `internal/tui`, populate none of the three fields and both record a human decision, so nothing changes for them and no journal already written can contain what this refuses — which is what makes widening the verifier safe as well. Resting on that coincidence was the thing worth fixing.
  - Latent, not a live leak: the only kinds with emitters today all record a human decision and were already cleared.


## [0.8.1] - 2026-09-20

Patch release fixing spurious file-replacement failures on Windows. A configuration save, and an audit journal rotation, could report a failure for an operation that a transient hold on the file would have allowed a moment later.

### Fixed

- **Publishing a configuration failed spuriously on Windows**: `ReplaceAgents` and the full-configuration save retried the atomic rename five times ten milliseconds apart, a fifty-millisecond budget that is shorter than a transient hold on the file lasts. The caller then reported `could not atomically publish configuration` for a save that would have succeeded a moment later, which in the Desktop GUI and the web settings panel is a save that appears to have failed. `publishConfigurationBytes`, the rollback path, had no retry at all.
  - Every file replacement in the repository now shares one helper in `internal/platform`, with a two-second budget and exponential backoff, matching the configuration lock's own timeout. Removing a file goes through it too: Windows refuses a remove on a held path exactly as it refuses a rename.
  - Retrying is confined to the platform conditions that clear on their own, confirmed against this platform rather than assumed: an exclusive handle on the destination reports `ERROR_ACCESS_DENIED`, one on the source reports `ERROR_SHARING_VIOLATION`. A missing source or directory is returned immediately, because waiting does not create a file. Away from Windows nothing is retried: `rename(2)` does not fail because another process holds the file open, so every error it reports is a real one.
  - The trigger is broader than antivirus. Go does not pass `FILE_SHARE_DELETE` when it opens a file, so **any** concurrent reader of the configuration is enough for Windows to refuse the publish.
  - The same exposure existed at four further replacement sites, now routed through the shared helper: audit journal rotation, the session-recording sidecar, and two in fixture capture. Rotation was the most consequential — it removes the oldest generation and renames the rest down, and a refusal there turned into a sticky rotate error on a fail-closed sink, so a record that should have been written was lost. A test that holds a rotated generation for 250ms proves the old code failed and the new code waits it out.
  - This was also the cause of the intermittent `internal/config` and `cmd/relayer-gui` test failures on Windows, including the lifecycle tests that reported `restart did not reach strict old-run stop` — a symptom downstream of the failed save. Both suites now run clean where they previously failed roughly one run in three.


## [0.8.0] - 2026-09-20

Minor release adding structured MCP tool-call badges beside the arbitration prompt, git interactions for the Aider adapter, and a correction to an adapter inventory that had been two releases out of date.

### Added

- **MCP Tool-Call Badges**:
  - Added `internal/adapters/mcp.go`, which reads a detected prompt block and reports whether it describes a Model Context Protocol tool call: the server, the tool, a bounded argument list, and a risk level.
  - Recognition anchors on the `mcp__<server>__<tool>` naming convention rather than on the frame an agent draws around a call. The convention is agent-independent; the layout differs per CLI and per version, and no captured output exists to match one against.
  - Risk is classified from the tool name alone and never from an argument value, because an argument is agent-controlled: a delete or shell tool carrying a reassuring path would otherwise read as low risk exactly when it should not. A block resolves to the most dangerous name it carries, so a decoy name printed inside an argument can only raise risk, never lower it.
  - The web interface renders the result as a badge beside the arbitration prompt, so an operator sees what a tool is about to be given before answering. The badge is suppressed on a confidential occurrence, where reprinting the surrounding text would undo the masking next to it.
  - The audit journal records the tool's identity only — `mcp_server`, `mcp_tool` and an argument count — so it can answer which tool somebody approved. Argument values never reach it, enforced independently by the metadata allowlist in `internal/audit`.
  - Detection and display only. Relayer does not intercept, sandbox or block a tool call, no policy decision changes, and an agent may call a tool without printing anything readable — an absent badge is not evidence that no tool ran. Heuristic, and not fixture-backed. See [docs/adapters.md](docs/adapters.md#mcp-tool-calls).

- **Aider git interactions**: the Aider adapter now recognises commit, push, git-add and gitignore confirmations alongside the four interactions it already covered. A commit is `RiskLow` and a push `RiskHigh`, because a push leaves the machine and Relayer cannot undo it. The push entry is ordered ahead of the broad `create_file` marker, which previously swallowed `Create branch and Push to remote?` and presented a high-risk push as a low-risk confirmation. Like the rest of the Aider adapter, these markers are hand-written and unverified against a real Aider build.

### Fixed

- **The README's adapter inventory was two releases out of date**: it named only Claude Code and Codex as adapters, and described both as fixture-backed. `internal/adapters` also contains Aider, Goose and Open Interpreter, all registered in the tool catalog, and only Claude and Codex have captured fixtures. A reader trusting the README concluded three supported agents were unsupported. The registry table in `docs/adapters.md` was missing Goose and Open Interpreter entirely, and claimed Aider's decisions had been "verified" when no fixture backs them.
- **The README understated which adapters can act automatically**: it said only the generic and Claude adapters cannot encode an automatic allow or deny. Aider, Goose and Open Interpreter all encode `y`/`n`, so a low-risk occurrence from them is eligible for automatic approval on patterns that no captured output backs. That is now stated where the limit is described.


## [0.7.1] - 2026-09-20

Patch release re-cut so the released commit passes its own build. It contains **no product change**: the only difference from v0.7.0 is in test files, and the two releases behave identically.

### Fixed

- **The v0.7.0 commit failed the Windows build job**: `TestInteractivePTYWebAndAudit` cancelled the gateway's context without waiting for `Serve` to return. Cancelling only asks the server to stop; it closes the audit journal while `Serve` unwinds, and Windows cannot delete a file another handle still holds, so `t.TempDir`'s cleanup failed. Three sibling tests had already been fixed individually; all six gateway bootstraps now go through one `startServeForTest` helper that owns both the goroutine and the shutdown wait, so no call site can omit it again.

## [0.7.0] - 2026-09-20

Minor release adding session recording to standard asciicast v2 files with in-browser replay, and multi-operator sessions where several people watch one interactive terminal while exactly one holds it. Also fixes an audit journal verification defect that made every journal the web gateway produced fail its own verifier.

### Added

- **Session Recording & Replay (asciicast v2)**:
  - Added `internal/record`, writing each supervised session's terminal stream to a standard asciicast v2 `.cast` file so it can be replayed later for audit or training. Any asciicast player reads the result, `asciinema play` included.
  - Tapped the stream at the three points every byte already funnels through: `adapters.Processor.Consume` for output, `processSession.write` for input, and `processSession.resize` for geometry. The new `Hooks.OnRawChunk` fires after the processor releases its state lock, on the escape-aligned prefix detection otherwise discards, so a transcript keeps the colours and cursor motion the detector throws away.
  - Added the `recording:` configuration block (`enabled`, `path`, `record_input`, `redact`, `max_file_size_mb`, `max_recordings`, `max_total_size_mb`, `retention_days`), off by default, YAML-only and deliberately absent from the visual settings editor.
  - Transcript storage mirrors the audit journal's hardening: 0700 directories, 0600 files, symlink and non-regular rejection, Unix ownership checks, plus a per-transcript JSON sidecar so a crashed run is reported as truncated on the next start instead of silently looking complete. Retention is enforced at startup against count, total size and age, oldest first, and never touches a transcript being written.
  - Added a **Recordings** panel to the web interface with an xterm replay surface: play, pause, scrubbing, and 0.5x–8x speed. Replay is strictly read-only — the player holds no bridge and cannot resize, interrupt, or type into a live session. Long transcripts are paged and a partially loaded one says so rather than ending early and looking like a session that finished there.
  - Added the `listRecordings`, `getRecording`, `readRecordingChunk`, `exportRecording` and `deleteRecording` RPCs plus the `relayer:recording` event. Export and delete are operator-only and audited with the acting operator; a transcript still being written cannot be deleted.
  - Added the `recording_started`, `recording_finished`, `recording_exported` and `recording_deleted` audit kinds, carrying a transcript's identity and shape but never its content.

- **Multi-Operator Sessions & Live Session Sharing**:
  - Several operators and viewers can now watch the same interactive session at once, with exactly one connection holding the terminal at a time.
  - Taking a terminal another operator holds is refused rather than silently stolen: the caller requests it, the holder is prompted, and the terminal moves only on a grant. An unanswered request expires, and a holder who disconnects frees it. Force takeover exists but is off unless a deployment opts in, since an operator mid-command can be interrupted by it.
  - The write lock is keyed by **connection**, never by identity: a second browser tab does not inherit the first tab's terminal, and releasing in one tab does not revoke the other.
  - Added a per-agent presence strip naming who is watching, who holds the terminal and who is asking for it, plus a grant/decline prompt shown only to the operator who can answer.
  - Added the `listPresence`, `observeSession`, `requestControl`, `grantControl`, `declineControl`, `releaseControl` and `forceTakeControl` RPCs, and the `relayer:presence` and `relayer:hand` events. Both events are complete snapshots rather than deltas, because the client send queue drops frames it cannot deliver and a lost delta would leave a roster permanently wrong.
  - `sendTerminalInput` and `resizeSession` now check the write lock. A resize from a non-holder is a silent no-op so two observers with different window sizes cannot thrash the agent's rendering. The one-keystroke race between the check and the write is documented in [docs/sharing.md](docs/sharing.md) rather than hidden.
  - Added the `control_requested`, `control_granted`, `control_declined`, `control_released` and `control_forced` audit kinds with the acting operator, their role, and the connection involved. Keystrokes remain unjournaled.
  - `sendTerminalInput` now validates `runID` against the active run; it was accepted and ignored.

- **Documentation**:
  - Added [docs/web-gateway.md](docs/web-gateway.md) covering `relayer serve`, its flags, the role matrix, named tokens, and the plaintext-transport boundary. `internal/server` had no prose documentation at all.
  - Added [docs/recording.md](docs/recording.md) and [docs/sharing.md](docs/sharing.md).

### Fixed

- **Audit journal verification rejected its own output**: `VerifyJournal` treated any metadata on a human decision entry as a security violation, while `SanitizeEntry` deliberately keeps the allowlisted operator attribution on exactly those entries. Since v0.6.0 began writing `operator` and `role` metadata on human decisions, every journal the web gateway produced failed its own verifier — `relayer audit verify` exited non-zero and the verification panel reported the journal as tampered. Verification now shares `allowedMetadataKey` with the sanitizer, so the two can no longer disagree about what a kind may carry.
- **`conn_id` was dropped from attach records**: the controller wrote it on `attach_started` and `attach_finished` but the metadata allowlist did not permit it there, so sanitization silently discarded the connection identity on exactly the interactive attach entries.
- **Removed the free-form `reason` metadata key from control entries**: `Entry.Reason` is bounded to a short code while a metadata value is only truncated, so the same name under two rules meant the weaker one was what a reader saw.
- **`getUserInfo` failed open to operator**: a failed or timed-out identity lookup left the interface assuming write authority it could not confirm, offering controls the server then silently rejected. It now fails closed to read-only.
- **The audit report exported as JSON was not JSON**: the download rendered Go struct syntax rather than a parseable document. The CSV path now quotes every field through `encoding/csv`, since an operator identity comes from token configuration and can contain a comma.
- **A transcript header carried the agent's whole command line**: the asciicast title used the display command, which for a shell agent is the entire script. Every player shows the title and the file is meant to be shareable for review, so it now carries the agent name, matching the audit model's exclusion of argv from a record.
- **Restored gofmt compliance** in the audit and server tests, which had been failing the build workflow's format gate on `main`.

### Changed

- Unified the interface language: the French strings introduced with the RBAC and interactive-PTY work are now English, matching the rest of the interface.
- Defined `.button--tiny`, referenced by the agent card and the new control prompt but never declared.


## [0.6.0] - 2026-09-20

Minor release hardening the Web Gateway for team use: role-based access control with per-operator attribution in the cryptographic audit trail, and a fully bidirectional interactive terminal (full PTY) streamed over WebSocket binary frames.

### Added

- **Role-Based Access Control (RBAC) for the Web Gateway**:
  - Introduced two distinct privilege levels, `operator` (read-write) and `viewer` (read-only), resolved at authentication time from the presented token and carried on every WebSocket connection.
  - Added the `--viewer-token` flag to `relayer serve` alongside the existing `--token` / `--operator-token` pair, so a supervision session can be shared for observation without granting arbitration or lifecycle authority.
  - Added named-token syntax (`--token alice:s3cret,bob:hunter2`) binding a human identity to each secret, and comma-separated lists so several operators and viewers can hold distinct credentials on a single gateway.
  - When binding outside localhost without explicit tokens, the gateway now auto-generates *two* independent secrets (operator and viewer) and prints both URLs, instead of a single shared credential.
  - Enforced authorization server-side in the RPC dispatcher: every mutating method (`submitDecision`, `submitLine`, `sendTerminalInput`, `setInteractiveSession`, `stopSession`, `startSession`, `restartSession`, `saveAgentProfiles`, `saveFullSettings`, `stopRun`, ...) is rejected for the viewer role, and inbound binary terminal frames from a viewer are dropped before reaching the PTY. `resizeSession` is a silent no-op for viewers so a passive observer's window size never perturbs the owning terminal.
  - Added the `getUserInfo` RPC returning the connected identity, role, and read-only flag, wired through `bridge.ts`, `webBridge.ts`, and `demoBridge.ts`.
  - Added read-only affordances across the React UI: a `VIEWER (READ-ONLY)` badge in the top bar, plus disabled arbitration buttons, decision modal controls, line input, lifecycle actions, and settings forms when the session is read-only. The server remains the authority; the UI only reflects it.

- **Per-Operator Attribution in the Audit Trail**:
  - Added an `Operator` field to `audit.Entry`, propagated through `SubmitDecisionWithOperator`, `SubmitLineWithOperator`, and `SetInteractiveSession`, so every human arbitration, manual line, and interactive attachment records *which* operator acted rather than only that a human did.
  - Extended `audit.SanitizeEntry` to sanitize and bound the operator field (redaction pass, 64-rune cap) under every redaction mode, including the operator-input path where free-form content is otherwise stripped.
  - Allowed the `operator` and `role` metadata keys on `KindDecision`, `KindDelivery`, and `KindOperatorInput` entries; the metadata allow-list remains closed for every other key.
  - Surfaced the operator column in the Audit Trail panel and in `AuditEntryView` (`operator` JSON field).

- **Bidirectional Interactive Web Terminal (Full PTY)**:
  - Added a raw input path from the browser to the pseudo-terminal: `terminal.RawSender` (optional backend capability), `ptybackend.Manager.SendRaw`, `tmuxbackend.Manager.SendRaw`, `session.Manager.SendRaw`, and `backendRouter.SendRaw` with graceful fallback to ordinary `Send` for backends that do not implement it.
  - Added the `sendTerminalInput` and `setInteractiveSession` RPCs plus a compact WebSocket **binary frame** protocol (`[session-id length][session-id][raw bytes]`) that streams keystrokes without per-character JSON overhead.
  - Interactive mode forwards VT escape sequences, arrow keys, and control characters (`Ctrl+C`, `Ctrl+D`, `Ctrl+Z`), making it possible to drive an agent's own TUI from the browser rather than only answering detected prompts.
  - Terminal output now prefers `AnsiOutput` over the plain-text snapshot when the backend can provide it, preserving colors, cursor positioning, and redraws in the web viewport; the plain-text path remains the fallback.
  - Added an explicit per-agent **Attach / Detach** toggle in the web UI backed by a live `Attached` field on `AgentState`, so keystrokes are only routed to an agent an operator has deliberately taken control of.
  - Every attach and detach is written to the audit trail as `attach_started` / `attach_finished` with the acting operator and role. Raw keystrokes themselves are deliberately **not** recorded: the audit log still has no field for terminal input.

### Security

- Viewer tokens are enforced at the transport boundary, not merely hidden in the UI: an observer replaying a mutating RPC or hand-crafting a raw binary frame is rejected server-side.
- Interactive attachment bypasses semantic prompt arbitration by design, since an attached operator types directly into the agent process. Attachment is therefore operator-only, audited at both ends, and should be treated as equivalent to sitting at the agent's terminal. See [docs/security-model.md](docs/security-model.md).

## [0.5.0] - 2026-09-20

Minor release introducing the headless Web Gateway (`relayer serve`), real-time Web Push and acoustic browser notifications, and full settings persistence across webhooks and security policies.

### Added

- **Headless Web Gateway (`relayer serve`)**:
  - Added the `relayer serve` subcommand with `--bind`, `--port`, and `--token` authentication flags, enabling remote multi-agent supervision in cloud devboxes, EC2/GCP instances, and Docker containers without local GUI installations.
  - Implemented an embedded HTTP and WebSocket server (`internal/server`) serving the production React UI and multiplexing terminal snapshots, semantic arbitration events, process lifecycle changes, and error reporting in real time.
  - Implemented `webBridge.ts` client adapter translating JSON-RPC requests and WebSocket push messages directly to the existing frontend application.

- **Web Push Notifications & Browser Audio Alerts**:
  - Implemented the `useNotifications` React hook integrating the HTML5 Notification API to deliver native OS notifications when the browser tab is hidden or running in the background.
  - Added an integrated Web Audio synthesizer (`AudioContext`) providing acoustic warning chimes (880Hz / 587Hz) for human arbitration requests and security blocks without external media dependencies.
  - Created a responsive in-app toast notification stack (`NotificationToast.tsx`) with severity-based coloring, auto-dismiss, and deduplication by event ID.
  - Added a dedicated "🌐 Web & Browser Push Alerts" section in the settings panel with permission request controls, audio toggle, and a test trigger button.
  - Added the `testNotification` RPC endpoint to verify alert channels end-to-end (browser push, audio, and remote Slack/Discord webhooks).

### Fixed

- **Full Settings Persistence & Notifier Live Reload**:
  - Fixed `SaveFullSettings` to atomically persist webhook configurations, notification thresholds, and security policies to the YAML configuration file on disk via `config.UpdateFullConfiguration`.
  - Added instant live reloading of `notify.Notifier` in memory upon saving without requiring a server restart.
  - Enhanced concurrency safety in the WebSocket server with atomic send guards and state cloning to prevent data races during graceful shutdowns.

## [0.4.0] - 2026-09-12

Minor release introducing granular per-agent process lifecycle controls, a comprehensive typography and UI visual overhaul, and cross-platform CI hardening.

### Added

- **Granular Per-Agent Lifecycle (Stop, Start, Restart)**:
  - Added individual `Stop`, `Restart`, and `Start` controls on each agent terminal card in the Desktop GUI, allowing operators to terminate, reboot in place, or restart an agent without interrupting other running processes.
  - Implemented TUI hot keys `x` (Stop active agent) and `r` (Restart active agent) with two-press safety confirmation and timeout disarm.
  - Introduced `internal/app.agentLifecycle` with fail-closed process ownership, `terminal.SessionRemover` backend release verification, and human operator attribution across the entire cryptographic audit trail.
  - Automated E2E Playwright test (`cmd/relayer-gui/frontend/e2e/supervisor.spec.ts`) validating dynamic agent creation from the catalog, form validation, and per-agent lifecycle operations.
  - Unit and integration tests in Go (`profiles_test.go`, `agent_lifecycle_test.go`) and Vitest (`agentProfiles.test.ts`, `relayerState.test.ts`, `AgentCard.test.tsx`) covering all 8 catalog templates, adapter round-trips, and lifecycle states.

### Changed

- **Visual Comfort & Typography Overhaul**:
  - Scaled up small text and table font sizes across the Desktop GUI (Settings editor, Catalog, Audit Trail tables, Webhooks inventory, Preflight checks, and modal cards) from 7.5–9px to 11–13.5px for increased readability on high-DPI displays.
  - Scaled up typography across the Observability & Metrics panel (KPI cards, Donut chart center and legend, histogram values and labels, security guardrails, and telemetry exporters) from 7.5–10px to 10–24px.
  - Optimized latency histogram bar widths, gaps, and compact interval labels to prevent any text clipping or label collision.
  - Added dedicated catalog logo initials and visual color schemes for Aider (`A`), Goose (`G`), Open Interpreter (`I`), and Ollama (`O`).

### Fixed

- **Cross-Platform Audit Permissions & CI Hardening**:
  - Scoped Unix-specific file permission assertions (`syscall.Stat_t`) under `//go:build unix` build constraints to ensure seamless cross-compilation on Windows.
  - Resolved staticcheck unused parameter warnings and standardized code formatting across internal supervisor packages.

## [0.3.1] - 2026-09-12

Patch release addressing GUI agent creation crashes and adapter lockout behavior.

### Fixed

- **GUI Agent Catalog Creation Crash (`nextProfileID`)**:
  - Resolved an uncaught `TypeError: Cannot read properties of undefined (reading 'trim')` crash when adding Aider, Goose CLI, or Open Interpreter profiles via the GUI catalog (`+`).
  - Added full preset mapping and defensive fallbacks in `nextProfileID` to guarantee valid, non-empty IDs for all built-in and third-party CLIs.
  - Added null-coalescing guards in `validateAgentProfiles` to prevent component unmounting on malformed or incomplete profile inputs.
- **Agent Profile Lockout (`editableProfileAdapter`)**:
  - Registered `toolcatalog.GooseCLI` (`goose`) and `toolcatalog.OpenInterpreter` (`interpreter`) in `editableProfileAdapter` and `safeExecutableLabel`.
  - Prevented saved Goose and Open Interpreter profiles from being erroneously flagged as locked `advanced_adapter`, ensuring they remain fully editable within the visual settings editor.

## [0.3.0] - 2026-09-11

General Availability (GA) release of Relayer, concluding the Alpha phase with enterprise telemetry, multi-channel alerting, visual configuration hot-reload, terminal comfort, and strict TUI/GUI parities.

### Added

- **Visual Settings Editor (Desktop GUI)**:
  - Interactive tabbed configuration: `🤖 Agents`, `🛡️ Security & Guardrails`, and `🔔 Notifications & Webhooks`.
  - Zero-downtime hot-reload: security policies, guardrails, and webhook endpoints are updated without restarting or interrupting active agent processes.
- **Terminal Search (`Ctrl+F`)**:
  - Integrated `@xterm/addon-search` in Desktop GUI agent viewports with match counter, match highlighting, and circular navigation (`Enter` / `Shift+Enter`).
- **Rapid Arbitration Shortcuts**:
  - `Alt+1..8`: Direct focus switch and instant arbitration dialog opening.
  - `Ctrl+Enter`: Quick approval (`Allow`) or direct line send.
  - `Esc`: Quick refusal (`Deny`).
- **TUI Metrics Overlay (`m` / `M`)**:
  - Bubble Tea full-screen overlay displaying session uptime, decision counts, guardrail blocks, and operator reaction latency statistics.
- **Enterprise Observability & Grafana Stack**:
  - Official Grafana dashboard template (`telemetry/grafana/dashboards/relayer-dashboard.json`).
  - Pre-provisioned `docker-compose.telemetry.yml` with Prometheus and Grafana.
- **Multi-Channel Alerts**:
  - Native OS notifications on Windows (Toast), macOS (osascript), and Linux (`notify-send`).
  - Webhooks for Slack (`blocks`), Discord (`embeds`), and generic JSON with adaptive rate limiting.

### Changed

- Promoted Relayer to General Availability (GA) status across CLI and Desktop GUI packages.
- Enhanced Desktop GUI typography with enlarged, comfortable font sizes in tables, badges, and inventory cards.



First pre-release of the 0.3.x cycle: multi-platform desktop distribution, Goose & Open-Interpreter agent adapters, contextual path-based policies, automated Playwright E2E testing, PTY stress endurance, and native OpenTelemetry & Prometheus telemetry export.

### Added

- Multi-platform Desktop GUI release workflow (`.github/workflows/release.yml`):
  - Automated Wails compilation matrix across Windows (x64 standalone .exe), macOS (Universal binary for Intel & Apple Silicon), and Linux (WebKitGTK-based distribution).
  - Cryptographic release signing via Sigstore Cosign keyless signatures (`.sig`, `.pem`) and SHA-256 checksums with build provenance attestations.

- New AI Agent Adapters (`internal/adapters`):
  - Block/Goose CLI adapter (`goose`): intercepts tool execution approvals, file editing confirmations, shell execution requests, and prompt withdrawals.
  - Open Interpreter adapter (`interpreter`): recognizes code execution prompts (Python/shell/JS), package installation requests, and interactive user inputs.
  - Full integration into `relayer doctor` inventory and Desktop GUI agent settings catalog with `StatusExperimental` maturity classification.

- Advanced Policy Engine & Contextual Rules (`internal/policy`):
  - Path-based contextual security rules (`sensitive_paths`, `allowed_paths`): prevents unauthorized access or alteration of critical project and system targets (`.env`, `~/.ssh`, keys).
  - Conditional auto-approval: safely auto-approves recognized read-only commands (`git status`, `npm test`, `cargo check`) while strictly requiring human arbitration for writes, installations, or deletions.
  - Built-in policy profiles: `strict` (zero automated allowances), `developer` (convenient read-only defaults), and `permissive` (low friction for sandboxed execution).

- Playwright E2E Automated Test Suite & PTY Stress Endurance:
  - Full desktop supervisor lifecycle E2E tests (`cmd/relayer-gui/frontend/e2e/supervisor.spec.ts`) validating agent startup, dual terminal streaming, sensitive event interception, interactive modal decisions, and compliance journal inspection.
  - PTY/ConPTY high-throughput endurance and concurrency stress tests (`internal/ptybackend/stress_test.go`) demonstrating 0 MB memory leak over 30,000+ ANSI lines and strictly bounded $\mathcal{O}(1)$ ring buffer memory consumption.

- Native OpenTelemetry & Prometheus Telemetry Engine (`internal/telemetry`):
  - Zero-leakage metric collection via `audit.EntryObserver`: metrics are derived solely from already-sanitized audit records, strictly preventing prompt, argument, or token leaks in metric labels.
  - Embedded Prometheus HTTP exporter (default `:9090/metrics`) serving standard v0.0.4 text format with graceful shutdown.
  - Periodic OpenTelemetry OTLP/HTTP JSON v1 metrics exporter (`/v1/metrics`) with configurable export intervals and authorization headers.
  - Comprehensive operational metrics: active sessions, pending events, total runs, detected events, withdrawn events, decisions applied, guardrail violations, and human reaction latency histograms (`relayer_decision_duration_seconds`).
  - Preflight checks (`telemetry.valid` under `ScopeTelemetry`), doctor command integration, and Desktop GUI metadata exposure.

- Desktop GUI Audit & Compliance Panel: added dedicated Audit modal panel accessible
  from the top bar. The panel presents chronological journal inspection, cryptographic
  and sequence continuity verification via `audit.VerifyJournal`, human vs policy
  decision metrics, an interactive filtered entries table with display-safe metadata
  inspection, and instant report exports to JSON and CSV formats.

### Fixed

- A question the agent takes back stops being asked. Detection was reconciled
  with the rendered screen, but an occurrence already awaiting a decision never
  was: nothing compared it to what the agent still had on screen. So a request
  the agent withdrew by itself — a timeout, a cancellation, an ESC typed in the
  attached view — stayed on offer indefinitely, and the operator's answer was
  delivered into a terminal that had gone back to its prompt: a `y` typed into a
  shell. Every write now reconciles the pending occurrence with the visible
  grid. One that has left it is withdrawn, the pane unblocks, the action queue
  drops it, and the audit records that the supervision gate opened without a
  decision — distinguished from the resync that could already withdraw one. A
  decision arriving after the withdrawal is refused rather than delivered.

  Absence of the question's text is never the evidence, because it is absent for
  reasons that have nothing to do with the agent giving up on it: halfway
  through a repaint larger than one read, after scrolling out of view, or when
  the grid stops joining two wrapped rows. Replacement is the evidence. The
  question must have been seen on the visible grid under this occurrence's
  identity, nothing may have left the grid since, and the row that carried it
  must now hold content that is neither blank, nor the question, nor the same
  line serialised differently. Everything that cannot be proved keeps the
  occurrence pending, which is what happened before; an agent that only appends
  is untouched.

## [0.2.0] - 2026-09-11

Second minor release: native Windows support, interactive desktop terminal emulator, audit CLI, Aider adapter, decision alerts, policy engine guardrails, and virtual screen detection.

### Added

- Native Windows ConPTY backend (`github.com/charmbracelet/x/conpty`). Relayer runs
  interactively on Windows without WSL or remote daemons, driving CLI agents through the
  Windows Pseudo Console API. Process groups, window resizing, input routing, and output
  capture are fully supported.

- Interactive xterm.js terminal emulator in the desktop interface. The plain-text console
  in the Svelte GUI is replaced by `@xterm/xterm` and `@xterm/addon-fit`, providing full
  terminal emulation with faithful ANSI color rendering, cursor positioning, progress
  bars, and alternate screen buffers.

- `relayer audit` CLI inspection tool. Dedicated subcommands inspect and verify Relayer's
  append-only JSONL audit trails:
  - `relayer audit inspect` for formatted, human-readable session playback and event filtering.
  - `relayer audit stats` for aggregated decision metrics and agent breakdown.
  - `relayer audit verify` for cryptographic Ed25519 signature verification across the audit chain.

- Native Aider coding assistant adapter (`internal/adapters/aider.go`). Dedicated pattern
  recognition and response encoding for the Aider AI pair programmer CLI, supporting
  tool execution confirmations, file modification prompts, and command execution approvals.

- Desktop notifications and terminal BEL alerts (`internal/notify`). When an agent prompts
  for human decision, Relayer emits a terminal BEL (`\a`) audio cue and sends a native OS
  desktop notification via `beeep` to alert operators working in background windows. Gracefully
  falls back or silences in headless and quiet environments.

- Policy engine guardrails and rate-limiting (`internal/policy`). Added consecutive
  auto-decision threshold (`max_consecutive_auto_decisions`) forcing human review after
  repeated automated allowances, sliding-window rate limiting (`max_actions_per_minute`)
  to prevent runaway loops, and high-risk destructive command inspection (blocking destructive
  shell commands such as `rm -rf`, `mkfs`, `dd`, and exfiltration patterns).

- Virtual terminal screen model (`internal/screen`). Terminal byte streams are parsed
  through a total escape sequence parser (CSI, OSC, DCS, SOS, PM, APC) into a 2D virtual
  character grid. Prompt detection now operates on the rendered screen rather than raw byte
  chunks, eliminating chunk boundary fragmentation and repaint artifacts.

### Fixed

- Detection on rendered screens: prompt detection evaluates the actual visual grid state,
  eliminating chunk-boundary misses when questions are split across multiple read system
  calls or surrounded by pinned status footers and frames (#29).

- ANSI DoS protection: terminal screen parser strictly bounds escape sequence parameter
  lengths and grid memory allocations to prevent denial-of-service from unbounded or
  malicious byte sequences.

- Question identity by full line: questions are keyed by their complete text line rather
  than partial ambiguous fragments (#30).

- Escape-only writes trigger detection: terminal updates consisting solely of cursor
  motion or styling escape codes without printable text correctly trigger screen
  re-evaluation (#31).

- Code-fence parity tracking: orphaned or unmatched markdown code fences in agent outputs
  no longer confuse multi-line prompt block detection (#32).

- Codex adapter repaint idempotence: answered prompts are preserved across screen repaints,
  preventing duplicate human decision requests when cursor or spinner updates occur (#33).

- Answered question memory retention across partial read chunks (#35) and tmux snapshot
  reconciliations (`ReconcileSnapshot`) (#37).

- Detection burst bounding: detection is constrained to the visible viewport grid,
  preventing ghost prompts from past scrollback lines (#36).

- ECH (Erase Character) and DCH (Delete Character) escape sequences mark screen regions as
  repaints (#38, #44).

- Multi-line question continuation boundary expanded to 16 lines to support large diffs
  and command preview displays (#39).

- A question the operator answered stops suppressing a different one. The memory
  that keeps an answered question from being asked again while it is still
  painted was tied to a row located after the fact, by searching the grid for the
  matched text — which returns the last row carrying it. With a fragment like
  `[y/n]` that is routinely another question, or a line detection had explicitly
  excluded as a candidate, such as one prefixed `log:`. The entry then watched a
  line that never changes, so it never expired and the re-asked question was
  swallowed for good: the operator was never asked and the agent waited forever.
  The row now comes from the render itself, at the moment the question is
  detected, and travels with the occurrence.

- An answered question is no longer re-asked after a scroll region moves. Rows
  were named by an index plus a count of what had scrolled away, and that count
  stood still under a scroll region whose top is the first row, and on the
  alternate screen. Rows that had never moved stopped recognising themselves, the
  memory was released, and the question came back — so the operator answered a
  second time and the keystroke reached an agent that had already acted on the
  first. A row is now named by an identity carried on the row itself, which
  follows the content when scrolling, insertion or deletion move it.

- A frame the agent moves while the operator is deciding no longer re-asks the
  question. The row is taken when the question is detected and the answer comes
  many frames later; an agent that erases and repaints its frame at a different
  height moves the question onto another row without ever taking it down. The
  row of the occurrence awaiting a decision is now kept current, so the memory
  written when the operator answers describes where the question actually is.

- Abandoning a question no longer records it as answered. A snapshot that comes
  back empty, a snapshot with nothing detectable on it, and a process that exited
  all wrote into that memory, leaving an entry describing a screen the caller had
  just declared stale. Nobody decided anything on those paths.

## [0.1.1-alpha] - 2026-08-29

Alpha still: configuration, adapter, backend and audit APIs may change without
compatibility guarantees.

### Fixed

- A prompt is found wherever the read boundary fell. Detection kept a match only
  when it touched the last non-empty line of the accumulated window, so whether
  an agent was supervised depended on where the operating system happened to end
  a read: a question written together with the frame, option list or footer
  beneath it was missed, and the identical bytes split across two reads were
  caught. Measured against this repository's own captured Codex screen — a
  question wrapping over three lines above a choice list and a key hint — the
  generic adapter found nothing in any chunking. Any CLI that paints a prompt
  that way, which is most of them, was supervised by the default adapter in name
  only.
- Codex keeps recording while a prompt is pending. The adapter returned before
  writing the chunk into its detection window, so a second prompt arriving while
  a human was deciding was never in the text at all: nothing could recover it,
  and the request vanished with the agent still waiting on it.
- The desktop interface no longer weights the permissive answer. **Allow** was
  the loudest control in the application — a filled gradient with a glow — while
  **Deny** was a low-opacity tint. In a tool whose purpose is to make a person
  stop and choose, the eye picked the permissive one. Both carry the same weight
  now, and the filled treatment is reserved for the single action a screen is
  asking for.
- The decision modal can scroll. It had `overflow: hidden`, no maximum height,
  and its only scroll rule sat behind a media query the shipped window can never
  reach, so a tall prompt was clipped and took its own controls with it.
- Text contrast meets WCAG. The faint tier measured 2.99:1 against the surfaces
  it actually composites onto and failed at every one of its text call sites;
  all three text tiers were re-spaced to 13.7 / 7.7 / 5.1, measured on the
  rendered page rather than computed on paper.
- Focus indicators are visible again. `outline: none` sat in a base rule rather
  than inside `:focus`, removing the ring in every state, and the settings fields
  replaced it with a shadow at 0.07 alpha. The three dialogs that had no Escape
  handler and no focus trap now share one, which also recovers focus when a
  control is disabled underneath it mid-decision.
- The top bar no longer reports a queue before a run exists, the agent
  catalogue no longer cuts its descriptions mid-word, and a session that has
  exited no longer claims to be under supervision.

### Added

- `internal/screen` renders a terminal byte stream into the grid a person
  actually sees, with a deliberately total escape parser: every CSI, OSC, DCS,
  SOS, PM and APC sequence is recognised and consumed. Detection is not wired to
  it yet — that needs a terminal size the adapters package cannot currently
  reach — so the two repaint cases it fixes are kept in the suite, running and
  skipped, naming what has to land.
- The desktop interface answers with the adapter's own encoding. **Allow** and
  **Deny** appear for the exact occurrences an adapter has verified bytes for,
  probed per event rather than assumed per adapter, and the prompt carries the
  end of the agent's own output so the decision is not made on a one-line
  summary.
- Substituted demo agents are marked as such on screen, and the startup facts
  that used to go only to standard error — which a windowed application does not
  have — reach the supervisor panel.
- The README shows the desktop interface. It animated the terminal one and
  described the other without ever showing it.

### Changed

- The whole interface is English, terminal and desktop, including the
  configuration file Relayer writes for a new install. French remains only where
  it is data rather than prose: the sensitivity patterns matched against what an
  agent prints, which exist to catch French-speaking CLIs.
- staticcheck's ST1005 applies to the desktop module. Its errors were carrying
  the sentence shown to the operator; they are ordinary Go errors now, and the
  interface phrases them where they are displayed.
- `golang.org/x/sys` moves to v0.44.0, clearing the last advisory govulncheck
  reported — unreachable from Relayer's code and on a platform the release does
  not build, but not worth shipping.

## [0.1.0-alpha] - 2026-08-28

First tagged release. Alpha: configuration, adapter, backend and audit APIs may
still change without compatibility guarantees.

### Added

- The supervisor title shows how many agents are waiting once more than one is.
  Only the agent being answered was named and only four are visible per page, so
  a queue building up behind it — on another page — was invisible.

- Release artifacts are signed and attested. The checksum file is signed with
  keyless cosign, so the release workflow's own identity is bound into a
  short-lived certificate and recorded in the public transparency log; build
  provenance is attested separately, and each archive ships an SBOM. A checksum
  published beside the binaries only proves the download was not corrupted, and
  anyone able to write to the release could replace both — which matters here,
  because a tampered `relayer` can approve everything and still write a
  plausible audit log. The README documents `cosign verify-blob`, including the
  identity flags without which any valid signature would pass.
- CI now builds the release archives on every change instead of only validating
  the configuration schema, so the first real run of the release pipeline is no
  longer the publishing one.

- `F2` and `F3` answer a pending prompt with an explicit allow or deny in the
  terminal interface. The adapters already encoded both and the audit already
  modelled a decision made by a person, but no operator surface could ask for
  either, so every human answer went through the free-text field and was
  recorded as an ask — `decision=deny, by=human` was impossible to produce. An
  adapter that cannot represent the answer leaves the prompt pending instead of
  having terminal bytes invented for it; today only the Codex adapter encodes
  them.

- `intercept_patterns` entries accept an optional `sensitive: true`, which masks
  the operator field and forces a human decision. Sensitivity was inferred from
  the pattern text alone, so a prompt worded outside that word list was entered
  unmasked with no way to correct it. The inference also now recognizes
  one-time-code, 2FA/MFA, recovery-code, private-key and seed-phrase wording in
  English and French. Declaring the field can only escalate: `sensitive: false`
  never downgrades an inferred secret.
- `examples/local.yaml`, a worked configuration showing real agents, mixed
  backends, a policy rule and a sensitive pattern. The README referenced this
  path but the file did not exist.

- A Bubble Tea supervisor for one to eight local interactive agents, with
  paging, viewport navigation, mouse scrolling, prompt focus, and bounded logs.
- PTY sessions with process-group ownership, resize propagation, input routing,
  output retention, and shutdown cleanup.
- Relayer-owned tmux sessions with private launch transport, streamed output,
  native attach/resync, explicit persistence policy, and ownership-verified
  cleanup.
- A product-neutral generic regex adapter with ANSI streaming, carriage-return
  handling, active-prompt filtering, occurrence IDs, snapshot reconciliation,
  and bounded state.
- Experimental, version-specific Claude Code and Codex CLI adapters backed by
  anonymized terminal fixtures, with the generic regex adapter retained as a
  compatibility fallback.
- A bounded output-only PTY/tmux fixture capture command with strict JSON,
  centralized secret refusal, private tmux isolation, and offline validation.
- A bounded, single-line operator input path for the TUI and desktop GUI that
  never acknowledges a pending event and never records the submitted text.
- A shared read-only doctor/preflight report for the CLI and desktop GUI, with
  passive executable discovery, effective adapter/backend reporting, static
  remediation, and no configuration, audit, backend, or process side effects.
- A combined Ollama / DeepSeek launch profile that requires the exact `run`
  subcommand and a user-selected model instead of inventing either.
- Strict version 1 YAML for agents, backends, session lifecycle, prompt
  patterns, policies, and audit recording.
- Conservative first-match policy evaluation with allow, ask, deny, dry-run,
  sensitive-event, credential, and risk safeguards.
- A local versioned JSONL audit recorder with metadata and detailed modes,
  bounded redaction, rotation, private Unix storage, lifecycle records, and
  fail-closed delivery behavior.
- Deterministic Bash mock agents and a VHS tape that does not depend on a
  vendor CLI or captured transcript.
- Development build version output through `--version`.

### Changed

- The minimum build toolchain is Go 1.25.13. The desktop module reached two
  vulnerable standard library paths through its Wails dependencies that the root
  module never calls — `crypto/x509`, and `encoding/asn1` under the Linux
  WebKit build tag — so the previous 1.25.8 pin left reachable vulnerabilities
  in the one module CI did not scan.

- Go error strings are in English. Roughly 40% of them were French, so an
  operator got a mix of two languages and could not search for the message they
  hit — a genuine support cost: `identifiants immuables tmux invalides` returns
  nothing anywhere. Format verbs, wrapping and the deliberate vagueness of
  messages that must not leak paths or terminal output are unchanged. The
  desktop application keeps French for now: its interface and its messages are
  one unit, and mixing them would be worse than either.

- The minimum build toolchain is Go 1.25.8 so release binaries include the
  standard-library security fixes enforced by `govulncheck`.
- `--pane1` and `--pane2` remain available as deprecated direct-command
  overrides; versioned YAML is the primary interface.
- Empty version 1 agent lists retain the historical two-mock quick start.
- Legacy direct pattern lists and `intercept_patterns` wrappers remain readable,
  with conservative defaults and auditing disabled for compatibility.

### Fixed

- Two tests reported a genuine failure as a skip. Starting the foreign tmux
  session in the fixture-capture test, and inspecting processes after a rollback
  in the app integration test, both ran an operation and skipped on any error —
  so a regression in the code under test looked like an absent capability. That
  is structurally how the tmux 3.7 format break stayed hidden behind a green
  suite. Both now fail with the underlying error, and `CONTRIBUTING.md` already
  stated the rule they violated.

- An operator on an unsupported platform was told only that agent execution is
  unavailable, never why. The build carried the explanation — a ConPTY backend
  must be implemented and tested first — in a function nothing called, which is
  what a `staticcheck` run on this module reported first.

- A half-composed direct instruction was discarded silently whenever a prompt
  took the shared input field. Preempting it is correct — a supervision request
  outranks a note to an agent — but nothing was logged, so the operator's
  sentence vanished under them with no explanation. The discard is now
  announced; the text itself is still never recorded.

- Pressing Enter on an empty supervisor field answered the prompt. The generic
  adapter encodes a manual decision as the typed text plus a carriage return, so
  an empty submission delivered a bare carriage return — whatever the prompt
  treats as its default, frequently the permissive one — and it was recorded as
  a human decision. Because the field takes focus by itself when a prompt
  arrives, a reflex keystroke could answer a prompt the operator had not read.
  An empty or whitespace-only manual decision is now refused in the core, so
  every interface is covered, and the terminal interface says so instead of
  doing nothing. The desktop interface already refused to submit an empty field.

- Withdrawing a pending occurrence left no trace. Snapshot reconciliation drops
  a prompt the replayed screen no longer shows — legitimate, since an operator
  attached to tmux may have answered it directly — but the pane simply stopped
  being blocked and the journal could not tell "answered" from "stopped being
  asked". A new `event_withdrawn` record names the occurrence, and like every
  other kind it has no field for the matched text.

- A prompt drawn inside an ASCII frame was never detected. The line
  `| Overwrite file? [Y/n]        |` shares its `| ` prefix with a markdown
  table row, which the generic adapter suppresses as quoted documentation, so
  the prompt was dropped at every chunk size with nothing to indicate it. A
  table row separates cells and carries more pipes than the two a frame uses to
  close its sides, and the two are now told apart.
- `docs/adapters.md` claimed that "correctness cannot depend on read
  boundaries". It does: a match must reach the active line, so a question
  followed in the same write by its own frame or option list is missed while the
  identical bytes split across reads are detected. The claim is removed and the
  limit is documented, including which patterns avoid it.

- A prompt that arrived while another one was pending was lost for good.
  Detection stops examining output while an occurrence is pending, so the second
  prompt reached the detection window and nowhere else — and answering the first
  one wiped that window. The second prompt was never evaluated by a policy,
  never audited, and still on the agent's screen, where the operator's next line
  or even their refusal of the first prompt would answer it. Claude Code's
  trust-prompt-then-tool-prompt sequence hits this every time, on both backends;
  there is no periodic reconciliation to recover it. The window is now retained
  and re-examined when the pending occurrence is answered, and dropped when
  nothing unexamined survives.

- A prompt laid out with cursor movement instead of spaces was invisible to
  every configured pattern. Claude Code 2.1.59 emits no literal space in its
  prompts, only `ESC[1C` between words, and those were stripped without
  substitution: the detector matched against `DoyouwanttousethisAPIkey?`, so
  the shipped `intercept_patterns` — and any pattern an operator writes with a
  space in it — could never fire. Deterministic, silent, and on the documented
  compatibility path. Cursor-forward escapes are now expanded to the spaces they
  produce, under a bound, since the column count comes from untrusted output.

- The audit sink took ownership of `audit.path` without checking what was
  already there. Startup truncates a partial trailing line and rotation removes
  generations beyond `max_files`, both purely by name, so pointing the setting
  at an ordinary document destroyed it: a file containing no newline at all was
  truncated to nothing, and neighbours matching `<base>.<n>` were deleted. On a
  private home directory nothing objected. A non-empty file is now only touched
  when its first line decodes as a Relayer entry with a known schema version and
  kind; `doctor` reports a foreign file as a blocker beforehand, and a journal
  interrupted while writing its first entry still recovers.

- The agent update path validated and re-read configuration files with `Load`,
  which creates a default configuration when the path is absent. Used on the
  temporary file of an in-flight save, a file that disappeared between writing
  and validating would be recreated with defaults and then renamed over the
  real configuration, replacing the user's agents, policies and audit settings
  while reporting a successful save. Every update, snapshot, revision and
  desktop profile call site now uses `LoadExisting`; only first-run bootstrap
  may create a file.

- Detection throughput was roughly 0.15 MB/s. The normalized detection window
  was rebuilt one rune at a time with `s.detectionText += ...`, copying the
  whole 16 KiB window per character, so consuming output was quadratic in chunk
  size — and it ran while holding the processor lock, serializing detection,
  operator input, and every snapshot the interfaces poll. A noisy build log
  could saturate a core and fall behind. The window now accumulates into a byte
  buffer: about 142x faster and 1650x less allocation on the same input,
  measured by a new benchmark. A differential test and a fuzz target check the
  rewrite against the original implementation.

- The tmux backend could not start a session on tmux 3.7. Machine-readable
  `-F` and `display-message` formats separated their fields with a tab, and
  tmux 3.7 rewrites an unprintable byte in rendered format output to `_`
  whenever `TMUX` is absent from the environment, which is the normal case for
  a Relayer launched from an ordinary shell. Every identity, ownership and
  snapshot response therefore failed to parse. Those formats now use a
  printable separator, guarded by a unit test that rejects an unprintable one
  on any platform and an integration matrix that pins the wire contract with
  `TMUX` absent, empty, and pointing at a foreign server. The same rewrite also
  broke `relayer-capture` and the tmux fixture capture.

- The `relayer-capture` SIGTERM test consumed the whole `go test` timeout
  instead of reporting a failure. Its single-slot wait channel could be drained
  by an early-exit path, after which the cleanup receive blocked forever. The
  cleanup now receives under a bounded select and the child has a `WaitDelay`,
  so a capture that exits early reports its real error in seconds. The test's
  capture record is also published through an atomic rename instead of a
  truncating write, so the polling reader can no longer observe a partial file
  and fail with a JSON error.

- Startup and `doctor` established tmux availability by finding the executable,
  so a tmux that could not run a session was selected anyway and failed at the
  first session start, after the readiness report had announced a healthy
  backend. Both now run a bounded functional probe: one short-lived session on
  a private socket inside a `0700` temporary directory, its identity parsed by
  the runtime parser, then removed by name. An unusable tmux blocks an
  explicitly requested tmux backend and makes `auto` fall back to PTY, each with
  a message distinct from tmux being absent. The probe never reads, attaches to,
  or modifies the user's tmux server and never calls `kill-server`; the doctor
  documentation records it as the one deliberate exception to passive checking.

### Security

- CI scans the desktop module. It carries the dependency surface — echo,
  gorilla/websocket, `x/net`, `x/crypto` all arrive with Wails — while the
  scanned root module has far fewer, so `staticcheck`, `govulncheck` and the
  `go mod tidy` diff were pointed away from the code that needed them. The
  frontend job additionally audits its locked dependency tree.

- Backend and adapter selection, policy validation, and audit initialization
  happen before agent startup.
- Partial startup and failed session-start audit writes trigger rollback.
- tmux cleanup requires immutable Relayer ownership evidence and never kills
  the tmux server.
- Terminal input values, raw output, commands, environment values, prompt
  matches, and raw errors are excluded from the audit model; direct input
  records retain only static lifecycle metadata.
- Audit storage rejects unsafe leaf symlinks and non-regular targets and checks
  private Unix ownership and permissions.

[Unreleased]: https://github.com/Hocsman/Relayer/compare/v0.8.8...main
[0.8.8]: https://github.com/Hocsman/Relayer/compare/v0.8.7...v0.8.8
[0.8.7]: https://github.com/Hocsman/Relayer/compare/v0.8.6...v0.8.7
[0.8.6]: https://github.com/Hocsman/Relayer/compare/v0.8.5...v0.8.6
[0.8.5]: https://github.com/Hocsman/Relayer/compare/v0.8.4...v0.8.5
[0.8.4]: https://github.com/Hocsman/Relayer/compare/v0.8.3...v0.8.4
[0.8.3]: https://github.com/Hocsman/Relayer/compare/v0.8.2...v0.8.3
[0.8.2]: https://github.com/Hocsman/Relayer/compare/v0.8.1...v0.8.2
[0.8.1]: https://github.com/Hocsman/Relayer/compare/v0.8.0...v0.8.1
[0.8.0]: https://github.com/Hocsman/Relayer/compare/v0.7.1...v0.8.0
[0.7.1]: https://github.com/Hocsman/Relayer/compare/v0.7.0...v0.7.1
[0.7.0]: https://github.com/Hocsman/Relayer/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/Hocsman/Relayer/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/Hocsman/Relayer/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/Hocsman/Relayer/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/Hocsman/Relayer/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/Hocsman/Relayer/releases/tag/v0.3.0
[0.3.0-alpha.1]: https://github.com/Hocsman/Relayer/compare/v0.2.0...v0.3.0-alpha.1
[0.2.0]: https://github.com/Hocsman/Relayer/compare/v0.1.1-alpha...v0.2.0
[0.1.1-alpha]: https://github.com/Hocsman/Relayer/compare/v0.1.0-alpha...v0.1.1-alpha
[0.1.0-alpha]: https://github.com/Hocsman/Relayer/releases/tag/v0.1.0-alpha
