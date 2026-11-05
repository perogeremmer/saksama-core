# Compatibility

> Generated from `internal/support` by `make compat-doc`. Do not edit by hand;
> edit the data and regenerate, or the test will fail.

Every row states how much evidence backs it:

- **verified**: a test in this repository ran against it
- **claimed**: the version gates and capability rules encode it, but nothing here has run it end to end
- **not yet**: the code path exists and is unit tested, but no real binary or server has touched it
- **no**: refused on purpose

## MySQL

| Version | Verified | Native DDL that applies | Detail |
|---|---|---|---|
| 8.4.x | yes | INSTANT add/drop column, INPLACE index | 8.4.13 via mysql:8.4 container: the full integration suite passes, including a real copy-engine migration |
| 8.0.x | yes | INSTANT add column (8.0.12+), INSTANT drop column (8.0.29+), INPLACE index | 8.0.46 via mysql:8.0 container: the full integration suite passes, including a real gh-ost migration and the INSTANT boundary test |
| 8.0.29+ | yes | INSTANT add column in any position, INSTANT drop column | integration test against mysql:8.0.29 ran ADD COLUMN ... AFTER and DROP COLUMN with ALGORITHM=INSTANT and the server accepted both; the same test against mysql:8.0.28 sees the server refuse them |
| 8.0.12 - 8.0.28 | yes | INSTANT add column, appended only | integration test against mysql:8.0.28: an appended ADD COLUMN ran with ALGORITHM=INSTANT, while ADD COLUMN ... AFTER and DROP COLUMN were refused by the server, which is exactly what R-ADD-COLUMN-MIDDLE-COPY and R-DROP-COLUMN-COPY predict |
| 5.7.44 | yes | INPLACE add/drop index; every column change is a rebuild | the full integration suite passes, except the cases that assert INSTANT behaviour and skip themselves there; gh-ost 1.1.11 performs a real ADD COLUMN on a table with rows, and pt-osc 3.7.2 runs its own cases |
| 5.6.51 | yes | INPLACE index, no INSTANT column | integration suite against mysql:5.6 with gh-ost 1.1.11 performing real migrations. Every fact the matrix needs is present. One caveat no version number hides: performance_schema.metadata_locks arrived in 5.7, so the lock queue cannot be read here; every plan carries E5b, and a production run blocks until --accept-unknown-mdl says the risk is understood |
| MariaDB 10.11 | no | refused | check E1 blocks every plan; verified against mariadb:10.11 (plan runs, then refuses with a clear error) |

Native DDL is the cheapest path and the one Saksama prefers. It needs neither a copy
engine nor extra disk, but it still takes the metadata lock at the end, so a long
statement can queue the whole server.

## Engines

| Engine | Version | Verified | Detail |
|---|---|---|---|
| native | 8.0.46, 8.4.13 | yes | ADD INDEX, DROP INDEX, ADD COLUMN, DROP COLUMN executed with explicit ALGORITHM and LOCK, then verified through information_schema |
| gh-ost | 1.1.11 | yes | real migrations through the wrapper: ADD COLUMN on a table with rows on 8.0.46 and on 5.7.44, plus the flag probe (CI job engine-flag-matrix) checking every emitted flag against the binary with --check-flag |
| gh-ost | 1.1.2 - 1.1.7 | claimed | the flags the wrapper emits by default all exist; --skip-port-validation (added 1.1.8) is the only gap, and it is emitted only when connecting through a mapped port |
| gh-ost | older than 1.1.2 | no | --skip-port-validation is absent whenever the port is mapped; the flag probe refuses the run |
| pt-osc | 3.7.2 | yes | real migrations: ADD COLUMN on a plain table, a table with a foreign key (ON DELETE CASCADE preserved), and a composite primary key that gh-ost cannot chunk. Rows and constraints verified afterwards |
| pt-osc | 3.0.13 | yes | same suite; behavioural difference worth knowing: some builds keep an underscore prefix on foreign key constraint names, so Saksama verifies constraints by columns and referenced table rather than by name |
| pt-osc | 2.x | no | no --resume, narrower ALTER grammar; percona-toolkit 3.x is required |

## Server settings `advise` reads

A suitabile engine on a server that cannot start it is still a failed migration,
so these are checked and shown with the value the server reports right now.

- log_bin: ON. gh-ost follows writes through the binary log; without it there is nothing to follow.
- binlog_format: ROW. Statement-based logging cannot reproduce row images.
- binlog_row_image: FULL. MINIMAL omits the column data gh-ost has to replay.
- server_id: any non-zero value. gh-ost refuses server_id=0.
- read_only / super_read_only: OFF. A read-only server rejects every migration.
- performance_schema: ON. Without it the metadata-lock check cannot see other sessions, so the main outage cause goes undetected.
- transaction_isolation: REPEATABLE-READ or lower. SERIALIZABLE widens locking during a copy.
- innodb_file_per_table: ON. With it OFF, dropping a table never returns disk to the filesystem, so a rebuild does not free space.
- sql_require_primary_key: OFF. When ON, DDL on a keyless table is rejected outright.
- lower_case_table_names: whatever it is, keep host and server consistent; a mismatch breaks table naming in subtle ways.

## Findings

Facts that cost someone an afternoon. They are here so the next person does not pay twice.

- gh-ost validates the connection port against @@port and refuses to start when they differ. Any proxy, port mapping, or NAT triggers this. Saksama reads @@port and adds --skip-port-validation automatically when it sees the mismatch.
- gh-ost needs log_bin=ON, binlog_format=ROW, and binlog_row_image=FULL. Without them it stops at startup, not halfway through a copy.
- gh-ost has no option-file support, so a password can only reach it through argv, which is visible in ps. Saksama refuses that by default; --allow-argv-password is the explicit opt-in.
- pt-online-schema-change is a single Perl file. The toolkit package is not needed: install perl with DBI and DBD::mysql, then fetch bin/pt-online-schema-change from the Percona Toolkit repository.
- pt-online-schema-change swaps as soon as the copy finishes. There is no built-in gate to verify first, unlike gh-ost's cut-over pause.
- pt-online-schema-change has no --max-flow-control-on option; it throttles on replica lag with --max-lag. That flag belongs to gh-ost, and passing it makes pt-osc exit with an options error before doing anything.
- pt-online-schema-change writes the copy into _table_new and removes it afterwards, along with its triggers. gh-ost keeps the pre-migration table as _table_del, which is real disk usage some time later.
- Constraint names are not guaranteed to survive a copy. Saksama verifies a foreign key by its columns, referenced table, and actions, and reports the name it actually found.
- Algolia's ALTER is rejected by MySQL when ALGORITHM=INSTANT is combined with LOCK=... (error 1221). INSTANT is metadata-only, so Saksama emits it without a LOCK clause.
- ALTER TABLE ... SET foreign_key_checks=0 is not available on Aurora, so a fast constraint recreate is not portable there.
- information_schema.TABLE_ROWS is an estimate on InnoDB and is used only for progress and sizing, never for a correctness decision.

## What to do with an unverified combination

Nothing here promises a combination that has not been tested. If you are on one:

1. `saksama advise --live -d <db>` is safe: it only reads the schema.
2. `saksama plan --live ...` is safe: it plans and refuses, and changes nothing.
3. `saksama apply ... --dry-run` builds the exact argv or SQL and stops.
4. Then read the plan, and only then run it without `--dry-run`.

If it works, saying so is a genuine contribution: add the row to
`internal/support/support.go` and run `make compat-doc`.

## PostgreSQL

- Not supported yet. Nothing in this repository has run against PostgreSQL.
- PostgreSQL has no online schema change for a table rewrite: CREATE INDEX CONCURRENTLY covers indexes, but ALTER COLUMN TYPE rewrites the table under an ACCESS EXCLUSIVE lock.
- pg_repack and pg_squeeze are the closest equivalents to a copy engine.
- The analyzer is engine-agnostic by design (a pure function over a schema model), but the introspection layer and the capability rules are MySQL-specific today.
- Anything contributed for PostgreSQL should follow the same rule as everything else here: no capability claim without a test that runs it.
