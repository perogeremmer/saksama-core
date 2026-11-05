// Package support declares what has actually been verified, as data.
//
// The compatibility document is generated from this package, so the matrix cannot
// drift away from what the tests and the released code actually do. If a row says
// "verified", something in this repository ran it.
package support

// Status is how much evidence backs a row.
type Status string

const (
	// Verified means a test in this repository ran against it.
	Verified Status = "verified"
	// Claimed means the rules encode it (version gates, capability matrix) but no
	// test in this repository has exercised it end to end.
	Claimed Status = "claimed"
	// Unverified means the code path exists and is unit tested, but no real binary
	// or server has been used.
	Unverified Status = "unverified"
	// Unsupported means the tool refuses it on purpose.
	Unsupported Status = "unsupported"
)

func (s Status) Mark() string {
	switch s {
	case Verified:
		return "yes"
	case Claimed:
		return "claimed"
	case Unverified:
		return "not yet"
	}
	return "no"
}

// MySQLRow is one server version.
type MySQLRow struct {
	Version string `json:"version"`
	Status  Status `json:"status"`
	// Native is what the capability matrix claims for native DDL on this version.
	Native string `json:"native"`
	Detail string `json:"detail"`
}

// EngineRow is one copy engine at one version.
type EngineRow struct {
	Engine  string `json:"engine"`
	Version string `json:"version"`
	Status  Status `json:"status"`
	Detail  string `json:"detail"`
}

// MySQL is the server matrix, newest first.
var MySQL = []MySQLRow{
	{
		Version: "8.4.x", Status: Verified,
		Native: "INSTANT add/drop column, INPLACE index",
		// Deliberately no test count: it was hand-maintained and went stale the moment
		// the suite grew. What a reader needs is what ran, not a number that rots.
		Detail: "8.4.13 via mysql:8.4 container: the full integration suite passes, including a real copy-engine migration",
	},
	{
		Version: "8.0.x", Status: Verified,
		Native: "INSTANT add column (8.0.12+), INSTANT drop column (8.0.29+), INPLACE index",
		Detail: "8.0.46 via mysql:8.0 container: the full integration suite passes, including a real gh-ost migration and the INSTANT boundary test",
	},
	{
		Version: "8.0.29+", Status: Verified,
		Native: "INSTANT add column in any position, INSTANT drop column",
		Detail: "integration test against mysql:8.0.29 ran ADD COLUMN ... AFTER and DROP COLUMN with ALGORITHM=INSTANT and the server accepted both; the same test against mysql:8.0.28 sees the server refuse them",
	},
	{
		Version: "8.0.12 - 8.0.28", Status: Verified,
		Native: "INSTANT add column, appended only",
		Detail: "integration test against mysql:8.0.28: an appended ADD COLUMN ran with ALGORITHM=INSTANT, while ADD COLUMN ... AFTER and DROP COLUMN were refused by the server, which is exactly what R-ADD-COLUMN-MIDDLE-COPY and R-DROP-COLUMN-COPY predict",
	},
	{
		Version: "5.7.44", Status: Verified,
		Native: "INPLACE add/drop index; every column change is a rebuild",
		Detail: "the full integration suite passes, except the cases that assert INSTANT behaviour and skip themselves there; gh-ost 1.1.11 performs a real ADD COLUMN on a table with rows, and pt-osc 3.7.2 runs its own cases",
	},
	{
		Version: "5.6.51", Status: Verified,
		Native: "INPLACE index, no INSTANT column",
		Detail: "integration suite against mysql:5.6 with gh-ost 1.1.11 performing real migrations. Every fact the matrix needs is present. One caveat no version number hides: performance_schema.metadata_locks arrived in 5.7, so the lock queue cannot be read here; every plan carries E5b, and a production run blocks until --accept-unknown-mdl says the risk is understood",
	},
	{
		Version: "MariaDB 10.11", Status: Unsupported,
		Native: "refused",
		Detail: "check E1 blocks every plan; verified against mariadb:10.11 (plan runs, then refuses with a clear error)",
	},
}

// Engines is the engine matrix.
var Engines = []EngineRow{
	{
		Engine: "native", Version: "8.0.46, 8.4.13", Status: Verified,
		Detail: "ADD INDEX, DROP INDEX, ADD COLUMN, DROP COLUMN executed with explicit ALGORITHM and LOCK, then verified through information_schema",
	},
	{
		Engine: "gh-ost", Version: "1.1.11", Status: Verified,
		Detail: "real migrations through the wrapper: ADD COLUMN on a table with rows on 8.0.46 and on 5.7.44, plus the flag probe (CI job engine-flag-matrix) checking every emitted flag against the binary with --check-flag",
	},
	{
		Engine: "gh-ost", Version: "1.1.2 - 1.1.7", Status: Claimed,
		Detail: "the flags the wrapper emits by default all exist; --skip-port-validation (added 1.1.8) is the only gap, and it is emitted only when connecting through a mapped port",
	},
	{
		Engine: "gh-ost", Version: "older than 1.1.2", Status: Unsupported,
		Detail: "--skip-port-validation is absent whenever the port is mapped; the flag probe refuses the run",
	},
	{
		Engine: "pt-osc", Version: "3.7.2", Status: Verified,
		Detail: "real migrations: ADD COLUMN on a plain table, a table with a foreign key (ON DELETE CASCADE preserved), and a composite primary key that gh-ost cannot chunk. Rows and constraints verified afterwards",
	},
	{
		Engine: "pt-osc", Version: "3.0.13", Status: Verified,
		Detail: "same suite; behavioural difference worth knowing: some builds keep an underscore prefix on foreign key constraint names, so Saksama verifies constraints by columns and referenced table rather than by name",
	},
	{
		Engine: "pt-osc", Version: "2.x", Status: Unsupported,
		Detail: "no --resume, narrower ALTER grammar; percona-toolkit 3.x is required",
	},
}

// Findings are hard-won facts that cost someone an afternoon. They belong in the
// document because the next person will hit them too.
// ServerSettings are the variables `saksama advise` reads before recommending a
// copy engine. They are listed here because "which engine suits my table" and
// "can my server start that engine" are different questions with different fixes.
var ServerSettings = []string{
	"log_bin: ON. gh-ost follows writes through the binary log; without it there is nothing to follow.",
	"binlog_format: ROW. Statement-based logging cannot reproduce row images.",
	"binlog_row_image: FULL. MINIMAL omits the column data gh-ost has to replay.",
	"server_id: any non-zero value. gh-ost refuses server_id=0.",
	"read_only / super_read_only: OFF. A read-only server rejects every migration.",
	"performance_schema: ON. Without it the metadata-lock check cannot see other sessions, so the main outage cause goes undetected.",
	"transaction_isolation: REPEATABLE-READ or lower. SERIALIZABLE widens locking during a copy.",
	"innodb_file_per_table: ON. With it OFF, dropping a table never returns disk to the filesystem, so a rebuild does not free space.",
	"sql_require_primary_key: OFF. When ON, DDL on a keyless table is rejected outright.",
	"lower_case_table_names: whatever it is, keep host and server consistent; a mismatch breaks table naming in subtle ways.",
}

var Findings = []string{
	"gh-ost validates the connection port against @@port and refuses to start when they differ. " +
		"Any proxy, port mapping, or NAT triggers this. Saksama reads @@port and adds --skip-port-validation automatically when it sees the mismatch.",
	"gh-ost needs log_bin=ON, binlog_format=ROW, and binlog_row_image=FULL. Without them it stops at startup, not halfway through a copy.",
	"gh-ost has no option-file support, so a password can only reach it through argv, which is visible in ps. " +
		"Saksama refuses that by default; --allow-argv-password is the explicit opt-in.",
	"pt-online-schema-change is a single Perl file. The toolkit package is not needed: install perl with DBI and DBD::mysql, then fetch bin/pt-online-schema-change from the Percona Toolkit repository.",
	"pt-online-schema-change swaps as soon as the copy finishes. There is no built-in gate to verify first, unlike gh-ost's cut-over pause.",
	"pt-online-schema-change has no --max-flow-control-on option; it throttles on replica lag with --max-lag. That flag belongs to gh-ost, and passing it makes pt-osc exit with an options error before doing anything.",
	"pt-online-schema-change writes the copy into _table_new and removes it afterwards, along with its triggers. gh-ost keeps the pre-migration table as _table_del, which is real disk usage some time later.",
	"Constraint names are not guaranteed to survive a copy. Saksama verifies a foreign key by its columns, referenced table, and actions, and reports the name it actually found.",
	"Algolia's ALTER is rejected by MySQL when ALGORITHM=INSTANT is combined with LOCK=... (error 1221). " +
		"INSTANT is metadata-only, so Saksama emits it without a LOCK clause.",
	"ALTER TABLE ... SET foreign_key_checks=0 is not available on Aurora, so a fast constraint recreate is not portable there.",
	"information_schema.TABLE_ROWS is an estimate on InnoDB and is used only for progress and sizing, never for a correctness decision.",
}

// Postgres is the forward-looking section. It is here so the document has a place
// to grow rather than being rewritten when someone starts on it.
var Postgres = []string{
	"Not supported yet. Nothing in this repository has run against PostgreSQL.",
	"PostgreSQL has no online schema change for a table rewrite: CREATE INDEX CONCURRENTLY covers indexes, " +
		"but ALTER COLUMN TYPE rewrites the table under an ACCESS EXCLUSIVE lock.",
	"pg_repack and pg_squeeze are the closest equivalents to a copy engine.",
	"The analyzer is engine-agnostic by design (a pure function over a schema model), but the introspection " +
		"layer and the capability rules are MySQL-specific today.",
	"Anything contributed for PostgreSQL should follow the same rule as everything else here: no capability claim without a test that runs it.",
}
