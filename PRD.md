# Product Requirements Document: Saksama Core

> **Product:** Saksama Core, a MySQL online schema migration router and supervisor
> **Binary:** `saksama` | **Domain:** `saksama.dev` | **License:** MIT
> **Status:** draft, v0.1
> **Date:** 2026-11-04
> **Release target:** MVP, v1
> **Related:** [`DESIGN.md`](DESIGN.md), [`docs/CHECKS.md`](docs/CHECKS.md), [`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md)

---

## 1. Executive summary

Saksama Core is an open source tool for planning and running MySQL schema changes safely, for small teams that have exactly one database instance, no DBA, and no ability to run a full blue-green deployment.

It is not a new copy engine and not a thin wrapper around gh-ost or pt-online-schema-change. Its value is:

1. **Analyzing the ALTER before it runs**, with risks stated in actionable terms.
2. **Choosing the cheapest and safest execution path**, especially native MySQL DDL when the capability is verified.
3. **Refusing operations that are not understood or not recoverable with certainty.**
4. **Supervising the migration as a durable session**, so a dropped SSH connection does not stop it.
5. **Providing a verification gate before a cutover** for gh-ost migrations.
6. **Stating the limits of recovery honestly**, in particular that the old table is not a replica and that rolling back data after a cutover is not automatically safe.

Product principle:

> **Refuse aggressively, explain precisely, automate only what has deterministic recovery.**

---

## 2. Background

### 2.1 The user's problem

Small teams run MySQL on one VPS, on RDS single-AZ, or on a managed instance without a replica. They still need to run:

- `ADD INDEX` on a large table,
- `ADD COLUMN`,
- `DROP INDEX`,
- a column type or structure change,
- a schema change on a table with tens of millions of rows.

Today they choose between:

- running `ALTER TABLE` directly without understanding the algorithm and lock behaviour,
- using gh-ost or pt-online-schema-change with complex options,
- postponing the migration out of fear of downtime,
- writing their own wrapper script, which tends to lose foreign key actions, leak credentials, lose state, or sit in a metadata lock timeout for hours.

The missing piece is not a copy engine. It is a layer that:

- understands the schema and the server,
- classifies the ALTER,
- chooses an engine,
- explains the risk,
- enforces guardrails,
- persists state,
- supervises execution and recovery.

### 2.2 Why existing solutions are not enough

| Solution | Strength | Gap for this user |
|---|---|---|
| Native MySQL DDL | fast, and often no table copy | hard to determine capability, still carries MDL risk, and the fallback algorithm can surprise you |
| gh-ost | one binary, throttling, cutover pause | no foreign key support, complex options, and it is not an analyzer |
| pt-online-schema-change | mature, handles more legacy schemas | Perl dependency, GPL-2, noisy output, no cutover pause |
| Flyway, Liquibase | versioned migrations | not an online copy engine and not a migration safety supervisor |
| An in-house script | fits one case exactly | brittle, hard to audit, usually no recovery path and no test matrix |

---

## 3. Product vision

Online schema change on a single MySQL instance should feel like a documented operational procedure that can be understood and audited, not a risky experiment with a long command.

Ideally the user supplies a target and an ALTER:

```bash
saksama advise -h db.prod -d shop -t orders \
  -a "ADD INDEX ix_created (created_at)"
```

And `saksama` answers:

- is the ALTER understood,
- can it use `INSTANT` or `INPLACE, LOCK=NONE`,
- what are the MDL, disk, foreign key, key length, privilege, and server load risks,
- which engine is recommended and why,
- what is the duration range and its confidence,
- will the tool allow execution,
- what recovery is available at each phase.

### 3.1 Product identity

| Item | Value |
|---|---|
| Name | **Saksama**, from KBBI: *teliti; cermat; hati-hati* (meticulous, careful) |
| Standard variant spelling | *seksama*, used for a redirect domain, not as the primary name |
| Binary | `saksama`, shell alias `skm` |
| Daemon | `saksama daemon start\|stop\|status`, one binary, a subcommand |
| Domains | `saksama.dev`, `saksama.app`, `saksama.id`, `seksama.dev` as a redirect |
| Repo and module | `github.com/perogeremmer/saksama-core` |
| State and socket | `~/.local/state/saksama/`, `saksama.sock` |
| Tagline | "Examine before you alter." |

**Name and product:** saksama is not a metaphor. It names the core function, inspecting before changing, and the product principle, fail closed and explain precisely. This document uses "Saksama Core" for the product and `saksama` for the binary and command.

**Naming consequences the product and docs must respect:**

1. It is an ordinary Indonesian word, so always capitalise it as a brand and always carry the tagline. Do not rely on the name alone for discoverability.
2. An English speaker will not guess the meaning, so the tagline must be on the first line of the README.
3. People will search for the variant spelling, so keep `seksama.dev` as a redirect and mention it once.
4. It is long to type, so ship the `skm` alias for manual use while scripts and CI use `saksama` explicitly.

---

## 4. Goals and non-goals

### 4.1 Goals for v1

1. Produce a plan that is accurate, conservative, and readable by both a human and a machine.
2. Run the subset of native MySQL DDL whose capability has been verified.
3. Run gh-ost on tables with no foreign keys, and pt-osc where gh-ost does not fit.
4. Let the operator choose the engine, record that choice, and execute it.
5. Supervise the process through a core that does not depend on the client's lifetime.
6. Check the schema, the operation, and the server settings before execution.
7. Refuse syntax, capability, or recovery paths that are not understood.
8. Provide session state, an event log, status, cancel, cleanup, and recovery guidance.
9. Support headless CLI use and CI integration.
10. Prevent credentials from appearing in argv, events, or logs.
11. Report which MySQL and engine versions are verified, with evidence.

### 4.2 Non-goals for v1

- writing a new copy engine,
- automating foreign key drop and recreate,
- promising data rollback after a cutover,
- blue-green deployment of an application,
- dual writes, CDC, or traffic switching,
- general purpose data backfill,
- the MySQL Clone Plugin as a table migration primitive,
- database engines other than MySQL, with MariaDB explicitly refused,
- a hosted SaaS or web UI,
- guaranteeing zero downtime for every ALTER,
- understanding the entire MySQL `ALTER TABLE` grammar.

### 4.3 Later goals

- a daemon with a socket so clients can attach and detach,
- a cutover pause and a cancel that truly stop an engine and clean up its artifacts,
- a TUI,
- a web UI, approval workflow, RBAC, and audit export,
- expand-contract advice and index hygiene,
- PostgreSQL, treated as a separate engine family with its own capability rules.

---

## 5. Target users

### 5.1 Primary persona: backend developer at a small startup

**Characteristics:** manages the application and the database, no DBA, large production tables, one primary instance, deploys over SSH or a simple CI, knows SQL but not MySQL internals.

**Needs:** to know whether an ALTER locks or rebuilds, to know the safe command, to get an actionable explanation, to be sure a dropped SSH session does not kill the migration, and to be able to inspect and cancel before a cutover.

### 5.2 Second persona: technical founder

**Characteristics:** sensitive to cost and engineering time, cannot build an internal platform, needs strong defaults, prefers a tool that refuses over one that quietly takes a risk.

### 5.3 Third persona: platform or DevOps generalist

**Characteristics:** runs several services without a DBA, needs JSON output, exit codes, an audit trail, and repeatability, and wants schema safety checks in CI.

---

## 6. Jobs to be done

1. When I am about to run an ALTER on a production table, I want to know the execution path and its risks before anything changes.
2. When the ALTER can be done natively, I want the tool to choose native DDL instead of copying the table.
3. When the ALTER needs a copy engine, I want the tool to verify that my table and my server support it.
4. When the tool does not understand the ALTER, I want it to refuse rather than guess.
5. When the migration is long, I want to disconnect and reconnect without stopping it.
6. When the copy is finished, I want to verify the shadow table before the swap.
7. When something goes wrong, I want to know which recovery action is actually safe at this phase.
8. When I run a plan in CI, I want stable JSON and a stable exit code.
9. When I am unsure which engine suits my table, I want the tool to inspect it and tell me, with the trade-offs.

---

## 7. User experience principles

1. **Plan before mutation.** Every execution is preceded by a recorded plan.
2. **No silent fallback.** MySQL must never quietly pick a more expensive algorithm or lock mode.
3. **Unknown means blocked.** Unrecognised syntax or capability is refused.
4. **Risk must be actionable.** Every blocker and warning states the reason and the remediation.
5. **Recovery is phase aware.** The UI never offers "rollback" without naming the consistency boundary.
6. **Production requires friction.** A hostname confirmation and an audited override are deliberate.
7. **Headless is first class.** Every core function works without a TUI.
8. **Secrets never become events.** No credential appears in output or in the audit trail.
9. **The recommendation is not the decision.** The analyzer suggests, the operator chooses, the tool executes.

---

## 8. MVP scope

### 8.1 Initial platform targets

- MySQL 8.0 and 8.4, verified. MySQL 5.6 and 5.7 are claimed by the capability rules but not verified by the test suite.
- Linux amd64 and arm64.
- Self-hosted MySQL first, with RDS and Aurora detected and their extra restrictions reported.
- InnoDB tables only.

### 8.2 Targeted ALTER operations

The first subset is defined by the capability matrix and integration fixtures:

- `ADD INDEX`, `DROP INDEX`,
- `ADD COLUMN` where the version genuinely supports `INSTANT`,
- `DROP COLUMN` where the version genuinely supports `INSTANT`,
- `SET DEFAULT`, `DROP DEFAULT`, `RENAME COLUMN`, `RENAME TABLE`,
- anything else is analysed and reported as unsupported, not executed.

Support is never granted on the strength of the tokenizer alone. Every executable operation needs a grammar fixture, a capability rule per version, a positive test, a negative test, and an explicit `ALGORITHM` and `LOCK` behaviour.

### 8.3 Engines in v1

| Engine | Status | Constraint |
|---|---|---|
| Native MySQL DDL | required | verified capability only, explicit algorithm and lock |
| gh-ost | required | no foreign keys in either direction, single-column key, binlog configured |
| pt-online-schema-change | required | never bundled, needs the Perl stack, no cutover pause |
| MySQL Clone Plugin | not supported | it clones an instance, not a table |

---

## 9. Main user flows

### 9.1 Inspect the server and the schema

```bash
saksama advise --live -h db.prod -d shop
```

1. The CLI connects and reads the server settings the engines need.
2. It reads every table in the schema in a handful of bulk queries.
3. The analyzer judges each table against each engine.
4. The output shows a settings doctor, then a table of tables with the engine to use for a rebuild.
5. Nothing changes.

### 9.2 Plan without mutation

```bash
saksama plan --live -h db.prod -d shop -t orders \
  -a "ADD INDEX ix_created (created_at)"
```

1. The CLI sends the request to the core.
2. The core introspects the server and the table.
3. The parser turns the ALTER into operations.
4. The analyzer applies capability rules and checks.
5. The core saves the plan and emits a plan card.
6. Nothing changes.
7. Blockers produce a non-zero exit code.

### 9.3 Apply native DDL

```bash
saksama apply -h db.prod -d shop -t orders \
  -a "ADD INDEX ix_created (created_at)" \
  --production --confirm db.prod
```

1. Build or reload the plan.
2. Revalidate the schema fingerprint and the server capability.
3. Run the preflight operational checks immediately before execution.
4. Require the production confirmation.
5. Set a conservative `lock_wait_timeout` on the session.
6. Run the ALTER with explicit `ALGORITHM` and `LOCK`.
7. Monitor the state.
8. Verify the result through `information_schema`.
9. Emit a completion event and an audit summary.

### 9.4 Choose the engine, then apply

```bash
saksama engine set pt-osc -d shop -t orders --note "foreign keys must be copied"
saksama apply -h db.prod -d shop -t orders -a "MODIFY COLUMN status varchar(64) NOT NULL"
```

1. The recorded choice is resolved for this table, falling back to the database and then the global choice.
2. The chosen engine is judged on its own requirements.
3. If it cannot run, the plan refuses and names what to change.
4. Otherwise the argv is built, shown, and executed.
5. Progress is polled from the database, never parsed from engine output.
6. The result is verified through `information_schema`.

### 9.5 Hold the cutover

```bash
saksama apply ... --engine=gh-ost --hold-cutover
saksama status s3
saksama cutover s3
```

Planned for phase 3. The copy finishes, the swap is held, the operator inspects the shadow table, then releases the cutover. This is a verification gate, not blue-green.

### 9.6 Reconnect after a dropped SSH session

Planned for phase 3, together with the daemon. Today a copy engine runs as a child of `apply`, so a dropped session ends it, which is stated plainly in the docs.

### 9.7 Cancel before the cutover

Planned for phase 3. Cancelling stops the engine and cleans up only the artifacts recorded as owned by that session.

### 9.8 Recovery after a cutover

```bash
saksama recover s3 --mode forward-fix
```

Planned. The tool does not rename back automatically, explains that new writes may exist only in the new table, shows the available recovery modes, requires explicit approval, may require a write freeze, and records the decision.

---

## 10. Functional requirements

### FR-1: parse and normalise the ALTER

- Support an explicit grammar subset.
- Handle backtick identifiers, nested parentheses, comments, and commas inside expressions.
- Reject multi-statement input.
- Reject ambiguous or unsupported syntax.
- Store both the original SQL and the normalised form.
- Never rely on a plain regex to understand a whole ALTER.

**Acceptance:** every supported fixture produces the right operations; every unsupported fixture produces a stable blocker rather than a panic or a fallback execution; input with an injected second statement is rejected.

### FR-2: database introspection

Read the vendor and version, storage engine, column type, charset, collation, nullability, default, and generated status, indexes and their parts, primary and unique keys, foreign keys in both directions with their actions, triggers, size and row count, row format, partitioning, active queries and transactions, metadata locks, replica state, grants, the relevant server settings, and leftovers from previous migrations.

Introspection is separate from the analyzer so the analyzer can be tested without a database.

### FR-3: capability matrix

Determine capability from the vendor and version, the engine version, the storage engine, table features, the operation, and managed-service constraints. Every decision yields a rule ID, a supported or blocked verdict, the expected algorithm and lock, the reason, the permitted fallbacks, and a reference. Unknown capability is a blocker.

### FR-4: check catalog

Implement the checks with stable IDs from [`docs/CHECKS.md`](docs/CHECKS.md). Each check yields:

```json
{
  "id": "D1",
  "severity": "WARN",
  "title": "long-running statement may hold the metadata lock",
  "detail": "1 statement has been running for 127s",
  "remediation": "wait for it, or terminate it deliberately",
  "mysql_error": 1205
}
```

Severity: `BLOCKER` refuses execution, `WARN` allows it with awareness, `INFO` is supporting detail.

`--force` may not bypass a blocker that protects a core invariant: an unparsed ALTER, a copy engine with foreign keys it cannot handle, an incompatible engine capability, clearly insufficient disk, a changed schema fingerprint, or unsafe credential handling. Overridable blockers need an explicit allowlist, never a global rule.

### FR-5: plan

The plan contains the host, database, and table, the original and normalised ALTER, a schema fingerprint, server information, the engine recommendation and its source, the fallbacks, the reasons, the checks, a sanitised preview of the SQL or argv, a duration range with confidence, a disk estimate, the recovery mode per phase, an assessment of every engine, the server setting report, and a timestamp.

The plan is available as a human-readable card, as stable JSON, and as a persisted session artifact.

### FR-6: engine routing

1. Native when every operation's capability is verified.
2. gh-ost when a rebuild is needed, the table qualifies, and the server can start it.
3. pt-osc when gh-ost does not qualify or the server cannot start it.
4. Refuse when no engine qualifies.

A manual override never bypasses capability or safety checks; it selects a path and is then judged on it.

### FR-7: native execution

- Use an explicit `ALGORITHM` when the target supports the syntax.
- Use an explicit `LOCK`.
- Never allow a silent fallback to a more expensive algorithm.
- Set `lock_wait_timeout` on the migration session.
- Recheck transactions and metadata locks immediately before the ALTER.
- Verify the result by introspection, not by exit code.
- Emit progress and state even when a percentage is not meaningful.

### FR-8: copy engine execution

- Detect the engine version and validate it against the declared matrix.
- Refuse credentials in argv unless the operator opted in explicitly.
- Block tables with foreign keys for gh-ost, and report the reason.
- Support `--allow-on-master` on a single server, with a warning and conservative throttling.
- Support a cutover pause where the engine offers one.
- Supervise the process from the core.
- Use stdout and stderr for diagnosis only: progress must not depend on parsing an unstable log format.
- Quote the engine's own final output lines when it fails, because an exit code alone is not a diagnosis.

### FR-9: session lifecycle

States: `planned`, `analyzing`, `approved`, `running`, `verifying`, `done`, `failed`, `cancelled`, `blocked`. Transitions are validated, and a command that is invalid for the current state is rejected idempotently.

### FR-10: approval

Approval is required for execution against a production host, for a cutover, for overriding a warning, for recovery after a cutover, and for any action that needs a write freeze. Each approval records the action, the actor, the timestamp, the plan or schema fingerprint, a risk note, and a correlation ID.

### FR-11: cutover pause

Available only for engines that support it. The state must make clear whether the production table has been swapped. The tool shows the verification instructions, accepts `cutover` or `cancel`, and never claims this is application blue-green.

### FR-12: cancel and cleanup

Cancel stops the engine gracefully. Cleanup removes only artifacts it can prove belong to the session, and refuses while an artifact is still needed for recovery. Artifact names and ownership are recorded in the session state rather than guessed from a prefix. Every drop is written to the audit log.

### FR-13: recovery

| Phase | Recovery | Guarantee |
|---|---|---|
| Before a cutover | cancel, cleanup | the production table has not been swapped |
| Cutover held | cancel, cleanup | the shadow table is discarded, production is untouched |
| Cutover ambiguous or crashed | reconcile | the database must be observed before acting |
| After a cutover | forward fix, or a procedure with a write freeze | no automatic data rollback |

The tool never offers a rename back without detecting the write boundary, explaining the risk of losing writes, requiring approval and a write freeze where relevant, and following a tested procedure.

### FR-14: status and progress

Status shows the session, its state and phase, the engine and version, elapsed time, progress where available, current load and throttling, replica lag where relevant, the active checks, the next action, the recovery mode, the last error, and the last event sequence number.

### FR-15: event contract

Every frontend uses one event contract, defined in `internal/events` and described here. The envelope is:

```json
{"contract_version": 1, "seq": 142, "ts": 1756930000.123, "type": "phase.progress", "session": "s3", "data": {}}
```

Requirements: monotonic sequence per session, an append-only audit log, a JSON Schema per event type, a snapshot for reconnects, redaction before emission, at most one progress event per second per phase, and command idempotency with correlation IDs.

### FR-16: daemon and core

The core runs separately from the CLI and the TUI, uses a unix socket by default, persists state durably, survives a client disconnect, reconciles orphaned sessions at startup, supports one writer and many readers, and prevents two conflicting executions on the same table.

### FR-17: CLI and JSON

```text
saksama advise
saksama plan
saksama apply
saksama status
saksama events
saksama export
saksama cleanup
saksama engine list|set|show|clear
saksama compat
saksama schema
saksama version
saksama daemon start|stop|status
saksama cutover | cancel | recover
```

Every command that matters supports `--json`, and machine-readable output never mixes in decorative progress on stdout.

### FR-18: credentials

Credentials come from `~/.my.cnf`, from `--defaults-extra-file` with safe permissions, from a secret provider that is never printed, or from an interactive prompt when a TTY exists.

Forbidden: a password in argv, a password in an event or a log, an unsanitised DSN, an audit export containing a secret, and a credential file that other users can read.

### FR-19: audit export

The export covers the plan, the checks, the approvals, the engine and version, the phase transitions, the redacted command, the verification results, the recovery decision, and a completeness marker. Initial formats are JSONL and a JSON summary.

---

## 11. Non-functional requirements

### NFR-1: safety

Fail closed by default. No silent capability fallback. No automatic foreign key manipulation. Every destructive action is scoped to a session artifact. The schema fingerprint is verified before execution and before a cutover.

### NFR-2: reliability

A core crash must not assume the migration failed or succeeded. Startup reconciles against the database and the process state. Commands are idempotent on retry.

### NFR-3: performance

The offline analyzer targets a p95 under 100 ms for a normal schema fixture. A live plan targets a p95 under 5 seconds excluding network and blocked queries. Progress emits at most once per second per session. The tool must never be the bottleneck compared with the engine.

### NFR-4: compatibility

A static binary for Linux amd64 and arm64 where dependencies allow. `TERM=dumb` and `NO_COLOR` produce plain output. The CLI stays usable at 80x24. Headless mode needs no TTY. Tables use ASCII borders rather than box-drawing characters.

### NFR-5: security

The socket is owner-only by default. The state directory and credential files require strict permissions. All logs pass through redaction. SQL shown to the user considers sensitive literals. Remote TCP is disabled until authentication and TLS exist.

### NFR-6: testability

The analyzer is a pure library with no database dependency. Introspection queries are tested against a container. Every check has a positive and a negative test. Every executable capability has a versioned integration fixture. Crash, reconnect, and reconcile are tested with fault injection.

### NFR-7: observability

The core produces structured events, a structured internal log, a session summary, engine exit metadata, redaction metrics, and an incomplete-audit marker after an unreconciled crash.

---

## 12. UX and output

### 12.1 The settings doctor

Before judging any table, `advise` shows what the server reports against what the engines need:

```
SETTING            CURRENT    REQUIRED    STATUS  NEEDED BY   WHY
log_bin            OFF        ON          FAIL    gh-ost      gh-ost follows writes through it
binlog_format      ROW        ROW         ok      gh-ost      statement format loses row data
binlog_row_image   FULL       FULL        ok      gh-ost      MINIMAL omits needed columns
server_id          1          non-zero    ok      gh-ost      gh-ost refuses server_id=0
```

### 12.2 The engine matrix

Every engine is judged, including the ones that do not fit, with the evidence and the trade-offs:

```
NATIVE    UNSUITABLE
  [FAIL] no table rebuild required     these operations rebuild the table: modify_column
GH-OST    UNSUITABLE
  [FAIL] no foreign keys in or out     2 constraint(s) involve this table
PT-OSC    SUITABLE  (ready)
  [  ok] primary or unique key         the table can be chunked
```

### 12.3 Plan card

```
ALTER      ADD INDEX ix_created (created_at)
TARGET     shop.orders @ db.prod
ENGINE     native [INPLACE, LOCK=NONE] (8.0.36)
DURATION   35s - 60m | confidence medium
WHY        every operation has a verified native DDL rule

RECOVERY   before execution: nothing has been mutated
           after success: forward migration only

RECOMMEND  native
BLOCKERS   0
RESULT     ready (nothing has been changed)
```

### 12.4 Blocked plan

```
BLOCK [R3] gh-ost is not installed on this machine
  detail:  the engine fits this change, but Saksama cannot start it
  action:  install it from the gh-ost releases page
```

### 12.5 Production confirmation

```
Target is marked as production: db.prod
Re-run with: --confirm db.prod
```

In non-interactive mode, approval needs an explicit flag or token that is recorded, never an assumed yes.

---

## 13. Conceptual data model

**Session:** id, created and updated timestamps, state, phase, target, original and normalised ALTER, schema fingerprint, server version, engine, blockers and warnings, the plan, the statements, the approvals, the verification records, the last event sequence, the actor, the error.

**Plan:** operations, recommendation, fallbacks, reasons, checks, engine assessments, table facts, environment report, duration estimate, disk estimate, required approvals, expiry.

**Artifact:** session id, kind, database, name, ownership marker, creation time, whether it is safe to delete.

**Approval:** command id, action, actor, decision, risk note, plan fingerprint, timestamp.

**Engine choice:** engine, scope, database, table, actor, note, timestamp.

---

## 14. Duration estimates

The estimate must not be a spuriously precise number. Sources, in order of preference:

1. A local historical profile per engine and table shape.
2. A bounded read-only benchmark with an explicit query budget.
3. A heuristic fallback with low confidence.

Output always contains a range, a confidence level, the basis for the estimate, and the factors that could extend it.

---

## 15. Success metrics

### 15.1 North star

The percentage of migration plans that produce a safe, actionable decision without the user having to understand engine options manually.

### 15.2 Product metrics

- the share of plans that choose native rather than a copy engine,
- the share of executions that finish without a manual incident,
- the number of blockers that prevented a dangerous operation,
- the false positive rate per check,
- incidents caused by a missed check (target: zero),
- plan to apply conversion,
- median time from plan to decision,
- reconnect success rate,
- guidance completion rate,
- credential leaks (target: zero),
- silent foreign key semantic changes (target: zero).

### 15.3 Go/no-go for the MVP

The MVP is not released as "safe for production" until every executable capability has an integration test, the foreign key copy path is genuinely handled or blocked, the secret redaction tests pass, the crash and restart reconcile tests pass, the native fallback behaviour is tested, the hold, cancel, and cutover state machine is tested, and there is no severity-1 data loss bug from a staging soak test.

Current state: the integration suite passes on MySQL 8.0.46 and 8.4.13, gh-ost 1.1.11 has run a real migration, and pt-osc is **not** verified against a real binary. The staging soak test has not been run.

---

## 16. Product-level acceptance criteria

**Plan:** a user can run `plan` without changing the database; unsupported ALTERs are always refused with a reason; the JSON schema is stable; a blocker produces a non-zero exit code.

**Native execution:** the tool never lets MySQL silently fall back; it revalidates the schema before execution; it checks for long transactions and metadata locks immediately before the ALTER; it verifies the resulting schema.

**Copy engines:** a table with foreign keys is refused for gh-ost; the argv is shown before it runs; progress comes from the database; the result is verified; an engine that exits 0 without doing the work is still reported as a failure.

**Engine choice:** an unrecorded table follows the recommendation; a recorded choice wins; a choice that cannot run is refused with the reason; the analyzer's own pick stays visible.

**Recovery:** before a cutover, cancel does not change the production table; after a cutover, the UI never claims an automatic rollback is available; an ambiguous crash triggers reconciliation rather than automatic cleanup.

**Security:** no password appears in the process list, in an event, in a log, or in an export; the socket and the state directory have safe permissions; a world-readable credential file is rejected.

---

## 17. Testing strategy

### 17.1 Unit tests

The ALTER parser and normaliser, the capability rules, the check evaluator, the engine argv builder and redaction, the state transitions, the recovery mode selection, the event schema, the table renderer, and the compatibility document generator.

### 17.2 Integration tests against MySQL

Fixtures cover: no primary key, auto-increment primary key, UUID primary key, composite primary key, incoming and outgoing foreign keys, foreign keys with `ON DELETE CASCADE` and `ON UPDATE SET NULL`, utf8mb3 and utf8mb4 mismatches, key length overflow, TEXT and BLOB indexes, generated columns, partitioned tables, an active long transaction, a metadata lock blocker, low disk, an existing trigger, leftover artifacts, a check constraint violation, a schema that changes between plan and apply, and a server with binary logging disabled.

### 17.3 End to end

Native plan to execution to verification; copy engine plan to copy to verification; a no-op engine still failing verification; disconnect and reconnect (phase 3); core crash and restart reconciliation (phase 3); engine crash; a duplicate command ID; cleanup refusing while an artifact is still needed.

### 17.4 Adversarial

Malicious identifiers and comments, multi-statement input, a credential inside an error string, a stale plan, a race between cutover and cancel, two sessions on the same table, and a database state that does not match the last event.

---

## 18. Roadmap

| Phase | Content | State |
|---|---|---|
| 0 | decision log, check catalogue, event contract | done |
| 1 | analyzer, offline plan, JSON and plan card | done |
| 2 | live introspection, native execution, sessions, events, engine matrix, server doctor | done |
| 3 | daemon, cutover pause, cancel, cleanup, crash reconciliation | not built |
| 4 | TUI | not built |
| 5 | pt-osc verified against a real binary, RDS and Aurora profiles, broader MySQL support | partially |
| 6 | web control plane, RBAC, scheduled windows | not started |
| 7 | PostgreSQL | not started |

---

## 19. Risks and mitigation

| Risk | Impact | Mitigation |
|---|---|---|
| the capability matrix is wrong | an unexpected lock or rebuild | versioned fixtures, explicit algorithm and lock, fail closed |
| the ALTER parser is wrong | execution differs from the plan | a narrow grammar, refuse the unknown, adversarial tests |
| a foreign key is broken | silent data integrity failure | read actions from structured metadata, never automate FK changes in v1 |
| a rollback discards writes | data loss | never promise a rename back, phase aware recovery |
| MDL causes an outage | query queueing and downtime | preflight plus an immediate recheck plus a short `lock_wait_timeout` |
| disk fills up | the migration fails and the instance suffers | a conservative disk estimate and a blocker |
| engine output changes | progress and logic break | never treat parsed output as the source of truth |
| credentials leak | security incident | file based credentials, a redaction layer, leak tests |
| a core crash during a cutover | ambiguous state | reconcile the database and the process before transitioning |
| scope grows too wide | the product never ships | MySQL 8.0 and a subset of ALTERs, native first |
| a false sense of zero downtime | a production incident | no absolute claims, show the residual risk |
| low usage frequency | weak retention | CI planning, expand-contract advice, index hygiene |

---

## 20. Dependencies and licenses

Go. A CLI framework chosen at implementation time. Bubble Tea and Lip Gloss for the TUI. `go-sql-driver/mysql`. gh-ost is MIT and may be bundled if distribution and LICENSE allow. `pt-online-schema-change` is GPL-2 and is never bundled, only invoked as an external binary. The product is MIT.

Every new dependency is checked for license, maintenance status, binary size, CGO requirement, security history, and whether it supports redaction and observability.

---

## 21. Documentation required at release

Quick start, the safety model and its guarantees, the supported MySQL and engine matrix, the supported ALTER matrix, an explanation of MDL and native online DDL, the cutover pause guide, the recovery guide per phase, credential setup, CI integration, RDS and Aurora limits, troubleshooting and artifact cleanup, and a security reporting policy.

The documentation must distinguish clearly:

- online DDL from zero downtime,
- pre-cutover verification from blue-green,
- schema recovery from data rollback,
- overridable warnings from invariant blockers.

---

## 22. Open questions

1. ~~Final name~~ resolved: Saksama Core, see D13.
2. ~~Minimum MySQL version for the MVP~~ resolved: 8.0 and 8.4 verified, 5.6 and 5.7 claimed only.
3. ~~Is RDS in the MVP or preview only~~ partially resolved: detected and reported, not verified.
4. How is a host marked as production: configuration, a label, a hostname pattern, or a combination?
5. Which blockers may be overridden, and who may override them?
6. ~~A safe default `lock_wait_timeout`~~ resolved: 10 seconds, configurable.
7. ~~Is gh-ost bundled or installed separately~~ resolved: installed separately today, embedding is allowed by license.
8. How is the schema fingerprint computed so it is stable yet sensitive to relevant change? Resolved for v1: a SHA-256 over the canonical schema, ignoring volatile row and size counts.
9. How long is a plan valid before a replan is required?
10. Should the state store start as SQLite, or as SQLite plus JSONL from phase one? Resolved for phase 2: JSON files plus JSONL.
11. Should `saksama daemon` run as a user service automatically or only on request?
12. How is free disk measured on RDS, where there is no filesystem access? Resolved: measured only when `@@datadir` exists locally, otherwise reported as not measured.
13. Does an offline plan accept `SHOW CREATE TABLE`, a schema JSON, or both? Resolved: both.
14. Is MariaDB explicitly refused in the MVP? Resolved: yes, check E1.
15. What is the cancel policy for a native DDL that has already started?
16. Which PostgreSQL operations, if any, can be analysed without an online rewrite? Open, for whoever picks up phase 7.

---

## 23. Binding MVP decisions

1. Native first, but only for proven capability.
2. An unknown ALTER is blocked, never assumed to be a rebuild.
3. Foreign key DDL is out of scope and blocked in v1.
4. The cutover pause is a verification gate, not full blue-green.
5. No claim of automatic data rollback after a cutover.
6. The CLONE plugin is not a table engine and is not in v1.
7. CLI and headless first, TUI after the core stabilises.
8. The core lives independently of the client.
9. The event contract and JSON output are the product API.
10. Credentials never appear in argv, an event, or an audit export in clear text.
11. The recommendation is advisory; the operator's recorded choice is executed.
12. Server settings are a separate axis from engine suitability, and unknown is not a pass.

---

## 24. Definition of done for v1

v1 is done when a user can:

1. Install one tool and connect it to MySQL with safe credentials.
2. Run `saksama advise` and see the server settings and which engine suits each table, without naming a table or a change.
3. Run `saksama plan` for the supported ALTER subset.
4. Get a recommendation with the reasons, the checks, an estimate, and the recovery boundaries.
5. Use the JSON output as a CI gate.
6. Record an engine choice and have the tool execute it.
7. Run a verified native DDL without a silent fallback.
8. Run gh-ost on a table without foreign keys, with a durable session.
9. Get a refusal that names the exact setting or package to fix, instead of an engine that fails at startup.
10. Export an audit trail with no credentials in it.

Success for v1 is not measured by how many ALTERs or engines are supported. It is measured by the accuracy of the decisions, the safety of the defaults, and the tool's willingness to refuse what it cannot guarantee.
