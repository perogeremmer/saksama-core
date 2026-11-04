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
		Detail: "8.4.13 via mysql:8.4 container: 24/24 integration tests pass",
	},
	{
		Version: "8.0.x", Status: Verified,
		Native: "INSTANT add column (8.0.12+), INSTANT drop column (8.0.29+), INPLACE index",
		Detail: "8.0.46 via mysql:8.0 container: 24/24 integration tests pass, including a real gh-ost migration",
	},
	{
		Version: "8.0.29+", Status: Verified,
		Native: "INSTANT drop column",
		Detail: "rule R-DROP-COLUMN-INSTANT-8029, covered by unit tests for both sides of the boundary",
	},
	{
		Version: "8.0.12 - 8.0.28", Status: Claimed,
		Native: "INSTANT add column, appended only",
		Detail: "rule R-ADD-COLUMN-INSTANT-8012; unit tested, not run against a server this old",
	},
	{
		Version: "5.7.x", Status: Claimed,
		Native: "INPLACE index only; every column change is a rebuild",
		Detail: "needs gh-ost 1.x or pt-osc for a rebuild; the integration suite targets 8.0+ and has not run here",
	},
	{
		Version: "5.6.x", Status: Claimed,
		Native: "INPLACE index; no INSTANT at all",
		Detail: "pt-osc 2.x era; plan only, never executed by this repository",
	},
	{
		Version: "MariaDB (any)", Status: Unsupported,
		Native: "refused",
		Detail: "capability matrix is not verified for MariaDB, so check E1 blocks every plan",
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
		Detail: "real migration: ADD COLUMN on a table with rows, --allow-on-master, copy plus cut-over plus verification",
	},
	{
		Engine: "gh-ost", Version: "1.1.x (older)", Status: Claimed,
		Detail: "the wrapper only uses flags that exist across 1.1.x, but older builds have not been run here",
	},
	{
		Engine: "pt-osc", Version: "3.x", Status: Unverified,
		Detail: "adapter is unit tested against a fake binary (argv, defaults-file, defaults to --print); no real pt-online-schema-change has been run by this repository",
	},
	{
		Engine: "pt-osc", Version: "2.x", Status: Unsupported,
		Detail: "no --resume, narrower ALTER grammar; percona-toolkit 3.x is required",
	},
}

// Findings are hard-won facts that cost someone an afternoon. They belong in the
// document because the next person will hit them too.
var Findings = []string{
	"gh-ost validates the connection port against @@port and refuses to start when they differ. " +
		"Any proxy, port mapping, or NAT triggers this. Saksama reads @@port and adds --skip-port-validation automatically when it sees the mismatch.",
	"gh-ost needs log_bin=ON, binlog_format=ROW, and binlog_row_image=FULL. Without them it stops at startup, not halfway through a copy.",
	"gh-ost has no option-file support, so a password can only reach it through argv, which is visible in ps. " +
		"Saksama refuses that by default; --allow-argv-password is the explicit opt-in.",
	"pt-online-schema-change needs Perl plus DBI and DBD::mysql. A container without those cannot run it, which is why the pt-osc row above is unverified here.",
	"pt-online-schema-change swaps as soon as the copy finishes. There is no built-in gate to verify first, unlike gh-ost's cut-over pause.",
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
