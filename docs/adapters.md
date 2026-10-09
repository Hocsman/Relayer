# Adapters and events

Adapters translate normalized terminal output into semantic events and encode a
decision back into terminal bytes. This interface is internal and mutable
during alpha; it is not a runtime plugin protocol.

## Current registry

| ID | Registry status | Implemented | Behavior |
| --- | --- | --- | --- |
| `generic` | Stable relative to the built-ins | Yes | Ordered regex prompt detection; manual input encoding. |
| `aider` | Experimental | Yes | Aider 0.86.2 questions: running a shell command, creating a file, editing a file not in the chat, adding a file or a command's output to the chat, adding to `.gitignore`; allow (`y` Enter), deny (`n` Enter) and manual input, each observed against a disposable repository. |
| `claude` | Experimental | Yes | Claude Code 2.1.59 workspace trust and environment-key prompts, the Bash and create-file prompts of 2.1.285 and 2.1.286 and the edit-file prompt of 2.1.285; generic fallback; manual input only. |
| `codex` | Experimental | Yes | Codex CLI 0.148.0-alpha.21 directory trust and command approval; generic fallback; command allow/deny and directory deny bytes verified. |
| `goose` | Experimental | Yes | Goose 1.52.0 tool-call approval menus, with and without an approval notice, in Unicode and ASCII; allow and deny picked by menu keys (`k`… `j`… Enter), and manual `allow`, `deny` or `cancel`, each observed. |
| `interpreter` | Experimental | Yes | Open Interpreter 0.4.3 questions: running a code block, and scanning it first under `--safe_mode ask`; allow (`y` Enter), deny (`n` Enter) and manual input, each observed. `open-interpreter` is accepted as an alias. |

“Stable” here is a registry maturity label, not a promise that the alpha API
will remain source-compatible.

If `agents[].adapter` is blank, the registry considers implemented executable
hints and then falls back to `generic`. A basename of `aider`, `claude`,
`codex`, `goose`, `interpreter` or `open-interpreter` selects the corresponding
experimental adapter. Every experimental adapter retains each configured
`intercept_pattern` as a generic compatibility fallback.

Every vendor adapter is backed by output under `internal/adapters/testdata`,
except the Claude Code 2.1.286 layouts, which rest on strings in `claude_test.go`
(and the 2.1.285 cases have unconfirmed provenance); an installed version that
words its questions
differently is not detected by it and falls back to `generic`. Aider's, Open
Interpreter's and Goose's patterns were written by hand from documentation
until the capture, which found that half of Aider's were questions Aider 0.86.2
never asks; that Open Interpreter 0.4.3 asks only two questions, neither worded
as the patterns had it, nothing before installing a package or saving a file,
and a shell command through the same question as any code block; and that
Goose 1.52.0 asks no `(y/n)` question at all. It asks with a menu, where `y`
and `n` do nothing and Enter picks the highlighted option, Allow: the `n` and
Enter the adapter sent for a deny would have allowed the call, had any of its
patterns ever matched Goose's output.

The desktop catalogue also contains generic launch profiles for MiMo Code, a
combined Ollama / DeepSeek entry, and a custom CLI. A launch profile is not an
adapter claim. In particular, no MiMo, Ollama, or DeepSeek prompt protocol is
implemented; those profiles use `generic` detection.

An unknown or unavailable explicit ID is a configuration error before a
terminal backend starts.


## Cursor movement and spacing

Some agents lay a prompt out by moving the cursor rather than emitting spaces.
Claude Code 2.1.59 does: its recorded prompts contain no literal space at all,
only `ESC[1C` between words.

Relayer substitutes a cursor-forward escape (`ESC[<n>C`) with the spaces it
visually produces before stripping the remaining ANSI. Without that step the
detector matched against `DoyouwanttousethisAPIkey?`, so any configured pattern
containing a space could never fire, silently — including the shipped defaults.
Each substitution is bounded, because the column count comes from untrusted
output.

Only horizontal movement is modelled. Absolute positioning and vertical
movement would need a screen model, which this package does not have, so a
prompt drawn with those still needs a pattern that tolerates missing spacing.
Writing `\s*` between words, as the vendor adapters do, remains the robust
form for anything you author yourself.

## Event model

An adapter event contains:

- a unique occurrence `ID` and stable replay `Signature`;
- a monotonically assigned per-state sequence;
- session and agent IDs;
- adapter ID;
- type: `confirmation`, `permission`, `credential`, or `process_exit`;
- bounded display summary and the internal matched text;
- sensitive and risk classification;
- timestamp and copied metadata.

Confirmation, permission, and credential events are actionable. Process exit
is a canonical lifecycle event and is not sent to policy for automatic
approval.

The event match is needed internally for detection, signature, and policy regex
evaluation. It is not a field in the local audit schema. Sensitive events also
omit derivative event IDs from audit records because a signature-derived value
could aid guessing low-entropy credentials.

## Streaming processor

Every session has independent adapter state. The processor consumes arbitrary
byte chunks from PTY or tmux. It:

1. appends original output to a bounded 256 KiB ring for display;
2. incrementally removes ANSI control sequences, including escapes fragmented
   across chunks;
3. normalizes terminal line behavior, including `\r` rewrites;
4. bounds the separate detection window to 16 KiB;
5. calls the adapter with only the new normalized effect and current state;
6. stores at most the current pending actionable occurrence;
7. emits copied events through the backend session channel.

Snapshot reconciliation uses the same normalized state. Replaying the same
pending snapshot does not create a second event. After acknowledgement, seeing
only the same historical occurrence does not reblock. A genuinely new identical
prompt receives a different occurrence ID. Two identical prompts in sequence
are therefore distinguishable without inventing vendor-specific markers.

An occurrence ID also carries a random token drawn when the agent's process
starts. The sequence in an ID starts again with each process, and a signature
can be as little as the `[y/n]` a pattern captured, so without the token a
restarted agent's first prompt had the previous process's ID: a decision on the
old prompt was delivered to the new one. The token is never recorded, and a
decision that names the previous process's ID is refused.

## Generic regex adapter

`intercept_patterns` is compiled before any agent starts. Pattern order is
significant: the first applicable match is emitted, and no additional
actionable event is emitted while one occurrence is pending.

Output produced during that window is retained rather than discarded. Answering
the pending occurrence is the moment it becomes examinable, so a prompt the
agent asked while the first one was unresolved is reported then instead of
being lost. The occurrence just answered is not reported again: it is
recognized by its signature and skipped. If nothing unexamined survives, the
retained text is dropped, so answered output can never merge with what arrives
next.

A generic match must overlap the active terminal line affected by the newest
normalized chunk. This prevents old retained output from becoming actionable
simply because a new unrelated line arrived.

### The actionable region

Detection once kept a match only when it touched that active line, which tied
supervision to where a read happened to end: a question written together with
the frame, option list or footer beneath it was missed, while the identical
bytes split across reads were detected. It failed silently and in the unsafe
direction, and it was not an edge case — the captured Codex directory-trust
screen, whose question wraps over three lines above its choices, was detected in
no chunking at all.

The region is now everything the current write produced, and two rules keep that
from becoming a flood:

- The fence parity is unwound backwards from the end of the window, so a match
  is judged by the fence state at **its own line**. An earlier attempt at
  widening used the window-end flag and turned a documented example inside a
  fenced block into a real supervision event.
- Everything below the match must be the agent's own furniture: decoration, a
  choice list, a key hint, or a bounded run of blank lines. A question with real
  content beneath it has been overtaken and is history. The wrapped remainder of
  a long question is allowed to cross this rule, but only when a choice or a key
  hint appears below it — without that anchor a paragraph is just output.

The empty tail is furniture by definition, so a match that still reaches the
active line cannot be rejected. The change is monotone: nothing detected before
stops being detected.

### The rendered screen

`internal/screen` renders the byte stream into the grid a person would see:
cursor addressing, erases, scroll regions, insert and delete, the alternate
screen, and wrapping at the terminal width. Its escape parser is total — every
CSI, OSC, DCS, SOS, PM and APC sequence is recognised and consumed, including
the ones the screen does nothing with, because failing to recognise one would
print its bytes as text, and an unrecognised erase would leave stale cells live.

What an operator sees comes from that screen as soon as the agent does something
an appended byte stream cannot express: addresses a row, erases, scrolls a
region, or switches screens. An agent that only prints and advances is reported
as not repainting and keeps the appended bytes exactly, so nothing changes for
the common case. The screen follows the terminal size through both backends, at
start and on every resize.

Detection reads that screen too. For a repainting agent the actionable region is
the set of rows the write touched — reported by the screen, since on a grid a
write is rows rather than a range of byte offsets, and a frame drawn top to
bottom then filled in the middle touches them out of order. The region runs from
the earliest touched row: it can include a row the write did not touch, never
exclude one it did, because excluding is the direction that hides a question.

Two consequences worth stating. A question painted into a frame by addressing
the cursor is detected, which a byte stream could not do. And a question the
agent has ERASED stops being reported — on a byte window it stayed matchable
forever, so an operator could be asked to answer something no longer on screen.

An answered question stays painted until the agent redraws without it, so what
was answered has to be REMEMBERED rather than the text forgotten. A byte window
solved this by dropping the answered text outright; a screen has no history to
drop. The memory is a bounded set — two questions can sit on screen at once, and
answering the second must not resurrect the first — and each entry is tied to
the row the question was DETECTED on, because the same text lower down is a
different question.

That row comes from the render, not from a search. The screen serialises itself
once and reports, along with the text and the burst, which row painted each
logical line; the detector converts the offset it already holds into that row at
the moment it raises the occurrence, and the row travels with the occurrence
into the memory. Locating the row afterwards by searching the grid for the
matched text is what the memory used to do, and it returns the LAST row carrying
that text: with a fragment like `[y/n]`, that is routinely another question, or
a line detection had explicitly excluded as a candidate — a `log:` prefix, a
quoted example. The entry then watched a line that never changes and could never
expire, and the re-asked question was swallowed for good.

A row is named by an identity stamped on the row itself rather than by an index
plus a count of what scrolled away. The count has to be maintained by every path
that moves a line, and it was not: it stood still under a scroll region and on
the alternate screen, so a row that had never moved stopped recognising itself.
The identity is carried by the row structure that scrolling, insertion and
deletion already move, so it follows the content it names. An ERASE deliberately
keeps it: a full-screen agent erases the frame it is about to repaint on every
tick, and a name that changed every tick would name nothing.

The row of the occurrence awaiting a decision is kept current while the agent
repaints underneath it. The anchor is taken when the question is detected and
the operator answers many frames later; an agent that erases and repaints its
frame at a different height moves the question onto another row without ever
taking it down, because an erase keeps a row's identity and the content slides
by one. Only an unambiguous sighting moves the anchor: when two rows show the
same words there is no way to tell which one the occurrence is.

Once the question is answered the entry no longer follows it. If the frame moves
AFTER the answer while the answered question is still painted, the entry is
dropped and the question is put to the operator once more. That is the behaviour
on the byte window too, so nothing is lost here — and the alternative is worse:
an entry that goes looking for its words elsewhere would, on a screen where the
answered question has scrolled away and the same words have been asked again
lower down, suppress the new question. Two tests pin that case.

Two things that looked like a move are not one, and each asked an answered
question again, which under an automatic policy means a second answer typed into
the agent. A full-screen program run after the answer, an editor or a pager,
takes the alternate screen, and the primary one comes back unchanged with the
answered question still on its row. While the program runs, that row is parked
with the primary screen: it is on no visible grid, but it is not gone, and the
entry stays. An entry that has no row yet stays too, and is looked for once the
primary screen is back rather than on the program's grid. And a terminal that
loses height keeps the cursor's row in view and pushes the rows above it into
the scrollback, as xterm and conhost do. The screen kept the top rows instead,
which dropped the row the agent had just asked on, and ConPTY's repaint after
the resize drew the answered question on a row the memory did not know.

An entry kept for the primary screen answers for nothing on the program's
screen, which is not where it was answered: the identical question asked there
is asked. Taking part, an answered row that was blank when the program started
let the generic adapter take that question for the answered one moved, and an
entry with no row matched it anywhere, since without a row the line alone
decides; after a full reset, which on Unix leaves the screen on the program's
grid for good, that lasted for the rest of the session. An entry with no row
remembered while the program has the grid is about a question on that grid,
raised from a tmux snapshot of it and answered before any write could find its
row. It is looked for there as on the primary screen: adopted by the row that
shows it, or kept for the primary screen when no row does. Kept without a row,
it matched its line anywhere on the program's grid, and the agent asking that
question again later was put to nobody. A question answered on the program's
grid is not the one answered underneath either, even in the same words: taken
for it, the primary screen's answer moved onto the program's row, went with it
when the program exited, and the question still painted underneath was asked a
second time.

On ConPTY a program's full reset does not leave the screen on its grid:
ConPTY turns it into leaving the alternate screen, a repaint of the primary
one, a reset, and a blank repaint. The answered row is then blank, and the
generic and Claude adapters' blank-row rule swallows the identical question
drawn above it, as after a clear. And a question raised from a tmux snapshot of
a program's grid, answered, then erased by a bare `ESC [H ESC [2J` before the
agent writes anything else, is kept for the primary screen, and the redraw asks
it again; v0.8.7 did the same.

While the program runs, only its screen follows the terminal. The primary one
is resized once, to the size then in force, when the program exits, which is
what ConPTY does: it leaves the primary buffer alone while the alternate one is
shown, so a window dragged smaller and back while an editor was open gives the
primary screen back exactly as it was. Resized at every step of the drag, it
came back higher than ConPTY drew it, and the answered question was asked
again. It also minted its new rows from the counter it was parked with, which
the program's screen had been using since: a pane that more than doubled its
height under a full-screen agent — four agents to one in the desktop grid —
reported a row of the agent's screen parked, an answer given there was kept for
the rest of the program, and the same dialog asked again on that row was put to
nobody.

All of this needs the switch to the alternate screen to reach Relayer. The
ConPTY of Windows 11 passes it on; that of Windows Server 2022, and presumably
of Windows 10, does not: it paints the program over the primary screen and
paints the primary screen back when the program exits. The answered question's
row then shows the program's text, the memory lets it go, and the repaint draws
the question again: it is asked a second time, and under an automatic policy a
second answer is typed into the agent, as before this release. Nothing in the
bytes tells that repaint from the agent asking the question again. Since
v0.8.9 the Desktop GUI and the web gateway ask, rather than answer, a question
repeated less than two seconds after its answer (`repeat_after_delivery`),
whatever the adapter; a program that stays open longer than that, as an editor
or a pager does, is not covered by it.

On the rendered screen the row also decides what an entry SUPPRESSES: the
answered question is the one on its own row.

- On the answered row, a line that still BEGINS with the answered question is
  that question, whatever follows it. What follows is the echo of the answer. A
  terminal in cooked mode echoes the keystrokes after the question, ConPTY
  flushes that echo as a write of its own whenever the agent is slow to print,
  and a comparison of whole lines could not see through it: every such question
  was asked again on the next write.
- On another row, the identical line is the agent asking again, and the operator
  is asked. The generic adapter, and so Claude and the configured
  `intercept_patterns` a vendor adapter tries first, make one exception: while
  the answered row is blank — a frame caught between an erase and a repaint that
  may be drawing the question a row higher or lower — the identical line
  elsewhere is taken for the answered question moved. Aider, Goose and Open
  Interpreter make none. They never consulted the memory before, and a clear
  (Ctrl+L at Aider's prompt, `clear`, a test runner) leaves the answered row
  blank for as long as nothing is written on it, while Aider asks for every
  shell command with one and the same line. The exception would silence the next
  command's question, and a question put to nobody blocks the agent without a
  sign. They ask the moved question again instead, as they always did.
- When either side has no row, the line alone decides.

An entry with no row — a question raised on the byte window before the agent
first repainted, or restored from a snapshot — adopts one the first time exactly
one visible row shows it. It is looked for by the line it was asked on first,
and by its match only when that line is not painted: a vendor match is the label
of a kind of prompt, "Run shell command?" for "Run shell commands?", which may
never be painted, and an entry looked for by it was dropped on the first repaint.

The tmux resync compares whole lines, and so does the Codex adapter, whose footer
must end the line so that nothing typed after the question can still match. A
snapshot is another text than the one the live screen rendered, so the live rows
cannot be used to read it: used, they mapped an offset in the snapshot onto
whatever live row sat at that offset, took the pending question for the answered
one's echo, and discarded it.

What is left, knowingly:

- An agent that rejects the answer and asks again on the SAME row, with the
  rejected input still showing after the question, is taken for the echo and is
  not asked. So is the identical question drawn again on the very row the
  answered one occupied, whether the input was erased first or a clear homed the
  cursor there. A re-ask on a new row is asked.
- For the generic adapter and Claude, after a clear, the identical question drawn
  above the answered row, which the clear left blank, is not asked until
  something is written on that row. The whole-line comparison they used before
  lost it too, anywhere on the screen.
- A frame that moves the answered question to another row while painting its old
  row with something else, in one write, asks it again; so does one that moves it
  off a blank row with the echo after it, since the exception wants the identical
  line. For Aider, Goose and Open Interpreter any move off the row asks it again.
- A question whose line changes after it is detected — a countdown, a hint drawn
  after the cursor that the echo overwrites, a spinner glyph at the start of the
  line — no longer begins its row, and is asked again.
- The memory keeps one entry per signature and match, and a vendor match is a
  label shared by every question of its kind. Answering a second Aider question
  of the same kind moves the entry to the second one's row; if the agent then
  erases the second, the first is the last line again, and it is asked again.
- On the tmux resync, the answered question with its echo after it is another
  line, and is asked again.
- A full-screen agent whose answered dialog is still painted near the top of
  its screen, with its cursor in an input box at the bottom, asks it again once
  it repaints for a terminal that lost height. The rows above the cursor go
  first, the dialog's with them, and the alternate screen has no history to
  keep them in. Keeping the top rows, as the screen did before, lost a dialog
  drawn near the bottom instead, beside the cursor.
- An entry with no row adopts the only row showing its question the first time
  it is looked for: at the agent's first repaint, or when a full-screen program
  that was running exits. If that same write also clears the screen and draws
  the identical question on another row, the new question is taken for the
  answered one and is not asked.

A match is not always one line. The vendor rules are regexes that run across a
whole prompt block, so the row check joins as many logical lines as the match
spans; comparing such a match against a single line could only ever answer
false, and would release the memory of a prompt plainly still on screen.

No row is adopted from a line detection would have excluded. Uniqueness is not
candidacy: during a repaint the only row carrying a fragment can be a `log:`
echo of the answer, and an anchor parked there never changes again.

Abandoning a question is not answering it. A snapshot that comes back empty, a
snapshot with nothing detectable on it, a process that exited: nobody decided
anything, so none of them writes into this memory any more. Each used to, which
left an entry describing a screen the caller had just declared stale.

A question the agent takes back is withdrawn. Detection stopping is not enough
once an occurrence is already pending: nothing was comparing that occurrence to
the screen, so a request the agent cancelled by itself stayed on offer, and the
decision was delivered into a terminal that had gone back to its prompt. Every
write now reconciles the pending occurrence with the visible grid, and one that
has left it is withdrawn and reported as withdrawn — the pane unblocks, the
action queue drops it, and the audit records that the gate opened without a
decision. A decision that arrives after the withdrawal is refused rather than
delivered.

The question's TEXT being absent proves nothing, so absence is never the
evidence. It is absent while the agent is halfway through repainting the frame
that will show it again — a full-screen frame is larger than one 4 KiB read, so
the grid is routinely observed between the erase and the redraw. It is absent
when the question merely scrolled out of view while the agent is still waiting.
It is absent when the same characters are still on screen but the grid stopped
joining two wrapped rows into one logical line, which happens whenever an agent
repaints those rows by addressing them. Withdrawing on any of those would stop
asking the operator about a live question and leave the agent waiting forever.

REPLACEMENT is the evidence. An occurrence is withdrawn only when it was seen on
the visible grid under its own identity, when nothing has left the grid since —
the screen counts evictions: scrolling either way, inserting or deleting lines,
switching screens, resizing, so equal counts mean the remembered row still
designates the same place — when that row now carries content that is not blank,
and when that content is neither the question nor a re-serialisation of the line
that carried it. A blank row is a frame in progress, not an answer that stopped
being wanted.

The eviction count is deliberately not `scrolledOff`, which stands still under a
scroll region or an alternate screen because it exists to give a row an absolute
coordinate rather than to report that something left.

### What is still not modelled

The tmux snapshot path reconciles from `capture-pane -p -J`, which joins wrapped
lines and carries no escape sequences: it is text tmux has already rendered, so
it does not pass through this screen and cannot report a burst. Reconciliation
after a native attach therefore still compares whole normalized text.

Resizing cannot be aligned with a byte offset in the stream, so the grid does
not reflow: it keeps what fits and waits for the agent to repaint. What fits is
what a terminal keeps: the cursor's row stays in view, the rows below it go
first, and the rows above it go into the scrollback when that is not enough.
Until the repaint, a wrapped line is wrapped at the old width. The primary
screen parked under a full-screen program is resized once, when the program
exits, to the size then in force.

Withdrawal is narrow on purpose, and everything it cannot prove keeps the
occurrence pending, which is the behaviour that existed before it. An agent that
erases its question and then paints nothing at all keeps it, because a blank row
is indistinguishable from a frame in progress. So does one that takes the
question back with a gesture that moves lines — deleting the line in place,
leaving an alternate screen — because those count as evictions and the
remembered row stops designating anything. And so does an occurrence whose
question scrolled out of view and never came back, since the row can only be
re-anchored by seeing the question again.

A match that spans several rows is only anchored while the grid serialises it as
one logical line. The Claude rules produce such matches.

The legacy `intercept` shim does not observe withdrawals. It keeps its own last
detection, and that value now outlives an occurrence the agent took back.

The adapter ignores several common non-prompt contexts on the active line:

- Markdown quote lines beginning `> `;
- table-like lines beginning `| `;
- fenced code and content while inside a code fence;
- `log:`, `previous:`, and `historique:` prefixes;
- a match enclosed in matching backticks, single quotes, or double quotes.

These rules target practical false positives such as:

```text
> The old output said Overwrite? [Y/n]
`Overwrite? [Y/n]` is the example syntax
log: Overwrite? [Y/n]
```

They are intentionally product-neutral and incomplete. Indentation, alternate
log labels, cursor-addressed interfaces, localization, or adversarial output
can still bypass or trigger the detector. Narrow expressions and a default ask
policy remain necessary.

## ANSI and carriage-return behavior

ANSI stripping is streaming rather than a per-chunk regular expression. An
escape beginning in one read and ending in another is removed as one sequence.
Control bytes are bounded so malformed escape input cannot grow state without
limit.

A read can also end between the `\r` and the `\n` of a line break, or inside
a UTF-8 character; the detection window holds either back for the next read,
so a line drawn with `└` and ended by `\r\n` reads the same a byte at a time
as in one write. A lone carriage return is applied when the next read comes.

Carriage return is modeled as an active-line rewrite. For example, a progress
line overwritten by a prompt can become actionable, while text overwritten by
a later progress line should not survive as a current prompt. Full cursor
addressing and alternate-screen emulation are outside the text viewport's
scope; use tmux attach for such programs.

## Sensitive classification and risk

A generic pattern becomes sensitive when the configured pattern metadata or
matched text looks credential-related. Sensitive matches become `credential`
events with high risk. Other generic confirmations currently carry unknown
risk.

Policy implications:

- credential or sensitive events always ask;
- generic unknown-risk confirmations cannot be automatically allowed;
- a configured deny can be proposed automatically for a valid non-sensitive
  event, but generic encoding does not currently support it;
- unsupported automatic encoding returns control to a human ask.

## Decision encoding

The generic adapter accepts only the internal manual decision form. It rejects
automatic allow/deny and non-actionable event types. Valid manual text is
encoded exactly with a trailing carriage return for the terminal; input
containing a NUL byte is rejected. On the Desktop GUI and the web gateway the
supervision core refuses a typed answer before any adapter sees it unless it is
one line of text of at most 4096 bytes with no control character, and refuses
a typed answer to a prompt that offers Deny alone. A prompt the policy would
deny on an adapter that encodes no deny, the generic one included, offers no
button at all, and there a typed answer is taken and journaled with the reason
`typed_over_policy_deny`.

No front end logs or audits the manual value. Delivery errors keep or restore
human-pending state when it is safe to do so. An uncertain automatic delivery
is not retried.

An adapter that supports automatic decisions must define exact semantic allow
and deny encodings, bind them to a pending occurrence, reject unsupported
event types, and remain safe across live output and snapshot replay.

### Claude Code 2.1.59, 2.1.285 and 2.1.286 (experimental)

The Claude adapter recognizes five prompt structures:

- workspace trust (2.1.59), emitted as a high-risk `permission`;
- whether to use a detected environment API key (2.1.59), emitted as a
  sensitive, high-risk `credential` whose match starts after the displayed key
  value;
- running a shell command (2.1.285 and 2.1.286), emitted as a high-risk
  `permission`. The event's `command` is a best-effort reading of the command
  Claude Code displays, for `command_regex` and `read_only` rules. It is cut out
  of the screen text by rules that fit the layouts seen, so treat it as
  advisory. The decision modal of the desktop application and of the web gateway
  shows it to an operator, and only for this prompt: the generic adapter's
  `command` is a quoted fragment of the question, and is not shown. Each line is
  redacted as the journal redacts values (common credential forms: a flag whose
  argument is the secret, such as `mysql -pSECRET`, a piped password and a bare
  key without a known prefix are not recognised), what follows a masked value is
  kept, the line breaks are kept, and the command is bounded to eight lines of
  200 characters with a mark where it was cut. A bidi override or a zero-width
  character shows as a replacement character. A viewer is sent none, a prompt
  that is a secret shows none, and the TUI does not show it;
- creating a file (2.1.285 and 2.1.286), and editing a file (2.1.285; no 2.1.286
  edit-file layout is recorded), each emitted as a `confirmation` of unknown
  risk. A `text_regex` rule matches the displayed block (file name, content,
  diff); a `path_regex` rule matches only the path-like tokens in it, and misses
  a bare name such as `Makefile` or `.gitignore`. The event metadata carries a
  `target_file` that nothing reads, shows or journals.

A person answers all five. Create and edit are unknown risk, not low, on
purpose: a policy allows a prompt by itself only when it is low risk, and what
Claude Code is asked to write decides what runs next (`.github/workflows`,
`.claude/settings.json`, `.vscode/tasks.json`, a hook, a `package.json` script),
which the guardrails only partly cover and Relayer has no path allowlist for.
The shell-command prompt is high risk. So no policy allows any of the five by
itself: a proposed allow is asked (`risk_not_low`, or `sensitive_event` for the
API-key prompt and for a prompt whose text contains a string the sensitivity
check looks for, such as `token`, `secret` or `password`, even inside a longer
word). A deny rule can still answer a prompt that is
not sensitive, but the adapter has no verified deny byte for Claude Code, so a
proposed deny is handed back to a person (`fallback_unsupported`) and nothing is
written.

Automatic allow and deny are unsupported for all five. Claude Code's menu is
expected to act on whichever choice is highlighted, and no answer to any of
them is backed by a stored capture of its effect. Manual bytes retain generic
compatibility: what a person types in the decision modal is sent as typed,
followed by Enter, provided it is one non-blank line of valid UTF-8, at most
4096 bytes, without control characters. Enter is expected to take the
highlighted option and a number to pick one, but no check of either against a
real Claude Code is recorded, and Relayer does not read which option is
highlighted. Answer at the terminal: a digit typed from
the decision modal is untested, and the Enter sent after it may reach whatever
Claude Code shows next.

The evidence is uneven. The 2.1.59 prompts and the 2.1.285 cases are in
`internal/adapters/testdata/claude/stream_cases.json`; the 2.1.285 cases are
labelled anonymized observations, and the repository records no more about how
they were obtained. The 2.1.286 evidence is strings in `claude_test.go`
(`TestClaudeAdapterReal21286Prompts`): a Bash prompt with three options, a
create-file prompt with two, a Bash prompt with four options in a subtest whose
name contains `real ConPTY artifacts` and whose characters were dropped
(`proeed`, `❯1Yes`), and
unnumbered-menu versions of the two 2.1.59 prompts. No fixture holds the bytes
typed in answer to a prompt, or their effect.

No rule is written for an overwrite of an existing file, a PowerShell command, a
web fetch or an MCP tool, and no capture of one is stored in the repository, so
such a prompt raises an event only if its text happens to match a rule: a
PowerShell prompt carrying a `Run shell command` line would match the
shell-command rule. That rule needs the words `Run shell command` (any case)
before the question, then `Do you want to proceed?`, a numbered Yes and No and
the footer `Esc to cancel` ... `Tab to amend`; a question whose line break falls
inside a word or a file name is missed too. Only a
configured `intercept_patterns` entry can catch the rest, and the patterns a new
configuration file is seeded with matched none of the layouts checked. Without
one the agent waits for an answer nobody was asked for.

### Codex CLI 0.148.0-alpha.21 (experimental)

The Codex adapter recognizes only two captured prompt structures:

- directory trust: human choice `1` maps to the observed default-selection
  carriage return (`0d`), while deny is the selection-independent `2` (`32`);
- command approval: allow is `y` (`79`), deny is Escape (`1b`).

Those four byte sequences were verified against disposable local sessions,
but automatic directory-trust allow remains unsupported because carriage
return depends on the current highlighted choice.
The event match is a constant question and never contains the displayed path
or command. Command approval carries unknown risk and directory trust high
risk, so the policy engine still refuses automatic allow; a verified,
non-sensitive deny may be automatic. No file-write, network, credential, MCP,
review, or other Codex prompt is claimed.

### Goose 1.52.0 (experimental)

Goose asks before a tool call, in approve mode, with a cliclack menu:
"Goose would like to call the above tool, do you allow?" offers Allow, Always
Allow, Deny and Cancel; "Do you allow this tool call?", drawn when a check
attached a notice to the call (extension management always does, a prompt
injection finding does), offers Allow, Deny and Cancel. Both are a high-risk
`permission`. The highlight starts on Allow, moves with `j` and `k`, stops at
either end, and Enter picks it; `y` and `n` do nothing. So an answer first
moves to the top, whatever the highlight: allow is `k` once per option below
the first, then Enter (`6b6b6b0d` and `6b6b0d`), deny the same then `j` down to
Deny (`6b6b6b6a6a0d` and `6b6b6a0d`). Manual input names `allow`, `deny` or
`cancel`; Always Allow, which changes Goose's permissions for every later call,
and free text, whose letters would move the highlight, are refused. The menu is
read only complete, from the question after the active symbol (`◆`, or `*` in
ASCII) through each option to the bar's end, as the last thing on the screen;
answered, Goose redraws the question after the submitted symbol, which is not
read as asked. No other Goose question, such as those of `goose configure`, is
claimed.

## MCP tool calls

`internal/adapters/mcp.go` reads a prompt block that detection has already
raised and reports whether it describes a Model Context Protocol tool call.
`DetectToolCall` returns the server, the tool, a bounded argument list and a
risk level.

The processor attaches the result to the occurrence it raised, and the web
interface renders it as a badge beside the arbitration prompt: the server, the
tool, its risk, and the arguments that were readable. An operator can therefore
see what a tool is about to be given before answering.

No policy consults it. Risk on the badge is informational; the policy engine
still evaluates the occurrence exactly as it would without one, so a tool call
never becomes auto-approvable because its badge says low.

The badge is suppressed on a confidential occurrence. A credential prompt's
surrounding text is precisely what must not be reprinted, and a badge built
from it would undo the masking sitting next to it.

This is a separate capability from the vendor adapters above, and it does not
contradict them. The Claude and Codex sections say that no MCP prompt is
claimed: that statement is about the captured fixtures, which contain no such
prompt, and it remains true. The parser here claims nothing about any
particular agent's MCP prompt layout.

Recognition anchors on the `mcp__<server>__<tool>` naming convention rather than
on the frame, wording, or choice list an agent draws around it. That convention
is a client convention, not part of the protocol: it is how Claude Code names an
MCP tool when it exposes one to a model, and no check was made of what any other
client does. A client that names tools differently is simply not recognized.
Anchoring on the name rather than the layout was still the narrower choice,
because the layout differs per agent and changes between versions and no
captured output exists to match one against.

Parameters are best effort. They are extracted from the same normalized text,
bounded in count and in length, and sanitised. A parameter value is terminal
text the agent produced: it is not authenticated, it is not necessarily what the
tool will actually receive, and it must never be treated as an instruction or as
evidence about the call. Read it the way you would read any other line of agent
output.

Risk is classified from the tool name alone and never from an argument value.
An argument is agent-controlled, so classifying on one would let the agent talk
a dangerous call down: a delete or a shell tool carrying a reassuring-looking
path, flag, or comment would read as low risk precisely when the operator most
needs the opposite.

Keeping that promise takes more than not reading the argument field, because an
argument can carry a second name. A block is therefore resolved to the most
dangerous name it carries, and among equals to the last one. Taking the last
name alone was enough to lose the promise: in
`mcp__fs__delete_file path=mcp__docs__get_page` the decoy sits after the real
name, so a plain last-name rule reported a page fetch. A second name can now
only raise the risk of a block, never lower it, at the cost of reporting the
wrong name upward when a block genuinely carries two calls.

The keyword lists are matched asymmetrically for the same reason. A high-risk
keyword matches as a substring, so the classifier over-reports danger. A
low-risk keyword has to match a whole token, because as a substring it
under-reports: `spreadsheet` carries `read` and `forget_session` carries `get`,
and either would have turned an unknown tool into a low-risk one.

The [audit journal](audit.md) records the tool's identity and nothing else. A
recognized call adds `mcp_server`, `mcp_tool` and `mcp_params` — a count — to
the occurrence and to the decision an operator then took, so the journal can
answer which tool somebody approved. Argument values never reach it: they are
agent-controlled content, and the audit model has no field for content. The
allowlist in `internal/audit` enforces that independently, so a value added at
the emitting end without a matching rule there is dropped rather than written.

This is parsing only. It does not intercept, sandbox, proxy, or block a tool
call, and it changes no policy decision. An agent can also call a tool without
ever printing anything Relayer can see, in which case there is nothing to read —
reading no call is not evidence that no tool ran.

The parser is heuristic and is not fixture-backed. Its patterns were written by
hand, not captured from a recorded session, so a real call can go unread, and
output that merely mentions a tool name — prose, a README printed by `cat`, a
diff line, a log line, a commit message — is read as a call. Nothing in the
parser distinguishes a name being used from a name being discussed.

## Fixtures and test policy

`internal/adapters/testdata/generic` contains synthetic product-neutral stream
and snapshot cases. Useful cases cover:

- matches split across chunks;
- ANSI sequences split across chunks;
- active-line carriage-return rewrites;
- quoted examples, fenced code, tables, and old-log prefixes;
- same snapshot replay, acknowledgement, and a new occurrence;
- successive identical prompts;
- bounded detection and output state;
- decision bytes and unsupported decisions.

The `claude`, `codex`, `aider`, `interpreter` and `goose` fixture directories
contain minimal anonymized observations plus provenance notes. `relayer-capture`
records output only, so the Aider, Open Interpreter and Goose questions were
captured by a PTY harness that also typed each answer, against a local
stand-in model, and the effect of every answer was checked; each directory's
README says how. They intentionally exclude account data,
personal paths, repository content, commands from real projects, credentials,
hostnames, and unrelated output.

Use the output-only capture utility for new evidence:

```sh
go run ./cmd/relayer-capture --tool example-cli --adapter generic \
  --backend pty --output /tmp/example-fixture.json -- example-cli
go run ./cmd/relayer-capture --validate /tmp/example-fixture.json
```

The same command accepts `--backend tmux`. It uses a private tmux socket,
never invokes an implicit shell, and has no stdin, environment-map, or
credential field. An explicitly selected shell remains an ordinary executable
with all of that shell's effects. Captures are bounded, redacted before
persistence, and fail closed on secret-shaped content. See
[fixture capture](fixture-capture.md).

Do not contribute real transcripts without explicit authorization and a
provenance/anonymization review. Remove credentials, account and repository
identifiers, personal paths, source code, task content, timestamps, hostnames,
and unique wording. Prefer a minimal synthetic generic fixture whenever it can
exercise the same parser contract.

## Adding an adapter

During alpha, adapters are compiled into Relayer. A contribution should include:

1. a unique lowercase ID and accurate maturity descriptor;
2. stateful detection that handles arbitrary chunking and snapshot replay;
3. occurrence identity and acknowledgement tests;
4. explicit sensitive and risk classification;
5. decision encoding tests, including unsupported actions;
6. memory bounds and malformed-input tests;
7. authorized anonymized fixtures and documented provenance;
8. TUI/backend integration tests for delivery, exit, resync, and failure;
9. configuration and user documentation without overstating vendor support.

Registering an executable hint is appropriate only after the adapter is
implemented. A placeholder must never intercept executable resolution merely
because its name exists.
