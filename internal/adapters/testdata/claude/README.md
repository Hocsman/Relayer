# Claude Code adapter fixtures

`stream_cases.json` contains bounded, anonymized excerpts observed through a
PTY with Claude Code 2.1.59 on macOS. They prove only these two prompts:

- the initial workspace trust question;
- the question asking whether a detected environment API key should be used.

The workspace path is replaced by `<WORKSPACE>`. The environment value is
replaced by the constant `<REDACTED>` without preserving its value or length.
The excerpts contain no account identifier, email address, hostname, personal
path, project name, token value, key value, or terminal output unrelated to the
prompt.

The ANSI cursor-forward sequences are retained because Claude Code uses them
to render spaces. Other repaint traffic was omitted after verifying that it
does not change the prompt text consumed by the adapter.

No Bash, file-edit, network, MCP, authentication, or other tool permission
prompt is claimed by these fixtures. A model-backed attempt was not used as
evidence because the locally configured OAuth session had expired. Automatic
allow and deny encoding also remain unsupported: the observed byte for Enter
depends on whichever TUI choice is currently highlighted.
