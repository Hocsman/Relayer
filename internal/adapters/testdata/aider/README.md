# Aider adapter fixtures

These fixtures were observed from `aider 0.86.2`, installed from PyPI, run in a
100x30 pseudo-terminal in disposable Git repositories under the system
temporary directory. Aider was pointed at a local stand-in for an
OpenAI-compatible model (`--model openai/<stand-in> --openai-api-base
http://127.0.0.1:<port>/v1`) that always gave the same reply, so each question
is the one Aider asks for that reply; no real model, account or key was
involved. Other flags: `--edit-format diff --no-check-update
--analytics-disable --no-show-model-warnings --no-show-release-notes
--no-auto-commits`, and `--no-gitignore` except for `git_ignore.json`.

Every question was answered both ways, and the side effect checked:

| Fixture | Question | `y` + Enter | `n` + Enter |
| --- | --- | --- | --- |
| `run_command` | Run shell command? | the command ran | it did not |
| `create_file` | Create new file? | the file was created | it was not |
| `add_to_chat` | Add file to the chat? | added, and the edit applied | Aider asked `allow_edits` |
| `allow_edits` | Allow edits to file that has not been added to the chat? | the file was edited | it was not |
| `add_command_output` | Add command output to the chat? | "Added 2 lines of output to the chat." | nothing added |
| `git_ignore` | Add .aider* to .gitignore (recommended)? | `.gitignore` created | it was not |

`ansi_chunks` are the raw bytes Aider wrote from the end of the model's reply
to the moment it waited for the answer, cut in three, once inside an escape
sequence where one was near the cut. `stripped` is the same text with the
escapes removed, for reading. The excerpts hold no path, host, account or
secret: the stand-in's reply named only `verified.txt`, `new_note.txt`,
`README.md` and fixed strings.

Every question comes from Aider's one confirmation function,
`io.confirm_ask`, which appends ` (Y)es/(N)o`, the optional `/(A)ll`,
`/(S)kip all` or `/(D)on't ask again`, and ` [Yes]: ` or ` [No]: `, and reads a
line. Aider asks other questions through it (fixing lint or test errors,
installing a package, trying a URL); they are not captured here and fall back
to the configured `intercept_patterns`.
