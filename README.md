<div align="center">

# Saksama Core

**Examine before you alter.**

Safe MySQL schema migrations for teams with one database, no DBA, and no budget for blue-green.

[![CI](https://github.com/perogeremmer/saksama-core/actions/workflows/ci.yml/badge.svg)](https://github.com/perogeremmer/saksama-core/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.24+-00ADD8.svg)](go.mod)

`Periksa saksama, baru ubah.`

</div>

---

**Saksama** (KBBI: *teliti, cermat, hati-hati*) is an Indonesian word meaning "meticulous". The standard spelling *seksama* is equally correct. The tool is called **Saksama Core** because the analyzer and session engine are meant to be embedded in other frontends — a TUI today, a web control plane later.

## Why this exists

You have one MySQL instance. No replica. No DBA. A 59-million-row table, and a change you have postponed for three months because you do not know whether `ALTER TABLE` will lock it for two minutes or two hours.

Blue-green would solve it. Blue-green is expensive in *engineering time*: dual writes, two deployment cycles, traffic switching, a rollback plan. That is the part small teams cannot afford — not the server.

So the real need is narrower than blue-green: **an online schema change on one instance, done correctly.**

The copy engines already exist and they are good. What is missing is the layer above them:

| Missing layer | What goes wrong without it |
|---|---|
| **Analysis** | The migration fails halfway: FK charset mismatch (3780), key too long for `utf8mb4` (1071), column used in a foreign key (1832), TEXT indexed without a prefix (1170), or a metadata-lock queue that turns one slow query into a total outage |
| **Routing** | A 59M-row table gets fully copied for a secondary index that MySQL can build in place with `LOCK=NONE` |
| **Guardrails** | The tool does something destructive that it cannot undo |
| **Honest recovery** | Someone promises "just roll back" for a phase where that silently discards writes |

Saksama Core is **not** a new copy engine and **not** a thin gh-ost wrapper. It owns the decision layer; engines stay interchangeable workers.

## You decide, we execute: `saksama engine`

Saksama is a wrapper. It recommends, you choose, and then **it runs the engine for you** — with the exact argv shown to you first, progress polled from the database, and the result verified against `information_schema`.

```
$ saksama advise --live -h db.prod -d shop -t orders -a "MODIFY COLUMN status varchar(64) NOT NULL"

NATIVE    UNSUITABLE
  [FAIL] no table rebuild required          these operations rebuild the table: modify_column
GH-OST    UNSUITABLE
  [FAIL] no foreign keys in or out          2 constraint(s) involve this table
PT-OSC    SUITABLE  (ready)
  [  ok] primary or unique key              the table can be chunked
  note  2 outgoing foreign key(s): pt-osc 3.x handles these natively

  analyzer recommends  pt-osc
  recorded choice      none, so the recommendation would be used
  decide with          saksama engine set <native|gh-ost|pt-osc>
```

### Ask about the whole database first

You do not have to name a table. With just `-d`, Saksama inspects every table in the
schema and tells you which engine suits each one, biggest first:

```
$ saksama advise --live -h db.prod -d shop

ENGINE SUITABILITY for schema shop
6 table(s), sorted by size. These are the engines for a rebuild;
an index-only change needs no engine at all on MySQL 8.0.

TABLE      ROWS    SIZE     PRIMARY KEY          FK   GH-OST  PT-OSC  REBUILD WITH
orders     59.0M   20.5 GB  id bigint unsigned   1/0  no      yes     pt-osc
m2m        180.0M  44.0 GB  composite(a_id,b_id) 0/0  no      yes     pt-osc
sessions   2.1M    640 MB   uuid char(36)        0/0  yes     yes     gh-ost
users      410.0k  88 MB    id bigint unsigned   0/0  yes     yes     gh-ost
audit_log  180.0M  44.0 GB  (none)               0/0  no      no      none

TABLES NO ENGINE CAN COPY
  audit_log
    - no primary key or unique key to chunk by

NOTES
  orders    pt-osc only: gh-ost cannot handle this table
  m2m       pt-osc only: gh-ost cannot handle this table

NEXT
  record your decision per table:  saksama engine set gh-ost -d <db> -t <table>
  inspect one table in detail:     saksama advise --live -d <db> -t <table>
```

`--alter` is optional throughout. Give it and the report covers that specific
change; omit it and the report covers the table's shape, which is what decides
the engine.

Record the decision, then apply:

```
$ saksama engine set pt-osc -d shop -t orders --note "FK must be copied"
engine set to pt-osc for shop.orders

$ saksama apply -h db.prod -d shop -t orders -a "MODIFY COLUMN status varchar(64) NOT NULL"
  [ 42.0%] copying 24.8M / 59.0M rows
  [100.0%] engine finished
SESSION    s3
STATE      done
ENGINE     pt-osc

STATEMENTS
  1. pt-online-schema-change \
  --alter "MODIFY COLUMN status varchar (64) NOT NULL" \
  --chunk-size 1000 --max-load Threads_running=50 --critical-load Threads_running=100 \
  --print D=shop,t=orders,h=db.prod,P=3306,u=deployer

VERIFICATION
  [ok  ] column_exists  status  column status present
  [ok  ] foreign_key_present  fk_orders_spot  constraint fk_orders_spot intact
```

```
saksama engine list                      engines, versions found locally, recorded choices
saksama engine set <name> [-d db -t t]   record a decision (default or one table)
saksama engine show [-d db -t t]         what apply would use right now
saksama engine clear [-d db -t t]        forget it
```

Precedence: `--engine` flag → recorded choice for `db.table` → recorded default → analyzer recommendation. The analyzer's own pick always stays visible, so an override is never silent.

## Which tool fits? `saksama advise`

The most common question is not "run this" — it is *"is this a gh-ost table or a Percona Toolkit table?"* That question deserves its own command, and it deserves an answer even when this build cannot execute the engine.

```
$ saksama advise --live -h db.prod -d shop -t orders \
                 -a "MODIFY COLUMN status varchar(64) NOT NULL"

ENGINE SUITABILITY for shop.orders
ALTER   MODIFY COLUMN status varchar(64) NOT NULL
SERVER  8.0.46
TABLE   8 cols, 3 indexes, 2 out / 0 in FK

[x]  NATIVE       BLOCKED by no table copy required
     plain ALTER TABLE, executed by MySQL itself with an explicit ALGORITHM and LOCK
     [  ok] understood operations            all operations are recognised
     [FAIL] no table copy required           these operations rebuild the table: modify_column
     [  ok] no foreign-key column touched    no constrained column is modified

[x]  GH-OST       BLOCKED by no foreign keys in or out
     row-by-row copy through a ghost table with triggers, then an atomic swap
     [  ok] MySQL 5.7 or newer               server is 8.0.46
     [  ok] InnoDB storage engine            table is InnoDB
     [FAIL] no foreign keys in or out        2 constraint(s) involve this table; gh-ost
                                             does not support foreign keys at all
     [  ok] single-column primary key        primary key is id
     [  ok] table is not partitioned         table is not partitioned
     [  ok] gh-ost binary installed          found /usr/local/bin/gh-ost (1.1.7)
     note  no replica detected: gh-ost must run with --allow-on-master
     note  estimated disk: about 41.0 GB extra

[!]  PT-OSC       NOT EXECUTABLE HERE
     Percona Toolkit's pt-online-schema-change: triggers plus a shadow table, then RENAME swap
     [  ok] MySQL 5.1 or newer               pt-osc supports every version Saksama plans for
     [  ok] InnoDB storage engine            table is InnoDB
     [  ok] primary or unique key            the table can be chunked
     [  ok] pt-osc binary installed          found /usr/local/bin/pt-online-schema-change (3.5.1)
     note  2 outgoing foreign key(s): pt-osc 3.x handles these natively when copying
     note  licensing: pt-osc is GPL-2, so it is never bundled

VERDICT
  the chosen path is refused (1 blocker(s)), but pt-online-schema-change satisfies
  every requirement for this change; its adapter is not built yet
```

Three verdicts, and the difference matters:

| Verdict | Meaning |
|---|---|
| `SUITABLE` | every requirement is met and this build can run it |
| `BLOCKED` | a hard requirement of *that engine* is not met (gh-ost + foreign keys, gh-ost + composite key, pt-osc + no key) |
| `NOT EXECUTABLE HERE` | the engine fits this change perfectly; this build has no adapter for it yet |

`BLOCKED` is about the engine. `NOT EXECUTABLE HERE` is about us. Confusing the two is how people end up attributing a tool's limitation to their own schema.

Use it offline too, and get JSON:

```bash
saksama advise --live -d shop                    every table in the schema
saksama advise --live -d shop -t orders          one table, no change needed
saksama advise --ddl orders.sql --mysql-version 8.0.36
saksama advise --live ... --json | jq '.tables[] | {table, rebuild_engine}'
saksama advise --live ... --no-probe             skip detecting installed binaries
```

The plan card carries a one-line summary so you never have to remember to ask:

```
ENGINES    native ok | gh-ost blocked | pt-osc fits/not-built   (see: saksama advise)
```

## See it work

```
$ saksama plan --live -h db.prod -d shop -t orders \
               -a "ADD INDEX ix_paid (paid_at)"

ALTER      ADD INDEX ix_paid (paid_at)
TARGET     shop.orders @ db.prod
ENGINE     native [INPLACE, LOCK=NONE] (8.0.46)
DURATION   3m29s - 17m29s | confidence medium
TABLE      ~59.0M rows, 20.5 GB, 8 cols, 3 indexes, 2 out / 0 in FK
FINGERPRINT sha256:0a208c2b6f7cc6bb...

WHY
  - every operation is supported by a verified native DDL rule; no table copy is needed
  - the operation is in-place and permits concurrent DML

OPERATIONS
  INPLACE, LOCK=NONE R-ADD-INDEX-INPLACE    ADD INDEX ix_paid (paid_at)

FINDINGS
  INFO    [A1] foreign key charset and collation agree
          constraint "fk_orders_spot" (outgoing spots) is compatible
  INFO    [D2] session lock_wait_timeout is bounded

RECOVERY
  Before execution: nothing has been mutated
  During execution: the server owns the DDL; cancellation may not be immediate
  After success:    forward migration only; schema changes are not data-rollback safe

RECOMMEND  native
BLOCKERS   0
RESULT     ready (nothing has been changed)
```

The same table, with a change that needs a rebuild:

```
$ saksama plan --live ... -a "MODIFY COLUMN status varchar(64) NOT NULL"

  BLOCKER [R3] copy-based migration is blocked for tables with foreign keys
          orders has 2 foreign key constraint(s) in or out
          action: use a verified native operation, or run a reviewed maintenance
                  procedure; Saksama Core v1 will not drop and recreate
                  constraints automatically
RECOMMEND  none
BLOCKERS   1
RESULT     refused
```

**That refusal is the product.** Most wrappers would happily drop both constraints, copy the table, and recreate them — silently losing `ON DELETE CASCADE`.

## Install

```bash
go install github.com/perogeremmer/saksama-core/cmd/saksama@latest
```

Or build a static binary:

```bash
git clone git@github.com:perogeremmer/saksama-core.git
cd saksama-core
make build          # -> bin/saksama
./bin/saksama version
```

Single static binary, no CGO, no runtime dependencies. Linux/macOS amd64/arm64.

## Usage

```
saksama plan     --ddl <file|-> ...        offline plan (CI gate, no database)
saksama plan --live ...                    plan against a server, records a session
saksama advise   --ddl|--live ...          which engine fits: native vs gh-ost vs pt-osc
saksama apply    ...                       execute a native migration
saksama status   [session]                 list or inspect sessions
saksama events   <session>                 append-only JSONL event log
saksama export   <session>                 full audit export
saksama cleanup  <session>                 refuses to guess ownership
saksama schema                             JSON Schema of the plan contract
saksama version
```

### 1. Offline plan — safe in CI, no database access

```bash
mysql -N -e "SHOW CREATE TABLE shop.orders\G" > orders.sql

saksama plan --ddl orders.sql \
             --alter "ADD INDEX ix_created (created_at)" \
             --mysql-version 8.0.36 --host db.prod --production \
             --rows 59000000 --data-bytes 18000000000 --index-bytes 4000000000

# Machine-readable gate
saksama plan --ddl orders.sql --alter "..." --mysql-version 8.0.36 --json \
  | jq '.recommendation.engine, .recommendation.blockers'
```

Exit codes are part of the contract: **`0`** plan produced with no blockers, **`1`** plan produced **with** blockers, **`2`** usage error, **`3`** execution or internal failure. Exit `1` is a usable CI gate.

### 2. Live plan and apply

```bash
saksama plan --live -h db.prod -u deployer -d shop -t orders \
             -a "ADD INDEX ix_paid (paid_at)"

saksama apply -h db.prod -u deployer -d shop -t orders \
              -a "ADD INDEX ix_paid (paid_at)" \
              --production --confirm db.prod
```

Every execution is recorded: plan, exact statements with `ALGORITHM`/`LOCK`, timings, approvals, and verification results.

### 3. Audit

```bash
saksama status                      # all sessions
saksama status s3 --json            # one session, machine-readable
saksama events s3                   # JSONL, seq-numbered
saksama export s3                   # session + events + completeness marker
```

### Credentials

Never in argv. Resolution order:

1. `--defaults-extra-file <path>`
2. `~/.my.cnf` / `$MYSQL_HOME/my.cnf` (`[client]` and `[mysql]` sections)
3. environment (`MYSQL_HOST`, `MYSQL_TCP_PORT`, `MYSQL_USER`, `MYSQL_PWD`, `MYSQL_UNIX_PORT`, `MYSQL_DATABASE`)
4. flags
5. `--ask-pass` interactive prompt

A credential file readable by other users is **refused**, not warned about:

```
$ saksama plan --live ...
~/.my.cnf contains a password but is readable by others (mode 0o644).
Fix it first: chmod 600 /home/you/.my.cnf
```

## What it will not do

Stated plainly, because a safety tool that overpromises is worse than no tool:

- **No silent algorithm downgrade.** Every statement carries explicit `ALGORITHM`/`LOCK`. If MySQL would have to pick something more expensive, the run fails instead.
- **No foreign key automation.** Dropping and recreating constraints opens an integrity window this version cannot make deterministic. It refuses.
- **No rollback claim after cutover.** After a swap, new writes live in the new table. The tool says "forward migration", not "just roll back".
- **No guessing.** An `ALTER` the parser does not fully understand becomes a blocker, never a "probably fine".
- **No cleanup by name pattern.** Objects are only dropped when the session records ownership of them.

## Design commitments

1. **Analyzer is a pure function.** `(schema, ops, server, liveState) → Plan`. No database handle, so it is unit-testable and usable offline.
2. **Unknown is blocked.** Including the degenerate case: a plan with zero recognised operations can never report "ready".
3. **Native first.** `ADD INDEX` is `INPLACE, LOCK=NONE`; `ADD COLUMN` can be `INSTANT`. Copying a table for a secondary index is the expensive mistake this tool exists to prevent.
4. **Referential actions come from structured metadata**, never a regex over `SHOW CREATE TABLE`. Losing `CASCADE` changes data semantics silently.
5. **Never parse engine output for progress.** Poll the database; engine stderr is for diagnosis only.
6. **No single duration number.** Always a range, a confidence label, and the basis.
7. **Secrets never become events.** Redaction is enforced in the event layer, not at each call site.
8. **Fail closed on every path.** A refusal is a feature.

## Engine ladder

```
1. native    MySQL DDL with verified capability for the target version
2. gh-ost    rebuild, single-column key, no foreign keys (MIT)
3. pt-osc    composite keys, foreign keys, legacy servers (GPL-2, never bundled)
4. refuse    no engine satisfies the safety policy
```

All three are executable. The analyzer walks this ladder, stops at the first engine whose requirements all pass, and applies your recorded choice on top.

MySQL Clone Plugin is deliberately **not** in the ladder: it clones an instance, not a table.

## Supported today

| Area | State |
|---|---|
| `ALTER` parser (fail-closed subset, 19 ops) | ✅ |
| `SHOW CREATE TABLE` parser incl. FK actions | ✅ |
| Version-gated capability matrix | ✅ |
| Checks catalog (A/B/C/D/E/R, 34 rules) | ✅ |
| Engine suitability matrix with evidence, pros and cons (`advise`) | ✅ |
| Operator-chosen engine, recorded and honoured (`engine set/show/list/clear`) | ✅ |
| gh-ost adapter (argv, subprocess, DB-polled progress, verification) | ✅ |
| pt-osc adapter (same, with `--defaults-file` so no password in argv) | ✅ |
| Installed-binary probe with version detection | ✅ |
| Offline plan, plan card, `--json`, exit codes | ✅ |
| Live introspection (`information_schema`, `performance_schema`) | ✅ |
| Native execution with explicit `ALGORITHM`/`LOCK` | ✅ |
| Session state, table lock, atomic persistence | ✅ |
| Event contract, redaction, audit export | ✅ |
| Verified post-execution checks via `information_schema` | ✅ |
| Cut-over pause and cancel for copy engines | 🚧 phase 3 |
| TUI (Bubble Tea) | 🚧 phase 4 |

Commands that are not implemented yet (`cutover`, `cancel`, `recover`, `daemon`, `attach`) exit `2` with an explicit message. They are never stubbed into something that looks like it works.

Engine credentials: pt-osc reads a temporary `0600` option file, so its password never reaches argv. gh-ost has no option-file support, so a password must go in argv — that is refused unless you pass `--allow-argv-password`, because a password in argv is visible in `ps`.

## Documentation

- [`PRD.md`](PRD.md) — requirements, personas, acceptance criteria, non-goals
- [`DESIGN.md`](DESIGN.md) — architecture, engine comparison, roadmap
- [`docs/DECISIONS.md`](docs/DECISIONS.md) — decision log with the alternatives that were rejected
- [`docs/CHECKS.md`](docs/CHECKS.md) — every check: ID, severity, SQL source, MySQL error code
- [`docs/EVENTS.md`](docs/EVENTS.md) — daemon event contract
- [`docs/RESULTS.md`](docs/RESULTS.md) — what was built, what the tests proved, which bugs they caught
- [`docs/STATUS.md`](docs/STATUS.md) — implementation ledger
- [`SECURITY.md`](SECURITY.md) — threat model and reporting policy

## Development

```bash
make test               # 165 unit tests
make lint               # go vet + gofmt check
make cover              # per-package coverage
make demo               # two plan cards side by side, no database needed

make integration-up     # disposable MySQL 8.0 in docker on :33306
make test-integration   # 24 tests against a real server
make integration-down
```

Integration tests run against a real MySQL because a claim like "`ADD COLUMN` is INSTANT on 8.0.12+" is either verified or it is marketing. They have already caught six real bugs, including one that made every apply on a foreign-key table report a false verification failure, and one where the plan card read "ready" for an engine this build cannot run.

### Contribution rules

1. **Every check needs a positive test and a negative test.** A rule that fires on healthy schemas is worse than no rule.
2. **No behaviour that is not tested against real MySQL** if it claims a capability.
3. **Never widen an engine capability without a versioned integration fixture.**
4. **The analyzer stays pure.** Database access belongs in `internal/introspect`.

## License

MIT — see [LICENSE](LICENSE).

gh-ost may be embedded (MIT, keep its license). `pt-online-schema-change` is GPL-2 and is **never** bundled; it will only ever be invoked as an external binary, which keeps this project MIT.

## Origin

Built from a hand-written `migrate.sh` wrapper that did the FK dance by hand — and quietly dropped `ON DELETE CASCADE` while doing it. The audit of that script is in [`DESIGN.md` §14](DESIGN.md); every item in it became a rule in this tool.
