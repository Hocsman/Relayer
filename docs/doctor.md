# Doctor and preflight

Relayer exposes one read-only readiness engine through both the terminal CLI
and the desktop GUI.

## CLI usage

Build Relayer, then inspect an existing configuration:

```bash
go build -o relayer ./cmd/relayer
./relayer doctor --config config.yaml
```

The report contains three possible outcomes:

| Outcome | Exit status | Meaning |
| --- | ---: | --- |
| `PRÊT` | 0 | Every check passed. |
| `AVERTISSEMENTS` | 0 | Startup is possible, with conservative limitations to review. |
| `BLOQUÉ` | 1 | At least one condition prevents a ready startup. |

The GUI presents the same report from **Santé** or **Vérifier
l’installation**.

## Checks

The versioned report covers:

- strict configuration shape and effective one-to-eight-agent plan;
- policy compilation and references to effective agents;
- macOS/Linux execution support and the explicit Windows configuration-only
  boundary;
- audit configuration plus passive type, ownership, and permission checks for
  the active journal and every recognized existing rotation;
- passive `PATH` discovery for fixed catalogue tools and configured agent
  executables;
- effective stable or experimental adapter selection;
- effective PTY/tmux selection, including visible `auto` fallback.

An empty `agents: []` configuration is inspected as the same two Bash demo
agents used by normal startup. An explicit `adapter: generic` remains generic
even when an executable name could select an experimental vendor adapter.

## Read-only and privacy boundary

Doctor uses `config.LoadExisting`: an absent file is reported and is never
created. It never opens an audit sink, creates a PTY or tmux manager, starts a
session, runs a provider command, or invokes `--version`.

Reports use only fixed catalogue identifiers and one-based agent ordinals.
They never contain:

- the selected configuration or audit path;
- command arguments, shell bodies, or environment names and values;
- configured agent IDs or names;
- resolved executable paths;
- parser, filesystem, detector, backend, or other raw errors.

Expected failures are converted to closed check identifiers, summaries, and
remediation text. The GUI additionally replaces an unexpected bridge failure
with a fixed message.

## Limits

Passive executable discovery proves only that the current Relayer process can
resolve a candidate through its `PATH` or configured command path. It does not
prove binary identity, version compatibility, authentication, provider access,
model availability, terminal protocol compatibility, or safe behavior.

Audit path inspection does not create a probe file or promise future writes;
the audit recorder repeats its authoritative checks during startup. Likewise,
all configuration, policy, adapter, backend, and audit initialization remains
authoritative at normal startup even after a successful doctor run.
