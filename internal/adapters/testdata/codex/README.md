# Codex adapter fixtures

These fixtures were observed on macOS with the bundled local executable
`/Applications/ChatGPT.app/Contents/Resources/codex`, reporting
`codex-cli 0.148.0-alpha.21`. Captures used interactive inline mode with
`--no-alt-screen`, `--ask-for-approval untrusted`, and
`--sandbox workspace-write` in disposable Git repositories under the system
temporary directory.

Only two interactions are evidence-backed here:

- command approval: `y` (`79` hex) executed the harmless captured command;
  `ESC` (`1b` hex) canceled it and the target file remained absent;
- directory trust: Enter (`0d` hex) continued; `2` (`32` hex) quit before a
  model request.

Directory-trust Enter is exposed only for the explicit human choice `1`.
Automatic allow is deliberately unsupported because Enter depends on the
current highlighted TUI choice. The selection-independent deny byte remains
available.

The stored transcripts are minimal normalized excerpts derived from the real
terminal output. A disposable path is replaced by `[fixture-directory]`.
Account data, session identifiers, hostnames, personal paths, usage details,
tokens, emails, secrets, and unrelated terminal output are excluded. The
harmless fixture commands contain only fixed public test strings.

This is deliberately experimental, version-specific evidence. It does not
claim support for file-write, network, authentication, credential, MCP, image,
review, or other Codex prompts. Sensitive-prompt coverage in tests comes only
from the existing configured `intercept_patterns` fallback.
