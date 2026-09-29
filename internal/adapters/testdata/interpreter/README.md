# Open Interpreter adapter fixtures

These fixtures were observed from `open-interpreter 0.4.3`, installed from
PyPI, run in a 100x30 pseudo-terminal in an empty working directory under the
system temporary directory, with a private `HOME`. Open Interpreter was pointed
at a local stand-in for an OpenAI-compatible model (`--model
openai/<stand-in> --api_base http://127.0.0.1:<port>/v1 --api_key <dummy>`)
that always replied with the same shell block, `printf verified-content >
verified.txt`; no real model, account or key was involved.

Every question was answered both ways, and the side effect checked:

| Fixture | Question | `y` + Enter | `n` + Enter |
| --- | --- | --- | --- |
| `run_code` | Would you like to run this code? | the block ran: `verified.txt` was created | it did not; back at the `>` prompt |
| `scan_code` | Would you like to scan this code? (`--safe_mode ask`) | "Scanning code..." and a semgrep run, then `run_code` | no scan, then `run_code` |

Open Interpreter prints the question indented two spaces, then `\n\n  `, and
reads the answer as a line there; with `--plain` it prints the run question
unindented, followed by `\n\n`. It asked nothing else in these runs, and its
`terminal_interface.py` asks nothing else before code runs: no question before
installing a package or saving a file, and a shell command is a code block like
any other. Its other `(y/n)` questions, about migrating a profile or sharing
conversations, do not gate code. The patterns written earlier from documentation
named questions this version does not ask; anything else it prints falls back
to the configured `intercept_patterns`.

`ansi_chunks` are the raw bytes Open Interpreter wrote for the last frame of
the model's reply and the question, up to the moment it waited for the answer,
cut in three, inside an escape sequence where one was near the cut. `stripped`
is the same text with the escapes removed, for reading. The excerpts hold no
path, host, account or secret.
