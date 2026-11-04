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

### Known limits

- The state directory is protected by filesystem permissions, not encryption. Anyone who is already that user on that host can read session metadata.
- The remote TCP transport for a future daemon is disabled by default until authentication and TLS exist (see `docs/EVENTS.md`).
- The tool does not "phone home". There is no telemetry and no network access other than the MySQL connection you configure.

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

In scope: credential handling, redaction, permission handling, session/state integrity, any path that could execute an unintended DDL statement.

Out of scope: the behaviour of MySQL itself, `gh-ost` or `pt-online-schema-change` binaries you installed separately, and anything requiring an already-compromised host.
