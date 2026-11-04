# Commands

Full reference for every `saksama` command. For what the tool is and why, see the
[README](README.md). For architecture, see [ARCHITECTURE.md](ARCHITECTURE.md).

```
saksama advise    inspect the server and the schema, and say which engine fits
saksama plan      produce a plan and record it, changing nothing
saksama apply     execute the chosen engine
saksama engine    list, set, show, and clear the engine choice
saksama status    list sessions, or inspect one
saksama events    print the append-only event log of a session
saksama export    write a full audit export
saksama cleanup   remove artifacts recorded as owned by a session
saksama compat    which MySQL and engine versions are verified
saksama schema    the JSON Schema of the plan contract
saksama version   build version and contract version
```

Exit codes are part of the contract:

| Code | Meaning |
|---|---|
| `0` | success, no blockers |
| `1` | a plan was produced **with** blockers, safe to use as a CI gate |
| `2` | usage error |
| `3` | execution or internal failure |

---

## Connection options

Shared by every command that talks to MySQL.

| Flag | Meaning |
|---|---|
| `-h`, `--host <name>` | MySQL host |
| `-P`, `--port <n>` | MySQL port |
| `-u`, `--user <name>` | MySQL user |
| `-d`, `--database <name>` | database |
| `-t`, `--table <name>` | table |
| `-a`, `--alter <sql>` | the `ALTER TABLE` body |
| `--defaults-extra-file <path>` | read credentials from this option file |
| `--ask-pass` | prompt for the password when none is found |
| `--no-defaults` | ignore `~/.my.cnf` |
| `--tls <mode>` | `preferred`, `true`, `false`, or `skip-verify` |

**Credential resolution order:** `--defaults-extra-file`, then `~/.my.cnf`, then
the environment (`MYSQL_HOST`, `MYSQL_TCP_PORT`, `MYSQL_USER`, `MYSQL_PWD`,
`MYSQL_UNIX_PORT`, `MYSQL_DATABASE`), then flags, then an interactive prompt.

A password is never accepted in argv. A credential file that other users can read
is rejected with the exact `chmod` command:

```
~/.my.cnf contains a password but is readable by others (mode 0o644).
Fix it first: chmod 600 /home/you/.my.cnf
```

---

## `saksama advise`

Inspects a server, or a table definition, and reports what the engines require
against what is actually there. It never changes anything.

```
saksama advise --live -h <host> -d <db>                    every table in the schema
saksama advise --live -h <host> -d <db> -t <table>         one table, shape only
saksama advise --live -h <host> -d <db> -t <table> -a "<ALTER>"   one table and one change
saksama advise --ddl <file|-> --mysql-version <X.Y.Z>      offline, from SHOW CREATE TABLE
saksama advise --schema <file>                             offline, from dumped table JSON
```

| Flag | Meaning |
|---|---|
| `--live` | read from the server instead of a file |
| `-t`, `--table <name>` | one table; omit it to inspect the whole schema |
| `-a`, `--alter <sql>` | optional; without it the report covers table shape |
| `--ddl <path\|->` | `SHOW CREATE TABLE` output, or `-` for stdin |
| `--schema <path>` | table JSON produced by `plan --dump-schema` |
| `--mysql-version <v>` | server version, required offline |
| `--no-probe` | skip detecting installed engine binaries |
| `--plain` | borderless output, for scripts and logs |
| `--json` | machine-readable output |
| `--rows`, `--data-bytes`, `--index-bytes` | sizing facts for the offline estimate |

Output has three parts: the server settings doctor, the engine matrix for the
chosen scope, and a list of leftover artifacts from previous migrations.

With no `-t`, every table in the database is scanned in a handful of bulk queries
and listed largest first. `--json` returns the same data under
`settings`, `environment`, `tables`, and `artifacts`.

---

## `saksama plan`

Produces a plan and records it as a session. Changes nothing.

```
saksama plan --live -h <host> -d <db> -t <table> -a "<ALTER>"
saksama plan --ddl orders.sql --alter "<ALTER>" --mysql-version 8.0.36 --json
```

| Flag | Meaning |
|---|---|
| `--ddl <path\|->` | `SHOW CREATE TABLE` output, or `-` for stdin |
| `--schema <path>` | table JSON from an earlier `--dump-schema` |
| `--dump-schema` | write the parsed table JSON instead of planning |
| `--mysql-version <v>` | server version, required offline |
| `--host <name>` | label for the target, used in reporting and confirmation |
| `--engine <name>` | force `native`, `gh-ost`, or `pt-osc`; still subject to blockers |
| `--production` | mark the target as production |
| `--free-disk <bytes>` | free space on the data volume, when it cannot be measured from here |
| `--disk-total <bytes>` | volume size, so the 10 percent floor can be applied |
| `--rows`, `--data-bytes`, `--index-bytes` | sizing facts for the estimate |
| `--json` | emit the plan JSON instead of the card |

Exit code `1` means the plan exists and contains blockers. That is the intended
CI gate, so a pipeline fails on a change the tool refuses.

---

## `saksama apply`

Executes the chosen engine. For native DDL that is a plain `ALTER` with an
explicit algorithm and lock. For a copy engine it starts the process, polls the
database for progress, and verifies the result.

```
saksama apply -h <host> -d <db> -t <table> -a "<ALTER>" --dry-run
saksama apply -h <host> -d <db> -t <table> -a "<ALTER>" --production --confirm db.prod

# plan, read it, decide, then execute exactly that plan
saksama plan --live ... -a "<ALTER>"
saksama apply --session s3 --confirm db.prod
```

| Flag | Meaning |
|---|---|
| `--production` | require `--confirm <host>` before any mutation |
| `--confirm <host>` | must equal the target host name when `--production` is set |
| `--dry-run` | build the statements or argv and stop |
| `--engine <name>` | override the recorded choice |
| `--force` | downgrade selected preflight aborts; never bypasses an invariant blocker |
| `--lock-wait-timeout <n>` | session `lock_wait_timeout` in seconds, default 10 |
| `--session <id>` | execute the plan stored in that session instead of planning again |
| `--hold-cutover` | ask the engine to pause before the swap, where it supports that |
| `--free-disk <bytes>` | free space on the data volume, when it cannot be measured from here |
| `--disk-total <bytes>` | volume size, so the 10 percent floor can be applied |
| `--allow-argv-password` | let an engine without an option file take the password in argv, visible in `ps`; off by default |
| `--no-probe` | skip detecting installed engines |
| `--json` | machine-readable session |

Engine precedence: `--engine`, then the choice recorded for this table, then the
choice recorded for the database, then the global choice, then the analyzer's
recommendation. The analyzer's own pick always stays visible.

After execution the result is verified through `information_schema`. An engine
that exits `0` without doing the work is reported as a failure.

The schema is checked twice, because a plan is a statement about a shape and shapes
move:

- `apply --session` compares the schema against the fingerprint recorded in that
  plan, and refuses if anyone altered the table in between;
- every apply rereads the schema immediately before the DDL, which also catches a
  change that happened during the preflight checks.

Row counts and sizes are excluded from the fingerprint, so a busy table does not
trip it.

---

## `saksama engine`

Records the operator's decision. The analyzer recommends; nothing is decided
until you set it.

```
saksama engine list
saksama engine set gh-ost --global
saksama engine set pt-osc -d shop
saksama engine set gh-ost -d shop -t sessions --note "no foreign keys here"
saksama engine show -d shop -t orders
saksama engine clear -d shop -t orders
```

| Scope | Flags | Applies to |
|---|---|---|
| table | `-d <db> -t <table>` | one table |
| database | `-d <db>` | every table in that database |
| global | `--global` | every table on every database |

`--global` cannot be combined with `-d` or `-t`. Typing just the engine name is
the same as `--global`.

Engine names accept common spellings: `gh-ost`, `ghost`, `gh`; `pt-osc`, `pt`,
`percona`; `native`, `ddl`, `sql`.

`engine show` reports the resolved choice and where it came from (`table`,
`database`, or `global`). `engine clear` forgets one scope without touching the
others.

---

## Disk space

MySQL cannot report free filesystem space over SQL, so Saksama has three honest
states for it, and treats them differently:

| State | How it happens | `plan` | `apply` |
|---|---|---|---|
| measured | the data directory exists on this machine, so the volume is read directly | judged | judged |
| supplied | `--free-disk` and `--disk-total` were given | judged | judged |
| unknown | a managed service, a remote host, no flags | `WARN` | **refused** |

Two thresholds apply whenever the number is known:

- **below 10 percent free** on the volume is refused outright, because binlogs,
  undo, and application writes keep growing while a copy runs, and a full volume
  takes the whole server down rather than one migration;
- **below 25 percent free** warns.

A copy engine additionally needs roughly twice the table size: the copy table plus
the retained original. That is checked against the same free figure.

Refusing an execution with an unknown disk is deliberate. The two ways out are one
line each:

```bash
saksama apply ... --free-disk 500GB --disk-total 1TB
```

or run Saksama on the database host, where `@@datadir` can actually be read.
`--force` downgrades this one blocker and records that it did.

---

## `saksama status`

```
saksama status                 # table of sessions, newest first
saksama status s3              # one session: state, statements, verification
saksama status s3 --json       # the same as JSON
saksama status --plain         # borderless, for logs
```

Flags `--json` and `--plain` are accepted in any position, including after the
session id.

---

## `saksama events`

Prints the append-only JSONL event log of a session, one JSON object per line with
a monotonic `seq`. Useful for an incident channel or an audit archive.

```
saksama events s3
```

---

## `saksama export`

Writes one document containing the session, every event, and a completeness
marker. If the event log contains an unparseable line, completeness says so rather
than silently truncating.

```
saksama export s3
saksama export s3 --format json
```

---

## `saksama cleanup`

Removes artifacts recorded as owned by a session. It refuses to drop anything it
cannot prove belongs to that session, so a `_table_old` or a `pt_osc_` trigger
with no ownership record is reported, never deleted.

```
saksama cleanup s3
```

---

## `saksama compat`

Prints the declared compatibility matrix and, optionally, compares it with what is
installed locally.

```
saksama compat                          # local engines against the matrix
saksama compat -h db.prod -u app        # also read the server version
saksama compat --json                   # the matrix as data
saksama compat --markdown               # the generated document
saksama compat --no-probe               # skip local engine detection
```

Every row states its evidence: `yes` (a test in this repository ran against it),
`claimed` (the version gates encode it, but nothing here ran it), `not yet` (the
code path exists and is unit tested, but no real binary), or `no` (refused).

The document at [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md) is generated from
`internal/support` by `make compat-doc`, and a test fails when it is stale.

---

## `saksama schema`

Prints the JSON Schema of the plan contract. This is the surface for another tool
to code against.

```
saksama schema
```

---

## `saksama version`

```
$ saksama version
saksama 1.2.3 (Saksama Core, contract v1)
```

---

## Installing the copy engines

`gh-ost` is a single static binary; take it from its releases page.

`pt-online-schema-change` is a single Perl file. The toolkit package is **not**
needed, and pulling it in installs dozens of tools this project never calls:

```bash
apt-get install -y --no-install-recommends libdbi-perl libdbd-mysql-perl
curl -fsSL -o /usr/local/bin/pt-online-schema-change \
  https://raw.githubusercontent.com/percona/percona-toolkit/v3.7.2/bin/pt-online-schema-change
chmod +x /usr/local/bin/pt-online-schema-change
pt-online-schema-change --version
```

That is exactly what the CI job does, so the verified combination is the one you
can reproduce locally.

## Not implemented yet

These commands exit `2` with an explicit message rather than pretending to work:

```
saksama daemon start|stop|status
saksama cutover <session>
saksama cancel <session>
saksama recover <session>
saksama attach <session>
```

`--hold-cutover` builds the flag for engines that support it, but the stop,
cleanup, and orphan-recovery paths belong with the daemon, so a held cutover is
currently released by the engine itself.
