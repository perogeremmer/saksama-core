# Implementation Status

> Last updated: 2026-11-04 · build: `saksama dev` · repo: `github.com/perogeremmer/saksama-core`
> This file is the honest ledger. If something is not here, it is not built.

---

## What works today

```bash
make build
./bin/saksama plan --ddl testdata/orders.sql \
  --alter "ADD INDEX ix_created (created_at)" \
  --mysql-version 8.0.36 --host db.prod \
  --rows 59000000 --data-bytes 18000000000 --index-bytes 4000000000
```

### Packages

| Package | Purpose | Tests |
|---|---|---|
| `internal/model` | neutral schema + ALTER model, version parsing | via parser/analyzer |
| `internal/parser` | fail-closed tokenizer, `ALTER TABLE` parser, `SHOW CREATE TABLE` parser | 19 tests |
| `internal/analyzer` | capability rules, checks, engine routing, duration/disk estimates, fingerprint | 34 tests |
| `internal/render` | ASCII-only plan card | 5 tests |
| `internal/events` | event envelope, redaction, per-second progress limiting | 9 tests |
| `internal/cli` | `plan`, `schema`, `version`; exit codes | 14 tests |

Total: **81 tests**, all green (`make test`).

### ALTER operations understood

`ADD INDEX` · `ADD UNIQUE` · `ADD FULLTEXT` · `ADD SPATIAL` · `DROP INDEX` ·
`ADD COLUMN` · `DROP COLUMN` · `MODIFY COLUMN` · `CHANGE COLUMN` ·
`RENAME COLUMN` · `RENAME TO` · `ADD PRIMARY KEY` · `DROP PRIMARY KEY` ·
`ADD FOREIGN KEY` (blocked) · `DROP FOREIGN KEY` (blocked) ·
`CONVERT TO CHARACTER SET` · `ENGINE=` · `ALTER COLUMN SET/DROP DEFAULT` ·
`ALGORITHM=` / `LOCK=` options.

Anything else → `OpUnknown` → blocker `E0`. Never a guess.

### Checks implemented

Offline-evaluable checks from `docs/CHECKS.md`:

- **A1** FK charset not verifiable offline (INFO — says so instead of faking it)
- **A2** charset conversion on a table with FKs (BLOCKER, 1832)
- **A3** FK column charset ≠ table charset (WARN, 3780)
- **A4** deprecated `utf8`/`utf8mb3` (WARN)
- **A5** index key over the InnoDB limit (BLOCKER, 1071), row-format aware
- **A6** legacy `COMPACT`/`REDUNDANT` row format (WARN, 1071)
- **A7** TEXT/BLOB indexed without a prefix (BLOCKER, 1170)
- **B1/B2** no primary / no unique key (WARN, escalated to BLOCKER when a rebuild is needed)
- **B3** composite PK (INFO → routing hint)
- **B4** UUID PK (WARN, chunking consequences)
- **B5** unique index with unverified duplicates (INFO offline)
- **B6** prefix index (INFO)
- **B7** FK column without index prefix (WARN)
- **B8** index bloat (INFO)
- **C1** column used by outgoing FK (BLOCKER, 1832)
- **C2** column referenced by incoming FK (BLOCKER, 1832)
- **C3** referential action must be preserved (INFO)
- **C5** many FK constraints (WARN)
- **C8** FK DDL out of scope (BLOCKER)
- **D2** bounded `lock_wait_timeout` (INFO)
- **D6** disk headroom (BLOCKER when measured and insufficient, WARN otherwise)
- **D7** existing triggers (WARN)
- **D10** partitioned table (WARN)
- **D12** production target (WARN, deliberate friction)
- **D17** very large table (WARN)
- **E0** unknown operation (BLOCKER)
- **E1** MariaDB (BLOCKER)
- **E2** non-InnoDB (BLOCKER)
- **E3** unparseable version (BLOCKER)
- **R1** rebuild required but no primary key (BLOCKER)
- **R2** rebuild required with composite PK (BLOCKER)
- **R3** rebuild required with foreign keys (BLOCKER)

### Capability matrix (verified in tests)

| Operation | Rule | Behaviour |
|---|---|---|
| `ADD INDEX` / `DROP INDEX` | `R-ADD-INDEX-INPLACE`, `R-DROP-INDEX-INPLACE` | INPLACE, LOCK=NONE, 5.6+ |
| `ADD COLUMN` (appended) | `R-ADD-COLUMN-INSTANT-8012` | INSTANT, 8.0.12+ |
| `ADD COLUMN` (FIRST/AFTER) | `R-ADD-COLUMN-INSTANT-8029` | INSTANT, 8.0.29+ |
| `DROP COLUMN` | `R-DROP-COLUMN-INSTANT-8029` | INSTANT, 8.0.29+ |
| `SET/DROP DEFAULT` | `R-DEFAULT-INSTANT` | INSTANT, 8.0.12+ |
| `RENAME COLUMN` | `R-RENAME-COLUMN-INPLACE` | INPLACE, 8.0+ |
| `RENAME TO` | `R-RENAME-TABLE-METADATA` | metadata, instant |
| `MODIFY` / `CHANGE COLUMN` | `R-MODIFY-COLUMN-COPY` | rebuild → copy engine |
| `ADD`/`DROP PRIMARY KEY` | rebuild | copy engine |
| `CONVERT TO CHARACTER SET` | rebuild | copy engine |
| `ADD FULLTEXT`/`SPATIAL` | rebuild | copy engine |
| everything else | `R-UNKNOWN-OP` | blocked |

## What is not built yet

| Item | Phase | Note |
|---|---|---|
| Live introspection (`information_schema`, `performance_schema`) | 2 | unlocks A1 verified, B5 duplicates, D1 MDL check, D8 leftovers |
| Native execution with explicit `ALGORITHM`/`LOCK` | 2 | |
| Session state store + approvals + audit export | 2 | |
| gh-ost supervision, hold-cutover, cancel/cleanup | 3 | |
| Crash reconciliation | 3 | |
| TUI (Bubble Tea) | 4 | |
| pt-osc adapter | 5 | must not be bundled (GPL-2) |
| Anything else (`apply`, `status`, `cutover`, `cancel`, `recover`, `cleanup`, `daemon`, `events`, `export`) | — | exits `2` with an explicit "not implemented in this build" |

## Known gaps in the current build

1. **Offline schema means one table.** Checks that need the parent/child table (A1 fully, C2 column types) only see what is in the input, and the plan says so explicitly rather than assuming safety.
2. **Duration is heuristic.** No local history profile yet, so confidence is capped at `medium` and `low`. The `Basis` field always explains the number.
3. **`--engine` overrides route selection but not blockers.** Overriding to `gh-ost` on a table with foreign keys still refuses, by design.
4. **Native cancel semantics.** With no execution path yet this is expressed as recovery text, not behaviour.
5. **`information_schema` row counts are estimates.** The plan labels them with `~` and never uses them for correctness decisions.
