# Checks Catalogue

Every check the analyzer can raise, with a stable ID. IDs are part of the JSON
contract, so they are never renumbered or reused for something else.

**Severity**

| Severity | Meaning |
|---|---|
| `BLOCKER` | execution is refused |
| `WARN` | execution may continue, but the risk must be read |
| `INFO` | supporting detail |

**Status**, because a catalogue that describes checks nobody wrote is a lie:

| Status | Meaning |
|---|---|
| `done` | the analyzer raises this ID today, and a test covers it |
| `planned` | specified here, not implemented yet |
| `moved` | superseded by the engine matrix (`saksama advise`), which reports it as a per-engine requirement instead of a check |

Counts today: **50 implemented check IDs**, plus **12 server settings** in the
doctor (section S).

---

## A. Charset and collation

| ID | Check | Severity | Status | MySQL error it prevents |
|---|---|---|---|---|
| A1 | the two sides of a foreign key disagree on charset or collation | `BLOCKER` when the pair was compared; `WARN` when it could not be verified (an unreadable peer table, or no text column pair to compare) | done | `3780: Referencing column ... are incompatible`. A check that never ran affirms nothing |
| A2 | `CONVERT TO CHARACTER SET` on a table with foreign keys | `BLOCKER` | done | constraint recreation fails |
| A3 | a foreign key column's charset differs from the table default | `WARN` | done | `3780` |
| A4 | a column still uses `utf8mb3`, deprecated since 8.0.28 | `WARN` | done | deprecation, and 3-byte truncation surprises |
| A5 | index key length exceeds the InnoDB limit for the row format | `BLOCKER` | done | `1071: Specified key was too long` |
| A6 | the table uses `COMPACT` or `REDUNDANT`, so keys cap at 767 bytes | `WARN` | done | `1071` |
| A7 | a `TEXT` or `BLOB` column is indexed without a prefix length | `BLOCKER` | done | `1170: BLOB/TEXT column used in key specification without a key length` |
| A8 | an index names a column the table does not have | `BLOCKER` | done | `1072: Key column does not exist in table` |

---

## B. Index and keys

| ID | Check | Severity | Status | Note |
|---|---|---|---|---|
| B1 | the table has no primary key | `WARN`, `BLOCKER` when a rebuild is needed | done | both copy engines need a key to chunk |
| B2 | the table has no unique key at all | `WARN` | done | ditto |
| B3 | the primary key is composite | `INFO` | done | gh-ost cannot chunk it, pt-osc can |
| B4 | the primary key looks like a random UUID | `WARN` | done | range chunks scatter, so the copy is slower and its ETA unreliable |
| B5 | a unique index is planned or present while duplicates exist | `BLOCKER` | done | `1062: Duplicate entry` |
| B6 | an index uses a prefix shorter than the column | `INFO` | done | queries must filter on the same prefix |
| B7 | a foreign key column has no index as its leading column | `WARN` | done | parent deletes scan the child table |
| B8 | the table would carry more than 20 indexes | `INFO` | done | write cost, and a slower rebuild |

---

## C. Foreign keys

| ID | Check | Severity | Status | Note |
|---|---|---|---|---|
| C1 | a column used by an outgoing foreign key is modified or dropped | `BLOCKER` | done | `1832: Cannot change column, used in a foreign key constraint` |
| C2 | a column referenced by an incoming foreign key is modified or dropped | `BLOCKER` | done | `1832` |
| C3 | a foreign key carries an `ON DELETE` or `ON UPDATE` action | `INFO` | done | any recreation must preserve it |
| C4 | a copy engine is used on a table with foreign keys | | moved | gh-ost's version of this is a hard requirement in the engine matrix; pt-osc handles it |
| C5 | the table is involved in more than ten foreign keys | `WARN` | done | each is a potential integrity window |
| C6 | Aurora, where `foreign_key_checks=0` is unavailable | | planned | reported today as a managed-service note, not a distinct ID |
| C7 | a foreign key column is nullable or not | `INFO` | planned | affects `ON DELETE SET NULL` behaviour |
| C8 | the ALTER itself changes a foreign key | `BLOCKER` | done | foreign key DDL is out of scope in v1 |

---

## D. Operational risk

| ID | Check | Severity | Status | Note |
|---|---|---|---|---|
| D1 | a statement has been running longer than the threshold | `WARN` at 30s, `BLOCKER` at 300s | done | DDL queues behind the metadata lock, and every query queues behind that; the remedy prints the query that shows who holds what and who is waiting, because "resolve the lock queue" is not something a person can type |
| D1b | the metadata lock queue is already non-empty, or could not be read | `BLOCKER` when non-empty, `WARN` when unreadable | done | adding this migration would extend the outage; a queue nobody could read is not an empty queue, and on a server that has the table this usually means the account cannot see `performance_schema` | Same query as D1, and the same instruction: kill the queued DDL, not the long SELECT under it.
| D1c | the account lacks `PROCESS`, so other sessions are invisible | `WARN` | done | the D1 hazard cannot be seen |
| D2 | `lock_wait_timeout` is unbounded | `INFO` | done | the default is 86400 seconds, so a DDL can hang for a day |
| D3 | a transaction older than the threshold is open | `WARN` | done | row locks and old row versions slow the copy |
| D4 | `Threads_running` is elevated | `INFO` or `WARN` | done | throttling will engage |
| D5 | replication is stopped, or lagging | `BLOCKER` when stopped or above 30s, `WARN` above 5s | done | gh-ost reads from a replica. Stopped means either thread down - an IO thread alone ships events nobody applies - and a NULL `Seconds_Behind_Master` reads as stopped, never as zero lag |
| D6 | disk space: below 10 percent free is refused, below 25 percent warns, and a copy needs twice the table size. An unmeasured disk refuses `apply` and only warns `plan` - but only for a change that copies or rebuilds the table, or when a copy engine was chosen; `ADD INDEX` and other metadata changes are never blocked for it | `BLOCKER` or `WARN` | done | `1021: Disk full`. A change that copies nothing is never blocked for the missing figure, and when the copy only happens because a copy engine was chosen, the remedy says so and names `saksama engine clear` |
| D7 | the table already has triggers | `WARN` | done | both engines create their own |
| D8 | leftover artifacts from a previous migration | `BLOCKER` | done | a new run would collide with them |
| D9 | a `CHECK` constraint would be violated by the copy | `WARN` | planned | enforced from 8.0.16 |
| D10 | the table is partitioned | `WARN` | done | gh-ost does not support it |
| D11 | the table has generated columns | `INFO` | planned | pt-osc is the safer engine there |
| D12 | the target is marked as production | `WARN` | done | requires a hostname confirmation |
| D13 | `innodb_online_alter_log_max_size` is small for a large rebuild | `WARN` | planned | `1799: Creating temporary table exceeded max size` |
| D14 | `sql_log_bin` cannot be set, as on RDS without the right role | `WARN` | planned | the copy lands in the binlog |
| D15 | the account lacks `SUPER` or `SESSION_VARIABLES_ADMIN` | `WARN` | done | `1227`, and `lock_wait_timeout` cannot be shortened |
| D16 | `AUTO_INCREMENT` has a large gap | `INFO` | planned | informational only |
| D17 | the table is larger than 100 GB | `WARN` | done | any rebuild takes a long time |

---

## E. Capability and understanding

| ID | Check | Severity | Status | Note |
|---|---|---|---|---|
| E0 | the operation is not understood | `BLOCKER` | done | unknown is never guessed |
| E1 | the server is MariaDB | `BLOCKER` | done | the capability matrix is not verified for MariaDB |
| E2 | the storage engine is not InnoDB | `BLOCKER` | done | the online DDL rules do not apply |
| E3 | the server version cannot be parsed | `BLOCKER` | done | without a version, no capability can be claimed |
| E4 | Percona Toolkit is older than 3.0 | `WARN` | planned | no `--resume`, narrower ALTER grammar |
| E5 | MySQL is older than 5.6 | `BLOCKER` | done | gh-ost requires 5.6 or newer, and 5.5 reached end of life in 2018 |
| E5b | the metadata lock queue cannot be observed (MySQL 5.6) | `WARN`, `BLOCKER` in production | done | performance_schema.metadata_locks arrived in 5.7; a queue that cannot be read is not an empty queue |
| E6 | gh-ost is not installed | `BLOCKER` | moved | now `R3`, per engine |
| E7 | gh-ost is selected for a composite primary key | `BLOCKER` | moved | now a gh-ost requirement in the engine matrix |
| E8 | a donor or replica exists | `INFO` | planned | relevant to a future multi-instance profile |
| E9 | the InnoDB row format caps key length | `INFO` | moved | now A5 and A6 |

---

## L. Planning context

| ID | Check | Severity | Status | Note |
|---|---|---|---|---|
| L0 | the plan is offline, so live checks did not run | `INFO` | done | an offline plan is a CI gate, not an execution approval |

---

## R. Engine selection

The engine matrix decides these. Each names the engine so a fix is obvious.

| ID | Check | Severity | Status | Note |
|---|---|---|---|---|
| R1 | no engine can perform this change | `BLOCKER` | done | lists each engine's unmet requirement |
| R2 | the chosen engine cannot perform this change | `BLOCKER` | done | honour the choice, name the reason |
| R3 | the chosen engine is not installed | `BLOCKER` | done | includes the install hint |
| R4 | incoming foreign keys need an explicit strategy | `WARN` | done | pt-osc `--alter-foreign-keys-method` |
| R5 | a managed server may refuse gh-ost | `WARN` | done | RDS needs `rds_superuser` |
| R6 | there is no replica, so the copy load lands on the primary | `WARN` | done | gh-ost runs with `--allow-on-master` |
| R7 | a copy engine was chosen although native would suffice | `WARN` | done | the expensive mistake |
| R8 | the server is not configured for the chosen engine | `BLOCKER` | done | names the setting, and how to change it |
| R9 | gh-ost was passed over because the server cannot start it | `WARN` | done | explains the automatic fallback to pt-osc |
| R10 | the engine may rename foreign key constraints during the copy | `INFO` | done | some pt-osc builds keep an underscore prefix; verification matches structure, not names |
| R11 | gh-ost cannot check the metadata lock queue on this server | `WARN` | done | `performance_schema.metadata_locks` arrives in 5.7; on 5.6 gh-ost refuses to start without `--skip-metadata-lock-check`, so the wrapper passes it and the plan says the check is off |
| R12 | native will rebuild this table in place | `WARN` | done | the ALTER does not block writes, but it cannot be throttled, paused, or resumed; cancelling rolls the whole rebuild back |
| R13 | native could rebuild this table, but that needs a decision | `WARN` | done | names `--allow-native-rebuild`, so a blocked production run does not look like "install a copy engine or else" |

---

## S. Server settings (the `advise` doctor)

Read by `saksama advise --live`, and carried in the plan as `environment[]`. These
decide whether an engine can start, not whether it suits the table. Both must pass.

| ID | Setting | Required | Needed by | Status when wrong |
|---|---|---|---|---|
| S1 | `log_bin` | `ON` | gh-ost | FAIL: gh-ost follows writes through the binary log |
| S2 | `binlog_format` | `ROW` | gh-ost | FAIL: statement format cannot reproduce row images |
| S3 | `binlog_row_image` | `FULL` | gh-ost | FAIL: MINIMAL omits the columns gh-ost replays |
| S4 | `server_id` | non-zero | gh-ost | FAIL: gh-ost refuses `server_id=0` |
| S5 | `read_only` | `OFF` | all | FAIL: a read-only server rejects every migration |
| S6 | `super_read_only` | `OFF` | all | FAIL: overrides `read_only` for privileged users |
| S7 | `performance_schema` | `ON` | native, gh-ost | FAIL: the metadata-lock check goes blind |
| S8 | `transaction_isolation` | not `SERIALIZABLE` | pt-osc | FAIL: SERIALIZABLE widens locking during the copy |
| S9 | `sql_require_primary_key` | `OFF` | all | FAIL: DDL on keyless tables is rejected |
| S10 | `innodb_file_per_table` | `ON` | advisory | WARN: a rebuild never returns disk to the filesystem |
| S11 | `lower_case_table_names` | consistent with the host | advisory | INFO only |
| S12 | `gtid_mode` | either | advisory | INFO only: neither engine requires GTID |

Rules:

- **Unknown is not a pass.** An offline plan reports every row as `unknown` and
  never claims the server is ready.
- **A fixable blocker keeps the engine named.** When the chosen engine cannot
  start, the plan refuses with R8 (or R3 for a missing binary), because "change
  this, then retry" is only useful if it says which engine it means.
- **Automatic routing prefers a runnable engine.** If gh-ost suits the table but
  the server cannot run it, the recommendation moves to pt-osc and R9 explains
  the skip. An explicit `engine set` is never overridden.

---

### D6 in detail

MySQL cannot report free filesystem space over SQL, so the number comes from one
of three places, and they are never conflated:

| Source | When |
|---|---|
| `filesystem` | the server's `@@hostname` matches this machine and `@@datadir` exists here, so the volume is read with `statfs` |
| `operator` | `--free-disk` and `--disk-total` were supplied |
| unmeasured | a managed service or a remote host, with no flags |

Thresholds: **10 percent free is a hard floor** for any execution, **25 percent
warns**, and a copy engine needs **twice the table size** available. Walking up to
a parent directory to find a filesystem is forbidden: it reports the free space of
a different volume, which is worse than reporting nothing.

---

## F. Native capability by MySQL version

| Operation | 5.6 | 5.7 | 8.0.12+ | 8.0.29+ |
|---|---|---|---|---|
| `ADD INDEX`, `DROP INDEX` | INPLACE, `LOCK=NONE` | INPLACE | INPLACE | INPLACE |
| `ADD COLUMN`, appended | INPLACE, `LOCK=NONE`, rebuilds | INPLACE, `LOCK=NONE`, rebuilds | INSTANT | INSTANT |
| `ADD COLUMN` with `FIRST` or `AFTER` | INPLACE, `LOCK=NONE`, rebuilds | INPLACE, `LOCK=NONE`, rebuilds | rebuild | INSTANT |
| `DROP COLUMN` | INPLACE, `LOCK=NONE`, rebuilds | INPLACE, `LOCK=NONE`, rebuilds | rebuild | INSTANT |
| `SET DEFAULT`, `DROP DEFAULT` | INPLACE, `LOCK=NONE` | INPLACE, `LOCK=NONE` | INSTANT | INSTANT |
| `RENAME COLUMN` | rebuild | rebuild | INPLACE | INPLACE |
| `RENAME TABLE` | metadata | metadata | metadata | metadata |
| `MODIFY`, `CHANGE` widening varchar/varbinary in the same length byte | rebuild | INPLACE, `LOCK=NONE` | INPLACE, `LOCK=NONE` | INPLACE, `LOCK=NONE` |
| `MODIFY`, `CHANGE` relaxing `NOT NULL` to `NULL`, same type | INPLACE, `LOCK=NONE`, rebuilds | INPLACE, `LOCK=NONE`, rebuilds | INPLACE, `LOCK=NONE`, rebuilds | INPLACE, `LOCK=NONE`, rebuilds |
| `MODIFY`, `CHANGE` anything else: narrower, type change, or crossing the length byte | rebuild | rebuild | rebuild | rebuild |
| `ADD` or `DROP PRIMARY KEY` | rebuild | rebuild | rebuild | rebuild |
| `CONVERT TO CHARACTER SET` | rebuild | rebuild | rebuild | rebuild |
| anything unrecognised | blocked | blocked | blocked | blocked |

**Fail-safe rule:** an operation not in this table is assumed to rebuild, so it is
routed to a copy engine. It is never assumed cheap.

**"INPLACE, rebuilds" is one fact with two halves**, and they answer different
questions. `LOCK=NONE` is why the change does not need a copy engine to be safe:
writes are not blocked and nothing has to be installed. The rebuild is why it should
still use one when the table is big: MySQL cannot throttle, pause, resume, or cap
replication lag on an in-place rebuild.

So native runs a rebuild only when the table is small enough for that to be seconds
(at most 100,000 rows and 64 MiB by default, both measured facts from
`information_schema`), and in `--production` only with `--allow-native-rebuild`. Above
that, or when the size was not measured, the answer is a copy engine - which is what
`R12` and `R13` exist to say out loud.

**How these rows are known.** They are not read off documentation: `test/integration/
native_test.go` asks the server to perform the ALTER the plan predicted, and for a
rebuild it checks that the table's InnoDB tablespace id actually changed - an index
build writes as many bytes as a rebuild, so bytes alone cannot tell them apart.
`boundary_test.go` does the same for the INSTANT boundary. Two rows here used to be
wrong on 5.6 and 5.7 (default changes, and a varchar widening), and the test is what
found them. The length-byte line is utf8mb4's: `varchar(63)` is 252 bytes and stays
one length byte, `varchar(64)` is 256 and needs two, so it rebuilds.

---

## G. Plan card

```
ALTER      ADD INDEX ix_created (created_at)
TARGET     shop.orders @ db.prod
ENGINE     native [INPLACE, LOCK=NONE] (8.0.36)
DURATION   35s - 60m | confidence medium
WHY        every operation has a verified native DDL rule

FINDINGS
  WARN  [D1]  one statement has been running for 127s
              action: wait for it, or terminate it deliberately
  INFO  [A5]  key length is 48 bytes, limit is 3072

RECOVERY   before execution: nothing has been mutated
           after success: forward migration only

RECOMMEND  native
BLOCKERS   0
RESULT     ready (nothing has been changed)
```

---

## H. A remedy has to name a step

Every `action:` line in a plan is written for someone who has to act on it, not for
someone who will nod at it. "Resolve the lock queue first" is a diagnosis; the operator
needs the query that shows who holds what, and the decision that follows from it - so
that is what the line contains. Where a finding is a fact to know rather than an action
("nothing to fix before applying, but budget for it: ..."), the line says so instead of
inventing a ritual.

`internal/analyzer/remedy_test.go` reads the source and fails when a remedy names no
step: no SQL, no command, no flag, no backticked instruction, and no explicit
"nothing to fix" clause. It can only judge literal strings - a remedy assembled by a
function is reviewed where it is built - so it is a guard against the easy regression,
not a proof about every word the tool prints.

## I. Checklist for a new check

- [ ] a stable ID that is never reused
- [ ] the SQL that gathers the fact lives in `internal/introspect`
- [ ] a fixture that triggers it
- [ ] a **negative** test proving it does not fire on a healthy schema
- [ ] remediation text that says what to do, not merely what is wrong - the query, the command, or the flag, as described in [H](#h-a-remedy-has-to-name-a-step); `remedy_test.go` will fail the build if it does not
- [ ] the MySQL error number it prevents, where one exists
- [ ] this catalogue updated, with the correct status
