# Implementation Status

> Last updated: 2026-11-04 · build: `saksama dev` · repo: `github.com/perogeremmer/saksama-core`
> This file is the honest ledger. If something is not here, it is not built.

---

## What works today

```
saksama plan    [--live]   produce and record a plan
saksama advise  [--live]   engine suitability for one table, or the whole schema
saksama engine  list|set|show|clear    choose the engine yourself
saksama apply   ...        execute a native migration
saksama status  [id]       list or inspect sessions
saksama events  <id>       append-only JSONL event log
saksama export  <id>       audit export
saksama cleanup <id>       refuses to guess ownership
saksama schema             JSON Schema of the plan contract
```


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
| `internal/analyzer` | capability rules, checks, engine routing, duration/disk estimates, fingerprint, engine suitability matrix, schema-wide assessment | 46 tests |
| `internal/render` | ASCII-only plan card, engine matrix, schema fitness table | 10 tests |
| `internal/events` | event envelope, redaction, per-second progress limiting | 9 tests |
| `internal/probe` | detects installed gh-ost / pt-osc / perl and their versions | 2 tests |
| `internal/engine/copyeng` | gh-ost and pt-osc adapters: argv, subprocess, verification | 12 tests |
| `internal/cli` | `plan`, `plan --live`, `advise`, `engine`, `apply`, `status`, `events`, `export`, `cleanup`, `schema`, `version` | 27 tests |

Total: **165 unit tests** + **24 integration tests** against real MySQL 8.0.46, all green.

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
| gh-ost supervision, hold-cutover, cancel/cleanup | 3 | engine matrix already reports whether it *fits* |
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
