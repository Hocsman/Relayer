# Contributing to Relayer

Relayer is alpha software. Contributions are welcome, but changes to process
ownership, terminal input, prompt classification, automatic decisions, audit
records, or cleanup can have security consequences. Keep changes small,
testable, and explicit about their failure behavior.

## Before opening a change

- Use a public issue or discussion for feature design when no sensitive detail
  is involved.
- Use the private process in [SECURITY.md](SECURITY.md) for vulnerabilities,
  credentials, private terminal output, or exploitable transcripts.
- Check [CHANGELOG.md](CHANGELOG.md) and the current tests before assuming an
  alpha API is stable.

## Development setup

Required:

- Go 1.25.8 or newer;
- a supported Linux or macOS development environment;
- Bash for mock-agent integration tests.

Optional:

- tmux for tmux backend and attach integration tests;
- [VHS](https://github.com/charmbracelet/vhs) to run
  [`docs/demo.tape`](docs/demo.tape).

Build the canonical entry point:

```bash
go mod download
go build ./cmd/relayer
```

Run the standard checks before proposing a change:

```bash
gofmt -w ./path/to/changed.go
go test -race ./...
go vet ./...
go build ./...
git diff --check
```

Some Unix and tmux integration tests may skip when their platform dependency is
not available. A skip is not evidence that the skipped behavior works on that
machine.

## Change guidelines

### Process and backend changes

- Preserve transactional startup: validation and audit initialization happen
  before agents start, and a partial start must be rolled back.
- Keep shutdown bounded, concurrency-safe, and idempotent or retryable where
  its API promises that behavior.
- Never kill a tmux server. Kill a session only after verifying Relayer's
  immutable ownership marker.
- Do not put terminal input or secrets in command arguments, diagnostics,
  snapshots, audit entries, or temporary filenames.
- Keep direct PTY behavior working when tmux is absent.

### Adapter and prompt changes

- Treat terminal output as untrusted input.
- Preserve streaming behavior across arbitrary chunk boundaries, fragmented
  ANSI escapes, carriage-return rewrites, snapshots, and replay.
- Add a regression test for both the intended detection and likely false
  positives, including quoted text, fenced code, tables, and old log lines.
- Keep windows and retained output bounded.
- Do not claim support for a vendor interaction until its parser and supported
  decision encodings are backed by authorized anonymized fixtures and tests.
  The experimental `claude` and `codex` adapters cover only the exact
  interactions documented in `docs/adapters.md`; their names are not a claim
  of complete tool support.

### Fixtures

Generic fixtures must be synthetic and product-neutral. Do not copy terminal
transcripts, prompts, repository paths, account identifiers, source code,
tokens, or other confidential data from a real AI CLI session.

Vendor-specific fixtures require explicit authorization, aggressive
anonymization, and a documented provenance review. The current
`internal/adapters/testdata/claude` and `internal/adapters/testdata/codex`
directories contain only the minimal reviewed observations and provenance
notes for the interactions implemented today. Never expand those claims with
invented “realistic” output.

### Configuration and audit changes

- Keep versioned YAML strict. Unknown fields, aliases, merge keys, multiple
  documents, and incorrect scalar types must fail before backend construction.
- Document defaults and compatibility behavior alongside code changes.
- Audit schemas are append-only contracts even during alpha: use a schema
  version and safe closed values instead of silently changing meanings.
- Never add raw terminal output, manual input, environment values, commands,
  shell text, event matches, or raw errors to the audit model.
- Treat redaction as defense in depth and prefer closed metadata allowlists.

### Documentation changes

State only behavior established by current code and tests. Do not add release,
platform, vendor, security, billing, or performance claims without evidence.
Update related pages when an alpha contract changes.

## Tests

Prefer focused table-driven tests for pure validation and policy logic, fakes
for lifecycle and routing contracts, and bounded subprocess tests for PTY or
helper execution. Tests involving concurrency should have deterministic gates
and deadlines rather than sleeps whenever possible.

At minimum, changes should retain coverage for:

- prompt detection across chunks and snapshots;
- ANSI sanitization and carriage-return rewrites;
- event occurrence deduplication and acknowledgement;
- ring-buffer bounds;
- PTY resize, input, exit, and cleanup;
- tmux ownership, private transport, attach/resync, persistence, and cleanup;
- policy conservative fallbacks and sensitive input handling;
- audit redaction, rotation, permissions, write failures, and concurrent close;
- TUI focus, paging, scrolling, prompt queues, delivery, and shutdown.

## Pull requests

A useful pull request includes:

- the user-visible problem and the intended behavior;
- the security and compatibility impact;
- tests that fail before and pass after the change;
- documentation and changelog updates when behavior changes;
- confirmation that no real credentials or private transcripts are included.

Maintainers may ask to split broad changes. Because the project is alpha,
review may also require revising an internal API instead of preserving it.

By contributing, you agree that your contribution is provided under the
project's [MIT License](LICENSE).
