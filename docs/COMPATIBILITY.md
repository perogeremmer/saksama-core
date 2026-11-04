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
| 8.4.x | yes | INSTANT add/drop column, INPLACE index | 8.4.13 via mysql:8.4 container: 24/24 integration tests pass |
| 8.0.x | yes | INSTANT add column (8.0.12+), INSTANT drop column (8.0.29+), INPLACE index | 8.0.46 via mysql:8.0 container: 24/24 integration tests pass, including a real gh-ost migration |
| 8.0.29+ | yes | INSTANT drop column | rule R-DROP-COLUMN-INSTANT-8029, covered by unit tests for both sides of the boundary |
| 8.0.12 - 8.0.28 | claimed | INSTANT add column, appended only | rule R-ADD-COLUMN-INSTANT-8012; unit tested, not run against a server this old |
| 5.7.x | claimed | INPLACE index only; every column change is a rebuild | needs gh-ost 1.x or pt-osc for a rebuild; the integration suite targets 8.0+ and has not run here |
| 5.6.x | claimed | INPLACE index; no INSTANT at all | pt-osc 2.x era; plan only, never executed by this repository |
| MariaDB (any) | no | refused | capability matrix is not verified for MariaDB, so check E1 blocks every plan |

Native DDL is the cheapest path and the one Saksama prefers. It needs neither a copy
engine nor extra disk, but it still takes the metadata lock at the end, so a long
statement can queue the whole server.

## Engines

| Engine | Version | Verified | Detail |
|---|---|---|---|
| native | 8.0.46, 8.4.13 | yes | ADD INDEX, DROP INDEX, ADD COLUMN, DROP COLUMN executed with explicit ALGORITHM and LOCK, then verified through information_schema |
| gh-ost | 1.1.11 | yes | real migration: ADD COLUMN on a table with rows, --allow-on-master, copy plus cut-over plus verification |
| gh-ost | 1.1.x (older) | claimed | the wrapper only uses flags that exist across 1.1.x, but older builds have not been run here |
| pt-osc | 3.x | not yet | adapter is unit tested against a fake binary (argv, defaults-file, defaults to --print); no real pt-online-schema-change has been run by this repository |
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
- pt-online-schema-change needs Perl plus DBI and DBD::mysql. A container without those cannot run it, which is why the pt-osc row above is unverified here.
- pt-online-schema-change swaps as soon as the copy finishes. There is no built-in gate to verify first, unlike gh-ost's cut-over pause.
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
