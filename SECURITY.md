# Security Policy

## Threat model

Saksama Core mutates database schema. A bug here can cause downtime or data loss, so the security posture is deliberately conservative.

### What the tool protects against

| Risk | Mitigation |
|---|---|
| Credentials leaking through `ps` | Passwords are never accepted in argv; option file, environment, or TTY prompt only |
| Credentials leaking through logs | Every emitted event passes through a redaction layer that removes sensitive keys and masks inline DSN passwords |
| Credential file readable by others | Refused with an error and the exact `chmod` command, not a warning |
| State directory readable by others | Created and re-asserted as `0700`; session files and event logs are `0600` |
| Two migrations rebuilding the same table | Per-table lock in the state directory; a second session is refused and names the owner |
| Secret persisted to disk | Session files and audit exports contain no password at any point |
| SQL injection through identifiers | Identifiers are tokenised and re-quoted; multi-statement input is rejected, not split |
| A tampered engine binary | `engine install` refuses to write anything it cannot verify: gh-ost against the release's published `SHA256SUMS`, the pt-osc single file against a digest pinned in this build |
| An engine binary that was replaced after the fact | Every install asserts the version the new file reports from its own `--version`, and the managed directory is probed before `PATH` |

## Network access

This used to say the tool never touched the network except for your MySQL
connection. That stopped being true when `saksama engine install` was added, and a
security document that describes the previous version is worse than no document.

| Command | Network | What it does |
|---|---|---|
| `advise`, `plan`, `apply`, `status`, `attach`, `cancel`, `cutover`, `reconcile`, `cleanup`, `events`, `export`, `schema`, `compat` | none | the MySQL connection you configured, and nothing else |
| `engine doctor` | none | reads local binaries and the server, runs `<engine> --version` locally |
| `engine install` | GitHub, HTTPS | the only command that downloads anything |

There is no telemetry, no update check, and no call home. Nothing installs itself:
`plan`, `advise`, and `apply` never download, and the wizard only prints the
command.

### What `engine install` trusts

- **HTTPS to the publisher.** `github.com/github/gh-ost` releases, and
  `raw.githubusercontent.com` for the Percona single file. There is no pinning of
  the TLS certificate; the chain is the operating system's.
- **gh-ost: the release's own `SHA256SUMS`.** Fetched over the same transport, so it
  defends against a corrupted or truncated download and against the wrong asset, not
  against a compromised GitHub account or release.
- **pt-osc: a digest pinned in this repository.** Percona publishes no checksum for
  the single Perl file, so `internal/enginesetup` pins the sha256 of the v3.7.2 file.
  A silent change upstream fails the install instead of being trusted. That digest is
  a statement about one exact file, and it is updated deliberately when the verified
  version moves.
- **The version, from the binary itself.** The installed file must report the version
  the manifest names, or it is deleted and the install fails.
- **No privilege escalation.** The default target is `~/.local/share/saksama/bin`,
  which needs no root. `--system` writes to `/usr/local/bin` and requires `--yes`.
- **Nothing is overwritten silently.** An existing file stops the install unless
  `--force` is passed.

The residual risk is worth stating plainly: a compromised gh-ost release, or a
compromised GitHub account able to rewrite the pinned Percona tag, would be
installed. The mitigation is that version and digest must both change before that
happens, and the change is visible in the diff.

### Known limits

- The state directory is protected by filesystem permissions, not encryption. Anyone who is already that user on that host can read session metadata.
- The remote TCP transport for a future daemon is disabled by default until authentication and TLS exist (see the event contract in `internal/events`).
- `engine install` verifies what it downloaded; it does not sandbox what it runs. The copy engines are executed with your MySQL credentials and your filesystem permissions, which is why the version is pinned to one this repository has tested.

## Reporting a vulnerability

**Do not open a public issue for a security problem.**

Use GitHub's private vulnerability reporting on
`https://github.com/perogeremmer/saksama-core/security/advisories/new`.

Please include:

- affected version (`saksama version`) and platform,
- MySQL version and vendor,
- a reproduction, ideally a `SHOW CREATE TABLE` plus the ALTER,
- what an attacker gains.

### What to expect

- Acknowledgement within 7 days.
- A coordinated disclosure window of 90 days, shortened if a fix ships sooner.
- Credit in the release notes unless you ask otherwise.

## Scope

In scope: credential handling, redaction, permission handling, session/state integrity, the engine download path (verification, target directory, privilege), and any path that could execute an unintended DDL statement.

Out of scope: the behaviour of MySQL itself, `gh-ost` or `pt-online-schema-change` binaries you installed separately, and anything requiring an already-compromised host.
