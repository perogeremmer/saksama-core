<div align="center">

# Saksama Core

**Examine before you alter.**

Safe MySQL schema migrations for teams with one database, no DBA, and no budget
for blue-green.

[![CI](https://github.com/perogeremmer/saksama-core/actions/workflows/ci.yml/badge.svg)](https://github.com/perogeremmer/saksama-core/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.24+-00ADD8.svg)](go.mod)

</div>

Saksama is Indonesian for meticulous, careful. The standard spelling *seksama* is
equally correct.

---

## The problem

You have one MySQL instance. No replica. No DBA. A 59-million-row table, and a
change you have postponed for three months because you do not know whether
`ALTER TABLE` will lock it for two minutes or two hours.

Blue-green would solve it, and blue-green is expensive in *engineering time*: dual
writes, two deploy cycles, traffic switching, a rollback plan. That is the part
small teams cannot afford, not the server.

So the need is narrower than blue-green: **online schema change on one instance,
done correctly.**

The copy engines already exist and they are good. What is missing is the layer
above them:

| Missing layer | What goes wrong without it |
|---|---|
| **Analysis** | the migration fails halfway: FK charset mismatch (3780), key too long for `utf8mb4` (1071), a column used by a foreign key (1832), TEXT indexed without a prefix (1170), or a metadata lock queue that turns one slow query into an outage |
| **Routing** | a 59M-row table is fully copied for a secondary index that MySQL can build in place with `LOCK=NONE` |
| **Server checks** | gh-ost refuses to start because binary logging is off, and you find out after planning the whole change |
| **Guardrails** | the tool does something destructive that it cannot undo |
| **Honest recovery** | someone promises "just roll back" for a phase where that silently discards writes |

## What it is

A router and supervisor, not another copy engine. It reads the schema and the
server, decides which path is cheapest and safest, shows you the evidence and the
trade-offs, and then runs the engine you chose.

- **Native first.** `ADD INDEX` is `INPLACE, LOCK=NONE`. `ADD COLUMN` can be
  `INSTANT`. Copying a table for a secondary index is the mistake this tool exists
  to prevent.
- **gh-ost and pt-osc when a rebuild is needed**, chosen by what the table and the
  server actually support.
- **You decide.** `saksama engine set` records your choice, and the tool executes
  it. The recommendation never overrides you silently.
- **Verified, not assumed.** The result is checked against `information_schema`.
  An engine exit code is not evidence.
- **Disk before anything.** Below 10 percent free is refused, a copy needs twice
  the table size, and an unmeasurable volume refuses `apply` rather than guessing.

## Quick start

```bash
go install github.com/perogeremmer/saksama-core/cmd/saksama@latest
```

Or take a static binary from the [releases page](https://github.com/perogeremmer/saksama-core/releases):
Linux and macOS, amd64 and arm64, with checksums and an SBOM.

Ask first. This only reads:

```bash
saksama advise --live -h db.prod -d shop
```

```
SERVER SETTINGS (what the engines require, against what this server reports)
+---------------------+------------+----------+--------+-----------+----------------------------------+
| SETTING             | CURRENT    | REQUIRED | STATUS | NEEDED BY | WHY                              |
+---------------------+------------+----------+--------+-----------+----------------------------------+
| log_bin             | ON         | ON       | ok     | gh-ost    | gh-ost follows writes through it |
| binlog_format       | ROW        | ROW      | ok     | gh-ost    | statement format loses row data  |
| binlog_row_image    | FULL       | FULL     | ok     | gh-ost    | MINIMAL omits needed columns     |
| server_id           | 1          | non-zero | ok     | gh-ost    | gh-ost refuses server_id=0       |
| read_only           | OFF        | OFF      | ok     | all       | read-only rejects every write    |
+---------------------+------------+----------+--------+-----------+----------------------------------+

ENGINE SUITABILITY for schema shop
5 table(s), sorted by size.

+------------+--------+---------+----------------------+-----+--------+--------+--------------+
| TABLE      |   ROWS |    SIZE | PRIMARY KEY          |  FK | GH-OST | PT-OSC | REBUILD WITH |
+------------+--------+---------+----------------------+-----+--------+--------+--------------+
| m2m        | 180.0M | 44.0 GB | composite(a_id,b_id) | 0/0 | no     | yes    | pt-osc       |
| audit_log  | 180.0M | 44.0 GB | (none)               | 0/0 | no     | no     | none         |
| orders     |  59.0M | 20.5 GB | id bigint unsigned   | 1/0 | no     | yes    | pt-osc       |
| sessions   |   2.1M |  640 MB | uuid char(36)        | 0/0 | yes    | yes    | gh-ost       |
| users      | 410.0k |   88 MB | id bigint unsigned   | 0/0 | yes    | yes    | gh-ost       |
+------------+--------+---------+----------------------+-----+--------+--------+--------------+

TABLES NO ENGINE CAN COPY
  audit_log
    - no primary key or unique key to chunk by
```

Then decide, and apply:

```bash
saksama engine set pt-osc -d shop -t orders --note "foreign keys must be copied"
saksama apply -h db.prod -d shop -t orders -a "MODIFY COLUMN status varchar(64) NOT NULL"
```

```
  1. pt-online-schema-change \
  --alter "MODIFY COLUMN status varchar (64) NOT NULL" \
  --chunk-size 1000 --max-load Threads_running=50 --critical-load Threads_running=100 \
  --print D=shop,t=orders,h=db.prod,P=3306,u=deployer

VERIFICATION
  [ok  ] column_exists        status  column status present
  [ok  ] foreign_key_present  fk_orders_spot  constraint fk_orders_spot intact
```

Every command, flag, and exit code: **[COMMANDS.md](COMMANDS.md)**.

## What it will not do

A safety tool that overpromises is worse than no tool, so this is explicit:

- **No silent algorithm downgrade.** Every statement carries `ALGORITHM` and
  `LOCK`. If MySQL would have to pick something more expensive, the run fails.
- **No foreign key automation.** Dropping and recreating constraints opens an
  integrity window this version cannot make deterministic. It refuses.
- **No rollback claim after a cutover.** After a swap, new writes live in the new
  table. The tool says "forward migration", not "just roll back".
- **No guessing.** An `ALTER` the parser does not understand becomes a blocker,
  never a "probably fine".
- **No cleanup by name pattern.** Objects are dropped only when the session
  records ownership of them.

## Design commitments

1. **The analyzer is a pure function.** `(schema, ops, server, liveState) -> Plan`.
   No database handle, so it is unit testable and usable offline in CI.
2. **Unknown is blocked**, including the degenerate case: a plan with zero
   recognised operations can never report "ready".
3. **Server settings are a second axis.** An engine can suit a table perfectly and
   still be unable to start. Unknown is not a pass.
4. **Referential actions come from structured metadata**, never a regex over
   `SHOW CREATE TABLE`. Losing `CASCADE` changes data semantics silently.
5. **Progress is polled from the database**, never parsed from engine output,
   because output formats change between versions.
6. **No single duration number.** Always a range, a confidence label, and the
   basis.
7. **Secrets never become events.** Redaction runs in the event layer, not at each
   call site.
8. **Fail closed on every path.** A refusal is a feature.

## Engine ladder

```
1. native    MySQL DDL with verified capability for the target version
2. gh-ost    rebuild, single-column key, no foreign keys, binlog configured (MIT)
3. pt-osc    composite keys, foreign keys, legacy servers (GPL-2, never bundled)
4. refuse    no engine satisfies the safety policy
```

The MySQL Clone Plugin is deliberately absent: it clones an instance, not a table.

## Status

| Area | State |
|---|---|
| `ALTER` parser, 19 operations, fail closed | done |
| `SHOW CREATE TABLE` parser including FK actions | done |
| Version gated capability matrix | done |
| Checks catalogue, 49 checks plus a 12-setting server doctor | done |
| Offline plan, plan card, JSON, exit codes | done |
| Live introspection and the server-settings doctor | done |
| Native execution with explicit algorithm and lock | done |
| gh-ost adapter, verified against 1.1.11 | done |
| pt-osc adapter, verified against 3.0.13 and 3.7.2 | done |
| Engine choice with table, database, and global scope | done |
| Session state, table lock, event log, audit export | done |
| Cutover gate, cancel, cleanup with ownership, attach, reconcile | done |
| Daemon with a socket, TUI | not built |

Commands that are not implemented exit `2` with an explicit message. They are
never stubbed into something that looks like it works.

## Compatibility, with evidence

`docs/COMPATIBILITY.md` is generated from `internal/support`, so a row cannot
claim something this repository does not do.

| | Verified |
|---|---|
| MySQL 8.0.46 and 8.4.13 | 25 integration tests each |
| gh-ost 1.1.11 | a real migration: copy, cutover, verification |
| pt-online-schema-change 3.0.13 and 3.7.2 | real migrations, including a table with `ON DELETE CASCADE` and a composite key |
| MySQL 5.6 and 5.7 | claimed by the rules, not run by the suite |
| MariaDB | refused on purpose |

## Documentation

- **[COMMANDS.md](COMMANDS.md)**: every command, flag, and exit code
- **[ARCHITECTURE.md](ARCHITECTURE.md)**: system shape, engine comparison, safety policy, roadmap
- **[docs/CHECKS.md](docs/CHECKS.md)**: every check, its ID, severity, SQL source, and the MySQL error it prevents
- **[docs/COMPATIBILITY.md](docs/COMPATIBILITY.md)**: which versions are verified, and how
- **[SECURITY.md](SECURITY.md)**: threat model and reporting policy
- **[CONTRIBUTING.md](CONTRIBUTING.md)**: how to add a check or an engine

## Development

```bash
make test               # 205 unit tests
make lint               # go vet plus a gofmt check
make cover              # per-package coverage
make demo               # two plan cards, no database needed

make integration-up     # disposable MySQL 8.0 in docker on :33306
make test-integration   # 39 tests against a real server, 4 more when pt-osc or gh-ost is installed
make integration-down

make compat-doc         # regenerate docs/COMPATIBILITY.md
```

Integration tests run against a real MySQL because a claim like "`ADD COLUMN` is
INSTANT on 8.0.12+" is either verified or it is marketing.

What the suite has caught so far, all in shipped code:

| Bug | What it would have caused |
|---|---|
| `ALGORITHM=INSTANT` sent with `LOCK=NONE` | every `ADD COLUMN` and `DROP COLUMN` failed with error 1221 while the plan said it was safe |
| a verification query with three arguments for two placeholders | every apply on an FK table reported a false failure after a successful DDL |
| preflight threshold hardcoded, ignoring the configured value | the metadata-lock hazard went undetected in a shortened window |
| `--json` ignored after a positional argument | CI pipelines received human text instead of JSON, silently |
| an empty operation list produced a "ready" plan | a green CI gate for a change nothing had understood |
| `RESULT ready` printed for an engine that cannot run | the card promised something `apply` then refused |
| the Makefile health check waited on the unix socket while tests use TCP | the whole integration suite skipped without failing, so CI was green while nothing was tested |
| `engine set gh-ost -d shop -t orders` ignored `-d`, `-t`, `--note` | the choice was stored at the wrong scope with no warning |
| gh-ost refused to start because the connection port differed from `@@port` | every proxy or port mapping failed at startup, with no explanation |
| gh-ost given `--ask-pass` with an empty password | the run blocked waiting on a terminal |

Three of those share a theme: a green result for something that was never checked.
That is the failure mode this project exists to prevent, so each has a test.

### Contribution rules

1. **Every check needs a positive test and a negative test.** A rule that fires on
   healthy schemas is worse than no rule.
2. **No behaviour that is not tested against real MySQL** if it claims a
   capability.
3. **Never widen an engine capability** without a versioned integration fixture.
4. **The analyzer stays pure.** Database access belongs in `internal/introspect`.

## License

MIT, see [LICENSE](LICENSE).

gh-ost may be embedded, since it is MIT; keep its license. `pt-online-schema-change`
is GPL-2 and is **never** bundled, only invoked as an external binary, which keeps
this project MIT.

## Origin

Built from a hand-written `migrate.sh` wrapper that did the foreign key dance by
hand, and quietly dropped `ON DELETE CASCADE` while doing it. That script became a
list of rules; the audit is in [ARCHITECTURE.md](ARCHITECTURE.md) section 14.
