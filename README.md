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

The deployment is usually one of these two, and neither is unusual:

```
   how it starts                     how it ends up
   +---------------------+           +-----------+     +-----------+
   |  app + MySQL        |           |  app      |     |  MySQL    |
   +---------------------+           +-----------+     +-----------+
```

One box carrying both, or two boxes because managed MySQL was the first bill to
go. Either way: no replica. No DBA. A 59-million-row table, and a change you have
postponed for three months because you do not know whether `ALTER TABLE` will lock
it for two minutes or two hours.

Blue-green is the answer everyone gives, and on this deployment it is not a
pattern you adopt. It is a database you provision: a second MySQL instance to pay
for and keep in sync, dual writes in the app, two deploy cycles, traffic
switching, a rollback plan. The instance is a monthly bill; the rest is paid in
salary or in features that do not ship. Neither is available, and the engineer
paying is the engineer doing everything else.

So the need is narrower than blue-green: **online schema change on the instance
you already run.** Same server, same app, one deploy. What has to be correct is
the procedure, not the topology.

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

## Requirements

What the tool needs is deliberately small, because it is meant to run on the same
one or two boxes you already have.

### The machine Saksama runs on

| | |
|---|---|
| OS | Linux or macOS, amd64 or arm64. The binary is **statically linked** (`ldd` says *not a dynamic executable*), so there is no libc version to match and nothing to install first |
| Disk | A writable home directory: the state directory defaults to `~/.local/state/saksama` (`SAKSAMA_STATE_DIR` or `--state-dir`), and installed engines go to `~/.local/share/saksama/bin` |
| Network | Outbound HTTPS **only** for `saksama engine install`. Every other command talks to your MySQL and nothing else |
| Privileges on that machine | None beyond the user you already are. Installing goes to `~/.local/share/saksama/bin`, no sudo, no `/usr/local/bin` unless you pass `--system` |
| Not required | A MySQL client, Perl, Go, a daemon, a container runtime |

### The database

| | |
|---|---|
| Server | MySQL **5.6 – 8.4**, including 5.6. The verified combinations and what was run against each are in [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md); `saksama compat --markdown` prints the same table from the binary, and `saksama compat` alone reports what is installed on this machine |
| Storage engine | InnoDB. MyISAM and anything else is refused, not "handled" |
| Not supported | MariaDB is refused by check `E1`: its capability matrix is not verified here, and guessing is not a feature. RDS and Aurora are analysed but their privilege behaviour is **claimed, not verified** |
| Replica | Not needed. Copy engines can run on the primary; gh-ost is told `--allow-on-master` when there is no replica |

### Privileges for the account Saksama connects with

Grant these to a dedicated user, not to `root`:

```sql
CREATE USER 'saksama'@'10.%' IDENTIFIED BY '...';

-- reading the schema, the load, and the metadata lock queue
GRANT SELECT, PROCESS ON *.* TO 'saksama'@'10.%';
GRANT SELECT ON performance_schema.* TO 'saksama'@'10.%';

-- the DDL itself, on the databases you migrate
GRANT ALTER, CREATE, DROP, INDEX, TRIGGER, REFERENCES
  ON `shop`.* TO 'saksama'@'10.%';

-- a copy engine reads and writes rows while it copies
GRANT SELECT, INSERT, UPDATE, DELETE ON `shop`.* TO 'saksama'@'10.%';

-- for gh-ost only: it reads the binary log
GRANT REPLICATION CLIENT, REPLICATION SLAVE ON *.* TO 'saksama'@'10.%';

-- so DDL fails fast instead of queueing behind a lock
GRANT SESSION_VARIABLES_ADMIN ON *.* TO 'saksama'@'10.%';
```

Those grants were run against a real server while this section was written: a user
with exactly this set gets `RESULT ready` on a live plan with no privilege warnings.
Three of them are checked on every live plan, and a missing one is reported by name
rather than discovered at execution time:

| Missing | Check | What it means |
|---|---|---|
| `PROCESS` | `D1c` WARN | other sessions are invisible, so the metadata-lock hazard cannot be seen |
| `SESSION_VARIABLES_ADMIN` (or `SUPER`) | `D15` WARN | the session cannot shorten `lock_wait_timeout`, so a DDL may queue instead of failing fast |
| `SELECT` on `performance_schema` | `D1b` WARN | the lock queue cannot be read on a server that has the table |

`REPLICATION CLIENT` and `REPLICATION SLAVE` are gh-ost's requirements, not something
Saksama can check for you: without them gh-ost exits at startup and the event log
says so.

### Disk

Two independent thresholds, both measured on the database host when it is reachable:

- **below 10 % free** is refused outright; **below 25 %** warns;
- a copy needs roughly **twice the table size** (the copy table plus the retained
  original), and an in-place index build needs a quarter of the index size.

If the volume cannot be measured (a managed service, or a remote host), `apply`
refuses until you pass `--free-disk` and `--disk-total`. That is on purpose: an
unmeasurable disk is not an empty one.

### Copy engines, only if a change has to rebuild

An index-only change needs no copy engine at all on MySQL 5.6+. A rebuild picks one:

| Engine | Requirement | How it gets there |
|---|---|---|
| gh-ost | one static binary, no dependencies | `saksama engine install gh-ost` |
| pt-osc | Perl with `DBI` and `DBD::mysql` | `saksama engine install pt-osc`, then the Perl modules for **your** distribution; `engine doctor` prints which package that is |

`saksama engine doctor` is the answer to "is this box ready": it checks the platform,
the binary, the version against the verified table, the Perl modules, and the server
settings, and puts the fix next to each failure.

---

## Install for production (beta)

Beta means one thing here: **the procedures are the ones that were tested, and the
limits are written down instead of discovered later.** What is verified and what is
merely claimed is published in [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md) and
printed by `saksama compat`. What the tool will not do is in
[What it will not do](#what-it-will-not-do) below.

### 1. Take a binary you can verify

No Go toolchain, no package manager, no root:

```bash
curl -fsSL https://raw.githubusercontent.com/perogeremmer/saksama-core/master/install.sh | sh
```

It works out the platform, downloads the archive for it, verifies the checksum
against the release's `checksums.txt`, and installs to `/usr/local/bin` when that is
writable or `~/.local/bin` otherwise. It refuses to install an archive it cannot
verify, and says so instead of unpacking it. Pin a version or a directory with
`SAKSAMA_VERSION=v0.4.0` and `SAKSAMA_INSTALL_DIR=...`, and point it at an internal
mirror with `SAKSAMA_BASE_URL=...` (CI runs the same script against a local mirror on
every push, including the tampered-archive and missing-checksums paths).

The long way, if you would rather check each step by hand:

```bash
VERSION=v0.4.0
curl -fsSLO https://github.com/perogeremmer/saksama-core/releases/download/$VERSION/saksama_${VERSION#v}_linux_amd64.tar.gz
curl -fsSLO https://github.com/perogeremmer/saksama-core/releases/download/$VERSION/checksums.txt
sha256sum -c checksums.txt --ignore-missing --quiet && echo verified
tar xzf saksama_${VERSION#v}_linux_amd64.tar.gz
install -m 0755 saksama ~/.local/bin/saksama   # no sudo, no system directories
saksama version
```

Use `darwin_arm64`, `linux_arm64`, or `darwin_amd64` as appropriate. The archive
holds the binary and its licence; the documents live in this repository at the tag
you downloaded, so a corrected README never means downloading again:

```bash
open https://github.com/perogeremmer/saksama-core/tree/${VERSION}   # or just read on
```

Prefer `go install github.com/perogeremmer/saksama-core/cmd/saksama@${VERSION}` if
you have a Go toolchain and would rather build it locally.

### 2. Create a dedicated database user

Use the grants above. Do not run as `root`: the tool never needs it, and `D15`/`D1c`
exist precisely because the account's limits matter.

### 3. Say where state lives

The state directory holds sessions, the event log, the table lock, and recorded
engine choices. It must survive a reboot and be shared by every terminal that will
run `status`, `attach`, `cancel`, or `cutover`:

```bash
export SAKSAMA_STATE_DIR=/var/lib/saksama   # default: ~/.local/state/saksama
mkdir -p "$SAKSAMA_STATE_DIR" && chmod 700 "$SAKSAMA_STATE_DIR"
```

`--state-dir` does the same thing per command. The directory is created `0700`,
sessions are `0600`, and nothing in it contains a password. Two cheap habits that
save an incident: keep it on local disk rather than a network mount, and let exactly
one user own it, because every terminal that will run `status`, `attach`, `cancel`,
or `cutover` has to read the same directory.

### 4. Check the box and the server before the first migration

```bash
saksama compat                     # what this build has verified
saksama engine doctor              # platform, binaries, Perl modules
saksama engine doctor gh-ost -h db.prod -u saksama    # + the server's settings
saksama engine install gh-ost      # verified build, checksum checked, version asserted
```

### 5. Read before you write

`advise` and `plan` only read. Run them on production first:

```bash
saksama advise --live -h db.prod -u saksama -d shop
saksama plan   --live -h db.prod -u saksama -d shop -t orders   -a "ADD INDEX ix_created (created_at)"
```

`plan` exits `1` when the change is refused, which makes it usable as a CI gate
without `apply` anywhere near it.

### 6. Execute, detached, with the swap gated

```bash
saksama apply --live -h db.prod -u saksama -d shop -t orders   -a "MODIFY COLUMN status varchar(64) NOT NULL"   --production --confirm db.prod   --detach --hold-cutover   --free-disk 100000000000 --disk-total 200000000000
```

`--detach` puts the engine in its own process group, so closing the terminal does not
stop it. `--hold-cutover` (gh-ost only) builds the new table and waits before
swapping: check the result, then release it in a separate, auditable step:

```bash
saksama status s3
saksama cutover s3                  # this is the point of no return
saksama cancel s3                   # before cutover only; add --force-kill if needed
saksama reconcile                   # after a crash, an OOM, or a lost terminal
saksama reconcile --retire s3       # mark a dead attempt failed and free the table
saksama cleanup s3 --dry-run        # then drop only what the session owns
```

`--production` also requires `--confirm <host>`; on MySQL 5.6 it additionally blocks
until you accept the unobservable lock queue with `--accept-unknown-mdl`. There is no
silent production path.

### 7. What to watch after the first run

| Signal | Where | What it means |
|---|---|---|
| session state | `saksama status` | `done`, `failed`, and the engine's own output |
| progress | `saksama attach <id>` | rows copied, percent, `Threads_running`, throttling, polled from the database |
| leftovers | `saksama reconcile`, `advise --live` | tables and triggers an interrupted run left behind |
| a dead attempt | `saksama reconcile --retire <id>` | the session said `running` but the engine is gone; retire marks it failed and releases the table lock, so the change can be planned again |
| audit | `saksama export <id>` | the event log as a file you can keep |

### Beta, honestly

| | |
|---|---|
| No daemon, no TUI | state is files on disk and commands; nothing supervises a migration for you |
| MariaDB refused | by design; the capability matrix is not verified there |
| RDS/Aurora | analysed, but the privilege behaviour is claimed, not verified |
| No rollback claim after a cutover | once the swap happens, writes live in the new table; the tool says "forward migration" |
| Native `ALTER` cannot be cancelled | the server owns it; the tool says so instead of pretending |

If a concrete failure is not written down here, that is a documentation bug: open an
issue with the `SHOW CREATE TABLE`, the `ALTER`, and the version.

---

## Quick start

```bash
go install github.com/perogeremmer/saksama-core/cmd/saksama@latest
```

Or take a static binary from the [releases page](https://github.com/perogeremmer/saksama-core/releases):
Linux and macOS, amd64 and arm64, with checksums and an SBOM.

A copy engine is only needed when a change has to rebuild the table. Saksama
installs the version it has actually verified, checks it against a published
digest, and refuses a build it has no evidence for:

```bash
saksama engine doctor                 can gh-ost and pt-osc run here?
saksama engine install gh-ost         verified build, this os and arch
saksama engine install pt-osc         single Perl file, pinned digest
saksama engine doctor gh-ost -h db.prod -u deployer
```

Nothing installs itself: `plan`, `advise`, and `apply` never touch a binary.

Nothing to remember? `saksama` on a terminal opens a numbered menu that calls the
same commands, prints the equivalent command line before it acts, and records a
plan only when you ask. On a pipe it prints this page instead of waiting for input.

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

VERDICT for schema shop
  index-only change   no engine, no copy: ADD INDEX = INPLACE, LOCK=NONE on MySQL 8.0.46
  if a rebuild is needed
    gh-ost   2 tables: sessions, users                      ready
    pt-osc   2 tables: m2m, orders                          ready
  no engine           1 table: audit_log  (no primary key or unique key to chunk by)

  run this next
    decide a statement   saksama advise --live -d shop -t orders -a "<ALTER>"
    record the engine    saksama engine set pt-osc -d shop -t orders

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

Every command, flag, and exit code: **[COMMANDS.md](COMMANDS.md)**. The interactive
menu is `saksama wizard` (or just `saksama`).

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
| Checks catalogue, 55 checks plus a 12-setting server doctor | done |
| Offline plan, plan card, JSON, exit codes | done |
| Live introspection and the server-settings doctor | done |
| Native execution with explicit algorithm and lock | done |
| gh-ost adapter, verified against 1.1.11 | done |
| pt-osc adapter, verified against 3.0.13 and 3.7.2 | done |
| Engine choice with table, database, and global scope | done |
| Session state, table lock, event log, audit export | done |
| Cutover gate, cancel, cleanup with ownership, attach, reconcile | done |
| Terminal menu (`saksama`, `saksama wizard`) over the same core | done |
| `engine doctor` and verified `engine install` | done |
| MySQL 5.6, with its unobservable lock queue stated in every plan | done |
| Deferred claims published, not assumed: boundary pairs, killed-engine recovery, nightly volume | done |
| Hosts verified, not assumed: the binary runs on Fedora, Arch, and Alpine, and the package advice matches each | done |
| Install without a Go toolchain: one script, checksum verified, and CI proves a tampered archive is refused | done |
| Daemon with a socket, full-screen TUI | not built |

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
make test               # unit tests, no database needed
make lint               # go vet plus a gofmt check
make cover              # per-package coverage
make demo               # two plan cards, no database needed

make integration-up     # disposable MySQL 8.0 in docker on :33306
make test-integration   # the whole suite against a real server (copy-engine cases need gh-ost or pt-osc)
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

Where the deliberate gaps are - what was left out and why - lives in
[NEXT.md](NEXT.md).

## License

MIT, see [LICENSE](LICENSE).

gh-ost may be embedded, since it is MIT; keep its license. `pt-online-schema-change`
is GPL-2 and is **never** bundled, only invoked as an external binary, which keeps
this project MIT.

## Origin

Built from a hand-written `migrate.sh` wrapper that did the foreign key dance by
hand, and quietly dropped `ON DELETE CASCADE` while doing it. That script became a
list of rules; the audit is in [ARCHITECTURE.md](ARCHITECTURE.md) section 14.
