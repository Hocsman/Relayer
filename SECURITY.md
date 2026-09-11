# Security policy

Relayer is a local supervision tool, not a security boundary. It launches
configured programs with the current user's authority and observes terminal
output after those programs have started. Read the complete
[security model](docs/security-model.md) before relying on it.

## Supported versions

Relayer v0.3.0 is the current General Availability (GA) release. Security fixes
are published for the latest supported release and the `main` branch. Official
releases, signatures, and SBOMs are published on GitHub Releases.

## Reporting a vulnerability

Prefer a private GitHub security advisory:

[Report a vulnerability privately](https://github.com/Hocsman/Relayer/security/advisories/new)

Include, when safe:

- the affected commit and platform;
- the backend and configuration shape, with secrets removed;
- a minimal synthetic reproduction;
- the expected and observed security property;
- likely impact and whether exploitation was demonstrated.

Do not include credentials, tokens, private source code, real agent transcripts,
raw audit logs, or personal paths. If private advisories are unavailable, do
not open a public issue containing sensitive material. Contact the maintainer
through their GitHub profile to arrange a private channel.

After a report is received, the maintainer will assess it on a best-effort
basis. Publication, release, and credit timing must be coordinated; there is no
guaranteed embargo or bounty program.

## Useful security reports

Examples include:

- sending a decision to the wrong session or prompt occurrence;
- bypassing sensitive-event or audit failure gates;
- exposing manual input, environment values, raw output, or credentials in
  logs, audit entries, arguments, temporary files, or filenames;
- killing an unowned tmux session;
- unsafe file permissions, symlink handling, ownership checks, or rotation;
- command/argument confusion between direct and explicit shell execution;
- unbounded attacker-controlled memory, goroutine, file, or process growth;
- shutdown or rollback behavior that leaves unintended processes running.

Prompt false positives and false negatives are relevant when they demonstrate a
systematic bypass or cross-session decision. A single new CLI wording is usually
an adapter improvement rather than a vulnerability, but report privately if
the transcript itself is sensitive.

## Security boundaries and non-goals

Relayer does not:

- sandbox or restrict an agent process;
- mediate file, network, credential, tool, or operating-system access;
- guarantee that a CLI asks before acting;
- authenticate terminal output or prove that a displayed prompt is genuine;
- provide complete terminal emulation;
- enforce policy while a human is directly attached through tmux;
- protect data from the same operating-system user, an administrator, root,
  malware, backups, disk snapshots, or process inspection;
- make its local JSONL audit tamper-evident or cryptographically signed;
- guarantee redaction of unknown secret formats;
- native Windows is supported via ConPTY (tmux backend is Unix-only).

The detailed threat model, controls, residual risks, and safer operating
practices are in [docs/security-model.md](docs/security-model.md). Audit-specific
privacy limits are in [docs/audit.md](docs/audit.md).
