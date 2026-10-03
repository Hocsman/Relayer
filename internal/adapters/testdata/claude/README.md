# Claude Code adapter fixtures

`stream_cases.json` contains bounded excerpts of five prompts, each labelled
with the Claude Code version it comes from.

## Claude Code 2.1.59

Two cases, anonymized excerpts observed through a PTY with Claude Code 2.1.59 on
macOS:

- the initial workspace trust question;
- the question asking whether a detected environment API key should be used.

The workspace path is replaced by `<WORKSPACE>`. The environment value is
replaced by the constant `<REDACTED>` without preserving its value or length.
The excerpts contain no account identifier, email address, hostname, personal
path, project name, token value, key value, or terminal output unrelated to the
prompt.

The ANSI cursor-forward sequences are retained because Claude Code uses them to
render spaces. Other repaint traffic was omitted. A model-backed attempt was not
used as evidence because the locally configured OAuth session had expired.

The platform, the omitted traffic and the OAuth note are what was written when
the two cases were added. Nothing else in the repository records them.

## Claude Code 2.1.285

Three cases labelled anonymized PTY observations from Claude Code 2.1.285:

- running a shell command (`claude-2.1.285-bash-command`), high-risk
  `permission`;
- creating a file (`claude-2.1.285-write-file`), `confirmation` of unknown risk;
- editing a file (`claude-2.1.285-edit-file`), `confirmation` of unknown risk.

How they were obtained is not recorded beyond that label. `claude.go` calls the
Bash case an "earlier synthetic fixture", and the commit that added the three
calls the prompts real, so treat their provenance as unconfirmed. The file name
in the last two is `test_claude_probe.txt`.

The Bash case prints the command before the `Run shell command` line. The two
Bash strings in `claude_test.go` described below print it after, the adapter
reads both, and `claude.go` says real Claude Code prints it after from 2.1.286.
This README cannot tell a layout change from a synthetic fixture.

The edit-file prompt has only this case: no 2.1.286 edit-file prompt is
recorded.

## Claude Code 2.1.286

No fixture. `TestClaudeAdapterReal21286Prompts` in `claude_test.go` holds five
strings: a Bash prompt with three options (named 2.1.286), a create-file prompt
with two, a Bash prompt with four options in a subtest named `real ConPTY
artifacts` that has dropped characters (`proeed`, `❯1Yes`), and unnumbered-menu
versions of the two 2.1.59 prompts. How they were produced is not recorded. They
show that the adapter detects those strings and extracts their command or file
name, and nothing else.

## What none of them prove

No fixture file stores the bytes typed in answer to a prompt, or what Claude
Code did with them, so no allow or deny encoding is claimed for any prompt.
Enter is expected to take whichever choice is highlighted, which Relayer does
not read, and a digit that the menu does not offer is not a choice. A person
answers every prompt: what they type in the decision modal is sent as typed
followed by Enter, provided it is one non-blank line without control
characters.

No network, MCP, PowerShell, overwrite-of-an-existing-file, sign-in, or other
tool permission prompt is claimed.

To add a case, capture the prompt as described in `docs/fixture-capture.md`,
which records output only. Answered bytes and their observed effect are stored
as `captureFixture` in `vendor_capture_test.go` defines them (`allow_input_hex`,
`deny_input_hex`, `observed_allow`, `observed_deny`), and only then is an allow
or deny claimed.
