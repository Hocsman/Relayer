# Goose adapter fixtures

These fixtures were observed from `goose 1.52.0`, the `goose-cli` crate built
from the `v1.52.0` tag, run as `goose session` in a 100x30 pseudo-terminal in
an empty working directory under the system temporary directory, with a
private `HOME`. Goose ran in approve mode (`GOOSE_MODE=approve`) with its
`openai` provider pointed at a local stand-in for an OpenAI-compatible model
(`OPENAI_HOST=http://127.0.0.1:<port>`, a dummy key) that always replied with
the same tool call; no real model, account or key was involved.

Every menu was answered both ways, with the bytes the adapter sends, and the
side effect checked; the stand-in logged the tool result Goose returned:

| Fixture | Question | Allow | Deny |
| --- | --- | --- | --- |
| `tool_call` | Goose would like to call the above tool, do you allow? (a `shell` call) | `kkk` Enter: the command ran, `verified.txt` was created | `kkkjj` Enter: "The user has declined to run this tool", no file |
| `tool_call_with_notice` | Do you allow this tool call? (an `extensionmanager__manage_extensions` call, which carries a notice) | `kk` Enter: "The extension 'apps' has been disabled successfully" | `kkj` Enter: "The user has declined to run this tool" |

The files ending in `--ascii` are the same menus with `LANG` unset, where
cliclack draws `*`, `|`, `>` and `—` in place of `◆`, `│`, `●` and `└`; the
answers had the same effects. The menu does not wrap: with the highlight moved
to Cancel first (`jjj`), `kkk` Enter still allowed. `n` and Enter, which the
adapter sent for a deny before these captures, allowed the call.

The menus come from `prompt_tool_confirmation` in
`crates/goose-cli/src/session/mod.rs`: those two are the only questions Goose
asks before a tool call.

`ansi_chunks` are the raw bytes Goose wrote from the rule above the tool call
to the moment it waited for the answer, cut in three, inside an escape
sequence where one was near the cut. `answered_chunks` are the bytes it wrote
after the allow answer, up to its next input prompt. `stripped` is the
question's text with the escapes removed, for reading. The excerpts hold no
path, host, account or secret: the stand-in's calls named only
`verified.txt` and the bundled `apps` extension.
