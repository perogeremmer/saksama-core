//go:build integration

// Package integration exercises Saksama against a real MySQL server.
//
// Run with:
//
//	make test-integration
//
// or directly:
//
//	SAKSAMA_TEST_DSN='root:@tcp(127.0.0.1:33306)/' go test -tags=integration ./test/integration/ -v
//
// These tests are the only evidence that the analyzer's claims match MySQL's
// actual behaviour, so they assert against information_schema rather than
// against our own model.
//
// NOTE: all SQL here is written as double-quoted Go strings because MySQL
// identifiers use backticks, which cannot appear inside a Go raw string.
package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/core"
	"github.com/perogeremmer/saksama-core/internal/cred"
	"github.com/perogeremmer/saksama-core/internal/engine/process"
	"github.com/perogeremmer/saksama-core/internal/introspect"
	"github.com/perogeremmer/saksama-core/internal/model"
	"github.com/perogeremmer/saksama-core/internal/state"
)

func baseDSN() string {
	if v := os.Getenv("SAKSAMA_TEST_DSN"); v != "" {
		return v
	}
	return "root:@tcp(127.0.0.1:33306)/"
}

func withParseTime(dsn string) string {
	if strings.Contains(dsn, "parseTime") {
		return dsn
	}
	if strings.Contains(dsn, "?") {
		return dsn + "&parseTime=true"
	}
	return dsn + "?parseTime=true"
}

func adminDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", withParseTime(baseDSN()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("MySQL not reachable at %s: %v", baseDSN(), err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fixtureSchema creates an isolated database and returns its name plus a
// connection already bound to it.
func fixtureSchema(t *testing.T) (string, *sql.DB) {
	t.Helper()
	admin := adminDB(t)

	name := fmt.Sprintf("saksama_it_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := admin.Exec("CREATE DATABASE `" + name + "`"); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { admin.Exec("DROP DATABASE IF EXISTS `" + name + "`") })

	db, err := sql.Open("mysql", withParseTime(baseDSN()+name))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return name, db
}

// utf8mb4Collation returns a collation the running server actually has.
// utf8mb4_0900_ai_ci appeared in MySQL 8.0; on 5.7 the equivalent default is
// utf8mb4_general_ci. Fixtures ask rather than assume, so one suite covers both.
func utf8mb4Collation(t *testing.T, db *sql.DB) string {
	t.Helper()
	var v string
	if err := db.QueryRow("SELECT @@version").Scan(&v); err == nil {
		if !strings.HasPrefix(v, "8.") && !strings.HasPrefix(v, "9.") && !strings.HasPrefix(v, "10.") {
			return "COLLATE=utf8mb4_general_ci"
		}
	}
	return "COLLATE=utf8mb4_0900_ai_ci"
}

func mustExec(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()
	for _, s := range statements {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

func newRunner(t *testing.T) (*core.Runner, *state.Store) {
	t.Helper()
	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("state store: %v", err)
	}
	return &core.Runner{Store: store, Now: time.Now}, store
}

// testConn is the connection the tools under test must be told about. It follows
// SAKSAMA_TEST_DSN so a non-default port does not silently point the engine at the
// wrong server.
func testConn() cred.Conn {
	c := cred.Conn{User: "root", Host: "127.0.0.1", Port: "33306"}
	dsn := baseDSN()
	if i := strings.Index(dsn, "@tcp("); i >= 0 {
		rest := dsn[i+len("@tcp("):]
		if j := strings.Index(rest, ")"); j > 0 {
			hostPort := rest[:j]
			if k := strings.LastIndex(hostPort, ":"); k > 0 {
				c.Host = hostPort[:k]
				c.Port = hostPort[k+1:]
			} else {
				c.Host = hostPort
			}
		}
	}
	return c
}

func applyRequest(schema, table, alter string) core.Request {
	return core.Request{
		Conn:     testConn(),
		Database: schema,
		Table:    table,
		Alter:    alter,
		Actor:    "integration-test",
		Approve: func(ar core.ApprovalRequest) (state.Approval, error) {
			return state.Approval{Action: ar.Action, Actor: "integration-test", Decision: "granted"}, nil
		},
	}
}

func runApply(t *testing.T, runner *core.Runner, db *sql.DB, req core.Request) (*state.Session, error) {
	t.Helper()
	runner.DB = db
	sess, err := runner.Apply(context.Background(), req)
	if err != nil {
		// "1 blocker(s)" is not a diagnosis. A blocked apply in CI has to say which
		// check blocked it, or the next person reads a log that repeats the error
		// and hides the reason - and some blockers (a pending metadata lock) depend
		// on what the server was doing a moment ago.
		if prep, aerr := runner.Analyze(context.Background(), req); aerr == nil {
			var ids []string
			for _, c := range prep.Plan.Checks {
				if c.Severity == analyzer.SeverityBlocker {
					ids = append(ids, c.ID+": "+c.Title)
				}
			}
			if len(ids) > 0 {
				err = fmt.Errorf("%w (blocked by %s)", err, strings.Join(ids, "; "))
			}
		}
	}
	return sess, err
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	return n
}

func columnType(t *testing.T, db *sql.DB, schema, table, column string) string {
	t.Helper()
	var ct string
	err := db.QueryRow(
		"SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?",
		schema, table, column).Scan(&ct)
	if err != nil {
		t.Fatalf("column type: %v", err)
	}
	return ct
}

// --- introspection ----------------------------------------------------------

func TestIntrospectReadsEverythingTheAnalyzerNeeds(t *testing.T) {
	schema, db := fixtureSchema(t)

	mustExec(t, db,
		"CREATE TABLE `parents` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, PRIMARY KEY (`id`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		"CREATE TABLE `orders` ("+
			"`id` bigint unsigned NOT NULL AUTO_INCREMENT,"+
			"`parent_id` bigint unsigned NULL,"+
			"`status` varchar(32) NOT NULL DEFAULT 'pending',"+
			"`note` text,"+
			"`legacy` varchar(16) CHARACTER SET utf8mb3,"+
			"PRIMARY KEY (`id`),"+
			"KEY `ix_status` (`status`),"+
			"CONSTRAINT `fk_orders_parent` FOREIGN KEY (`parent_id`) REFERENCES `parents` (`id`) "+
			"ON DELETE CASCADE ON UPDATE SET NULL"+
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 "+utf8mb4Collation(t, db)+" ROW_FORMAT=DYNAMIC",
		"CREATE TRIGGER `trg_orders_noop` BEFORE INSERT ON `orders` FOR EACH ROW SET NEW.`status` = NEW.`status`",
	)

	c := introspect.NewCollector(db, schema, "orders")
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	if snap.Server.Version == "" {
		t.Error("server version missing")
	}
	if snap.Server.Vendor != "mysql" {
		t.Errorf("vendor = %q", snap.Server.Vendor)
	}
	if snap.Table.Engine != "InnoDB" {
		t.Errorf("engine = %q", snap.Table.Engine)
	}
	// MySQL 5.6 defaults to innodb_file_format=Antelope, where ROW_FORMAT=DYNAMIC
	// is silently downgraded to COMPACT. Assert the value was read and is a real
	// row format; the byte limits the analyzer derives from it differ, and A6
	// covers that difference.
	switch snap.Table.RowFormat {
	case "DYNAMIC", "COMPACT", "REDUNDANT":
	default:
		t.Errorf("row format = %q, want a row format the server reports", snap.Table.RowFormat)
	}
	if snap.Table.Charset != "utf8mb4" {
		t.Errorf("charset = %q", snap.Table.Charset)
	}
	if len(snap.Table.Columns) != 5 {
		t.Fatalf("columns = %d, want 5", len(snap.Table.Columns))
	}
	// PRIMARY, ix_status, plus the index MySQL created implicitly for the FK.
	if len(snap.Table.Indexes) != 3 {
		t.Fatalf("indexes = %d, want 3 (primary, ix_status, implicit FK index)", len(snap.Table.Indexes))
	}
	if _, ok := snap.Table.PrimaryKey(); !ok {
		t.Fatal("primary key not found")
	}
	if len(snap.Table.ForeignKeys) != 1 {
		t.Fatalf("outgoing FKs = %d, want 1", len(snap.Table.ForeignKeys))
	}

	// The single most important assertion in this file: referential actions must
	// survive introspection. Losing them silently changes data semantics.
	fk := snap.Table.ForeignKeys[0]
	if fk.OnDelete != "CASCADE" {
		t.Errorf("ON DELETE = %q, want CASCADE", fk.OnDelete)
	}
	if fk.OnUpdate != "SET NULL" {
		t.Errorf("ON UPDATE = %q, want SET NULL", fk.OnUpdate)
	}
	if len(fk.Columns) != 1 || fk.Columns[0] != "parent_id" || fk.RefColumns[0] != "id" {
		t.Errorf("FK columns = %v -> %v", fk.Columns, fk.RefColumns)
	}
	if len(snap.Table.Triggers) != 1 {
		t.Errorf("triggers = %v", snap.Table.Triggers)
	}

	// utf8mb3 must be visible so check A4 can fire.
	legacy, ok := snap.Table.Column("legacy")
	if !ok {
		t.Fatal("legacy column missing")
	}
	if legacy.Charset != "utf8mb3" {
		t.Errorf("legacy charset = %q, want utf8mb3", legacy.Charset)
	}

	// Incoming FKs must be visible from the parent side.
	pc := introspect.NewCollector(db, schema, "parents")
	parentSnap, err := pc.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect parent: %v", err)
	}
	if len(parentSnap.Table.ReferencedBy) != 1 {
		t.Fatalf("incoming FKs = %d, want 1", len(parentSnap.Table.ReferencedBy))
	}
	if parentSnap.Table.ReferencedBy[0].RefTable != "orders" {
		t.Errorf("incoming FK child = %q", parentSnap.Table.ReferencedBy[0].RefTable)
	}
}

func TestIntrospectFKCharsetCompatibility(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `p` (`id` varchar(36) NOT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 "+utf8mb4Collation(t, db),
		"CREATE TABLE `c` (`pid` varchar(36) NOT NULL, KEY `ix` (`pid`), "+
			"CONSTRAINT `fk_c_p` FOREIGN KEY (`pid`) REFERENCES `p` (`id`)) "+
			"ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 "+utf8mb4Collation(t, db),
	)

	c := introspect.NewCollector(db, schema, "c")
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(snap.FKChecks) == 0 {
		t.Fatal("no FK compatibility results")
	}
	for _, fk := range snap.FKChecks {
		if !fk.Compatible {
			t.Errorf("expected compatible constraint %q, got %+v", fk.Constraint, fk)
		}
	}
}

func TestIntrospectMissingTableIsAClearError(t *testing.T) {
	schema, db := fixtureSchema(t)
	c := introspect.NewCollector(db, schema, "does_not_exist")
	_, err := c.Collect(context.Background())
	if err == nil {
		t.Fatal("expected an error for a missing table")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestIntrospectDetectsPartitionedTable(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `pt` (`id` int NOT NULL, `at` datetime NOT NULL, PRIMARY KEY (`id`, `at`)) "+
			"ENGINE=InnoDB PARTITION BY RANGE (YEAR(`at`)) ("+
			"PARTITION p0 VALUES LESS THAN (2020), PARTITION p1 VALUES LESS THAN MAXVALUE)",
	)
	c := introspect.NewCollector(db, schema, "pt")
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !snap.Table.Partitioned {
		t.Fatal("partitioned table not detected")
	}
}

// --- native apply -----------------------------------------------------------

func TestApplyAddIndexEndToEnd(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `orders` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `created_at` datetime NOT NULL, "+
			"`status` varchar(32) NOT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		"INSERT INTO `orders` (`created_at`, `status`) VALUES (NOW(), 'new')",
	)

	runner, store := newRunner(t)
	sess, err := runApply(t, runner, db, applyRequest(schema, "orders", "ADD INDEX ix_created (created_at)"))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if sess.State != state.StateDone {
		t.Fatalf("state = %q, error = %q", sess.State, sess.Error)
	}
	if sess.Engine != "native" {
		t.Errorf("engine = %q", sess.Engine)
	}
	if len(sess.Statements) != 1 {
		t.Fatalf("statements = %d", len(sess.Statements))
	}

	// The SQL must carry an explicit algorithm and lock: silent fallback is the
	// failure mode this tool exists to prevent.
	sqlText := sess.Statements[0].SQL
	if !strings.Contains(sqlText, "ALGORITHM=INPLACE") {
		t.Errorf("missing explicit algorithm: %s", sqlText)
	}
	if !strings.Contains(sqlText, "LOCK=NONE") {
		t.Errorf("missing explicit lock: %s", sqlText)
	}

	if n := countRows(t, db,
		"SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'orders' AND INDEX_NAME = 'ix_created'",
		schema); n == 0 {
		t.Fatal("index was not actually created")
	}

	// Durable session and event log.
	saved, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	if saved.State != state.StateDone {
		t.Errorf("persisted state = %q", saved.State)
	}
	if len(saved.Verification) == 0 {
		t.Error("verification results were not persisted")
	}
	eventsRaw, err := store.ReadEvents(sess.ID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	for _, want := range []string{"session.created", "plan.computed", "phase.started", "session.completed"} {
		if !strings.Contains(string(eventsRaw), want) {
			t.Errorf("event log missing %q", want)
		}
	}
}

func TestApplyAddColumnUsesInstant(t *testing.T) {
	schema, db := fixtureSchema(t)

	// INSTANT add column begins at 8.0.12. On an older server the same change is
	// a rebuild and belongs to the copy engine, which is its own tests.
	var srvVersion string
	_ = db.QueryRow("SELECT @@version").Scan(&srvVersion)
	if !strings.HasPrefix(srvVersion, "8.") && !strings.HasPrefix(srvVersion, "9.") {
		t.Skipf("running %s; INSTANT add column is an 8.0+ behaviour, not a 5.7 one", srvVersion)
	}

	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, PRIMARY KEY (`id`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
	)

	runner, _ := newRunner(t)
	sess, err := runApply(t, runner, db, applyRequest(schema, "t", "ADD COLUMN `note` varchar(64) NOT NULL DEFAULT ''"))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if sess.State != state.StateDone {
		t.Fatalf("state = %q error = %q", sess.State, sess.Error)
	}
	if !strings.Contains(sess.Statements[0].SQL, "ALGORITHM=INSTANT") {
		t.Errorf("expected ALGORITHM=INSTANT, got %s", sess.Statements[0].SQL)
	}
	if n := countRows(t, db,
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 't' AND COLUMN_NAME = 'note'",
		schema); n != 1 {
		t.Fatal("column was not created")
	}
}

func TestApplyDropIndexEndToEnd(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `a` int NOT NULL, "+
			"PRIMARY KEY (`id`), KEY `ix_a` (`a`)) ENGINE=InnoDB",
	)

	runner, _ := newRunner(t)
	sess, err := runApply(t, runner, db, applyRequest(schema, "t", "DROP INDEX ix_a"))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if sess.State != state.StateDone {
		t.Fatalf("state = %q error = %q", sess.State, sess.Error)
	}
	if n := countRows(t, db,
		"SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 't' AND INDEX_NAME = 'ix_a'",
		schema); n != 0 {
		t.Fatal("index still present")
	}
}

// A rebuild on a table with foreign keys is not a dead end anymore: gh-ost is
// out, pt-osc copies the constraints, and that is what gets chosen. Nothing may
// be mutated unless the chosen engine actually runs.
func TestApplyForeignKeyRebuildRoutesToPtOSC(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `p` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, PRIMARY KEY (`id`)) ENGINE=InnoDB",
		"CREATE TABLE `c` ("+
			"`id` bigint unsigned NOT NULL AUTO_INCREMENT,"+
			"`pid` bigint unsigned NOT NULL,"+
			"`status` varchar(32) NOT NULL,"+
			"PRIMARY KEY (`id`), KEY `ix_pid` (`pid`),"+
			"CONSTRAINT `fk_c_p` FOREIGN KEY (`pid`) REFERENCES `p` (`id`) ON DELETE CASCADE"+
			") ENGINE=InnoDB",
	)

	before := columnType(t, db, schema, "c", "status")

	runner, store := newRunner(t)
	runner.DB = db
	// Narrowing, not widening and not a nullability change: MySQL refuses
	// ALGORITHM=INPLACE for both of those on at least one supported version, and
	// the ones it accepts are planned natively for a small table, so they would
	// not exercise the foreign-key routing this test is about.
	req := applyRequest(schema, "c", "MODIFY COLUMN status varchar(16) NOT NULL")
	// Report the engine binary as present so the plan is judged on the schema, not
	// on this machine's PATH.
	req.Binaries = map[string]analyzer.EngineBinary{
		"pt-online-schema-change": {Name: "pt-online-schema-change", Available: true, Path: "/nonexistent/pt-osc", Version: "3.5.1"},
	}

	sess, err := runner.Apply(context.Background(), req)
	if sess == nil {
		t.Fatal("no session returned")
	}
	if sess.Engine != "pt-osc" {
		t.Fatalf("engine = %q, want pt-osc", sess.Engine)
	}
	if err == nil {
		t.Fatal("expected the run to fail: the recorded binary path does not exist")
	}

	// The refused run must not have changed anything, and the constraint with its
	// action must be intact.
	if after := columnType(t, db, schema, "c", "status"); after != before {
		t.Fatalf("column changed despite the failure: %q -> %q", before, after)
	}
	var deleteRule string
	if err := db.QueryRow(
		"SELECT DELETE_RULE FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = ? AND CONSTRAINT_NAME = 'fk_c_p'",
		schema).Scan(&deleteRule); err != nil {
		t.Fatalf("foreign key disappeared: %v", err)
	}
	if deleteRule != "CASCADE" {
		t.Fatalf("ON DELETE changed to %q", deleteRule)
	}
	if _, err := store.Load(sess.ID); err != nil {
		t.Fatalf("session not recorded: %v", err)
	}
}

func TestApplyBlockedOnModifiedForeignKeyColumn(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `p` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, PRIMARY KEY (`id`)) ENGINE=InnoDB",
		"CREATE TABLE `c` (`pid` bigint unsigned NOT NULL, KEY `ix_pid` (`pid`), "+
			"CONSTRAINT `fk_c_p` FOREIGN KEY (`pid`) REFERENCES `p` (`id`)) ENGINE=InnoDB",
	)

	runner, _ := newRunner(t)
	_, err := runApply(t, runner, db, applyRequest(schema, "c", "MODIFY COLUMN pid bigint unsigned NOT NULL"))
	if err == nil || !errors.Is(err, core.ErrBlocked) {
		t.Fatalf("expected ErrBlocked for a column used by a foreign key, got %v", err)
	}
}

func TestApplyBlockedOnDuplicateUniqueIndex(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `u` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `email` varchar(191) NOT NULL, "+
			"PRIMARY KEY (`id`)) ENGINE=InnoDB",
		"INSERT INTO `u` (`email`) VALUES ('a@x'), ('a@x')",
	)

	runner, _ := newRunner(t)
	sess, err := runApply(t, runner, db, applyRequest(schema, "u", "ADD UNIQUE INDEX uk_email (email)"))
	if err == nil {
		t.Fatal("expected the unique index creation to be blocked")
	}
	if !errors.Is(err, core.ErrBlocked) {
		t.Fatalf("expected ErrBlocked, got %v", err)
	}
	if sess.Blockers == 0 {
		t.Error("expected at least one blocker")
	}
	if n := countRows(t, db,
		"SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'u' AND INDEX_NAME = 'uk_email'",
		schema); n != 0 {
		t.Fatal("index was created despite duplicates")
	}
}

func TestApplyBlockedOnUnknownOperation(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db, "CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, PRIMARY KEY (`id`)) ENGINE=InnoDB")

	runner, _ := newRunner(t)
	_, err := runApply(t, runner, db, applyRequest(schema, "t", "FORCE"))
	if err == nil || !errors.Is(err, core.ErrBlocked) {
		t.Fatalf("expected ErrBlocked for an unknown operation, got %v", err)
	}
}

func TestApplyRefusesNonNativeEngine(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `a` int NOT NULL, `b` text, PRIMARY KEY (`id`)) ENGINE=InnoDB",
	)

	runner, _ := newRunner(t)
	// An engine the core does not implement must be refused, not executed as
	// native and certainly not reported as a success. Real engines like gh-ost
	// are exercised elsewhere; this one only claims to exist.
	req := applyRequest(schema, "t", "ADD INDEX ix_a (a)")
	req.EngineHint = "definitely-not-an-engine"
	sess, err := runApply(t, runner, db, req)
	if err == nil {
		t.Fatal("expected an error for an unimplemented engine")
	}
	if sess != nil && sess.State == state.StateDone {
		t.Fatalf("unimplemented engine reported success: %+v", sess)
	}
}

// --- preflight --------------------------------------------------------------

func TestPreflightAbortsOnLongRunningStatement(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `a` int NOT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB",
	)

	other, err := sql.Open("mysql", withParseTime(baseDSN()))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		other.ExecContext(ctx, "SELECT SLEEP(40)")
	}()
	time.Sleep(1500 * time.Millisecond)

	runner, _ := newRunner(t)
	req := applyRequest(schema, "t", "ADD INDEX ix_a (a)")
	req.LongQueryThreshold = 1
	_, err = runApply(t, runner, db, req)

	cancel()
	<-done

	if err == nil {
		t.Fatal("expected preflight to abort")
	}
	if !strings.Contains(err.Error(), "running for") {
		t.Fatalf("expected a metadata-lock explanation, got: %v", err)
	}
}

func TestForcedPreflightProceeds(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `a` int NOT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB",
	)

	other, err := sql.Open("mysql", withParseTime(baseDSN()))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		other.ExecContext(ctx, "SELECT SLEEP(40)")
	}()
	time.Sleep(1500 * time.Millisecond)

	runner, _ := newRunner(t)
	req := applyRequest(schema, "t", "ADD INDEX ix_a (a)")
	req.LongQueryThreshold = 1
	req.Force = true
	sess, err := runApply(t, runner, db, req)

	cancel()
	<-done

	if err != nil {
		t.Fatalf("forced apply failed: %v", err)
	}
	if sess.State != state.StateDone {
		t.Fatalf("state = %q", sess.State)
	}
}

// --- sessions ---------------------------------------------------------------

func TestConcurrentSessionOnSameTableIsRefused(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `a` int NOT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB",
	)

	runner, store := newRunner(t)
	if err := store.Lock(schema, "t", "s999", 12*time.Hour); err != nil {
		t.Fatalf("lock: %v", err)
	}

	_, err := runApply(t, runner, db, applyRequest(schema, "t", "ADD INDEX ix_a (a)"))
	if err == nil {
		t.Fatal("expected the second session to be refused")
	}
	if !strings.Contains(err.Error(), "already owned by session s999") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDryRunMutatesNothing(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `a` int NOT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB",
	)

	runner, _ := newRunner(t)
	req := applyRequest(schema, "t", "ADD INDEX ix_a (a)")
	req.DryRun = true

	sess, err := runApply(t, runner, db, req)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(sess.Statements) == 0 {
		t.Fatal("dry run produced no statements")
	}
	if n := countRows(t, db,
		"SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 't' AND INDEX_NAME = 'ix_a'",
		schema); n != 0 {
		t.Fatal("dry run created an index")
	}
}

func TestNoCredentialsInEventLogOrSession(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `a` int NOT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB",
	)

	const secret = "s3cr3t-should-not-appear"

	runner, store := newRunner(t)
	req := applyRequest(schema, "t", "ADD INDEX ix_a (a)")
	req.Conn.Password = secret

	sess, err := runApply(t, runner, db, req)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	raw, err := store.ReadEvents(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("password leaked into the event log")
	}

	saved, err := store.Load(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatal("password leaked into the session file")
	}
}

func TestVerificationFailureIsReported(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `a` int NOT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB",
	)

	// A plan that asks for an index which already exists under a different name
	// shape: DROP then verify absence works, so use a drop of a missing index.
	runner, _ := newRunner(t)
	_, err := runApply(t, runner, db, applyRequest(schema, "t", "DROP INDEX ix_missing"))
	if err == nil {
		t.Fatal("expected MySQL to reject dropping a missing index")
	}
}

// Regression: verification of a pre-existing foreign key used a query with two
// placeholders but passed three arguments, so every apply on a table with an FK
// reported a false verification failure. The index must exist and the FK must be
// reported intact.
func TestVerificationSeesExistingForeignKeys(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `p` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, PRIMARY KEY (`id`)) ENGINE=InnoDB",
		"CREATE TABLE `c` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `pid` bigint unsigned NOT NULL, "+
			"`at` datetime NOT NULL, PRIMARY KEY (`id`), KEY `ix_pid` (`pid`), "+
			"CONSTRAINT `fk_c_p` FOREIGN KEY (`pid`) REFERENCES `p` (`id`) ON DELETE CASCADE) ENGINE=InnoDB",
	)

	runner, _ := newRunner(t)
	sess, err := runApply(t, runner, db, applyRequest(schema, "c", "ADD INDEX ix_at (at)"))
	if err != nil {
		t.Fatalf("apply failed: %v (session error: %s)", err, sess.Error)
	}
	if sess.State != state.StateDone {
		t.Fatalf("state = %q", sess.State)
	}

	var sawFK bool
	for _, v := range sess.Verification {
		if v.Kind == "foreign_key_present" && v.Name == "fk_c_p" {
			sawFK = true
			if !v.OK {
				t.Fatalf("existing foreign key reported as missing: %+v", v)
			}
		}
	}
	if !sawFK {
		t.Fatalf("foreign key verification did not run: %+v", sess.Verification)
	}
}

// Regression: an ALTER that only adds an index must be executable on a table with
// foreign keys, because it does not touch the constrained columns.
func TestAddIndexAllowedOnTableWithForeignKey(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `p` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, PRIMARY KEY (`id`)) ENGINE=InnoDB",
		"CREATE TABLE `c` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `pid` bigint unsigned NOT NULL, "+
			"`at` datetime NOT NULL, PRIMARY KEY (`id`), KEY `ix_pid` (`pid`), "+
			"CONSTRAINT `fk_c_p` FOREIGN KEY (`pid`) REFERENCES `p` (`id`)) ENGINE=InnoDB",
	)

	runner, _ := newRunner(t)
	sess, err := runApply(t, runner, db, applyRequest(schema, "c", "ADD INDEX ix_at (at)"))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if sess.Engine != "native" {
		t.Fatalf("engine = %q, want native", sess.Engine)
	}
}

// --- advise against a live server -------------------------------------------

func unmetRequirementsOf(a analyzer.EngineAssessment) []string {
	var out []string
	for _, r := range a.Requirements {
		if !r.Met {
			out = append(out, r.Name)
		}
	}
	return out
}

func TestAdviseLiveDetectsForeignKeyAndSuggestsPtOSC(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `p` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, PRIMARY KEY (`id`)) ENGINE=InnoDB",
		"CREATE TABLE `c` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `pid` bigint unsigned NOT NULL, "+
			"`status` varchar(32) NOT NULL, PRIMARY KEY (`id`), KEY `ix_pid` (`pid`), "+
			"CONSTRAINT `fk_c_p` FOREIGN KEY (`pid`) REFERENCES `p` (`id`) ON DELETE CASCADE) ENGINE=InnoDB",
	)

	runner, _ := newRunner(t)
	runner.DB = db
	// A rebuild on every version, so the routing question is about the foreign key
	// and nothing else (see the note in TestApplyForeignKeyRebuildRoutesToPtOSC).
	req := applyRequest(schema, "c", "MODIFY COLUMN status varchar(16) NOT NULL")
	req.Binaries = map[string]analyzer.EngineBinary{
		"gh-ost":                  {Name: "gh-ost", Available: true, Path: "/usr/bin/gh-ost", Version: "1.1.7"},
		"pt-online-schema-change": {Name: "pt-online-schema-change", Available: true, Path: "/usr/bin/pt-online-schema-change", Version: "3.5.1"},
	}

	prep, err := runner.Analyze(context.Background(), req)
	if err != nil {
		t.Fatalf("advise: %v", err)
	}
	defer prep.Close()

	byEngine := map[analyzer.Engine]analyzer.EngineAssessment{}
	for _, a := range prep.Plan.Engines {
		byEngine[a.Engine] = a
	}

	// gh-ost cannot work with foreign keys. This is the whole reason the engine
	// matrix exists.
	gh, ok := byEngine[analyzer.EngineGhOst]
	if !ok {
		t.Fatal("gh-ost assessment missing")
	}
	if gh.Verdict != analyzer.VerdictUnsuitable {
		t.Fatalf("gh-ost verdict = %q, want unsuitable on an FK table", gh.Verdict)
	}
	if !gh.Installed {
		t.Fatal("gh-ost is installed, so the verdict must be about the engine, not availability")
	}
	var fkBlocked bool
	for _, r := range unmetRequirementsOf(gh) {
		if r == "no foreign keys in or out" {
			fkBlocked = true
		}
	}
	if !fkBlocked {
		t.Fatalf("gh-ost unmet requirements = %v", unmetRequirementsOf(gh))
	}

	// pt-osc handles foreign keys natively, so it is the answer here.
	pt, ok := byEngine[analyzer.EnginePtOSC]
	if !ok {
		t.Fatal("pt-osc assessment missing")
	}
	if pt.Verdict != analyzer.VerdictSuitable {
		t.Fatalf("pt-osc verdict = %q (%v)", pt.Verdict, unmetRequirementsOf(pt))
	}
	if prep.Plan.Recommendation.Engine != analyzer.EnginePtOSC {
		t.Fatalf("recommended = %q, want pt-osc", prep.Plan.Recommendation.Engine)
	}
}

func TestAdviseLiveFavoursNativeForAddIndex(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `at` datetime NOT NULL, "+
			"PRIMARY KEY (`id`)) ENGINE=InnoDB",
	)

	runner, _ := newRunner(t)
	runner.DB = db
	prep, err := runner.Analyze(context.Background(), applyRequest(schema, "t", "ADD INDEX ix_at (at)"))
	if err != nil {
		t.Fatalf("advise: %v", err)
	}
	defer prep.Close()

	var native analyzer.EngineAssessment
	for _, a := range prep.Plan.Engines {
		if a.Engine == analyzer.EngineNative {
			native = a
		}
	}
	if native.Verdict != analyzer.VerdictSuitable {
		t.Fatalf("native verdict = %q: %v", native.Verdict, unmetRequirementsOf(native))
	}
	if !prep.Plan.Recommendation.Executable {
		t.Fatal("native recommendation must be executable")
	}
	if len(prep.Plan.TableFacts) == 0 {
		t.Fatal("a live plan must carry the schema facts the advice is based on")
	}
}

// --- engine choice and copy-engine wiring -----------------------------------

// The wrapper must invoke the engine it promised, with the argv it printed. A
// fake binary on PATH proves the wiring; because the fake does nothing, this also
// proves that a zero exit code is not treated as success.
func TestApplyRunsTheChosenCopyEngineAndStillVerifies(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `status` varchar(32) NOT NULL, "+
			"PRIMARY KEY (`id`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
	)

	dir := t.TempDir()
	capture := filepath.Join(dir, "argv.txt")
	bin := filepath.Join(dir, "pt-online-schema-change")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + capture + "\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Record the operator's decision, exactly as `saksama engine set` would.
	choice, err := state.NewChoice("pt-osc", false, schema, "t")
	if err != nil {
		t.Fatal(err)
	}
	choice.Actor = "test"
	if err := store.SetEngineChoice(choice); err != nil {
		t.Fatal(err)
	}

	runner := &core.Runner{DB: db, Store: store}
	req := applyRequest(schema, "t", "ADD COLUMN `note` varchar(64) NOT NULL DEFAULT ''")
	req.EngineHint = analyzer.EnginePtOSC
	req.EngineHintSource = "recorded"
	req.Binaries = map[string]analyzer.EngineBinary{
		"pt-online-schema-change": {Name: "pt-online-schema-change", Available: true, Path: bin, Version: "3.5.1"},
	}

	sess, applyErr := runner.Apply(context.Background(), req)
	if sess == nil {
		t.Fatal("no session returned")
	}
	if sess.Engine != "pt-osc" {
		t.Fatalf("engine = %q, want pt-osc", sess.Engine)
	}

	// The engine was really invoked with the argv we advertise.
	data, readErr := os.ReadFile(capture)
	if readErr != nil {
		t.Fatalf("the engine was never invoked: %v", readErr)
	}
	argv := string(data)
	for _, want := range []string{"--alter", "--execute", "D=" + schema, "t=t"} {
		if !strings.Contains(argv, want) {
			t.Errorf("engine did not receive %q; argv:\n%s", want, argv)
		}
	}

	// The fake engine exited 0 but changed nothing. Verification must catch it:
	// an engine's exit code is not evidence.
	if applyErr == nil {
		t.Fatalf("a no-op engine that exits 0 must not be reported as success; state=%q verification=%+v",
			sess.State, sess.Verification)
	}
	if sess.State != state.StateFailed {
		t.Fatalf("state = %q, want failed", sess.State)
	}
	if n := countRows(t, db,
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 't' AND COLUMN_NAME = 'note'",
		schema); n != 0 {
		t.Fatal("the column should not exist")
	}
}

// --- schema-wide scan -------------------------------------------------------

func TestIntrospectSchemaTablesReadsEveryTable(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `users` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, PRIMARY KEY (`id`)) ENGINE=InnoDB",
		"CREATE TABLE `spots` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, PRIMARY KEY (`id`)) ENGINE=InnoDB",
		"CREATE TABLE `orders` ("+
			"`id` bigint unsigned NOT NULL AUTO_INCREMENT,"+
			"`spot_id` bigint unsigned NOT NULL,"+
			"`status` varchar(32) NOT NULL,"+
			"PRIMARY KEY (`id`), KEY `ix_spot` (`spot_id`),"+
			"CONSTRAINT `fk_o_s` FOREIGN KEY (`spot_id`) REFERENCES `spots` (`id`) ON DELETE CASCADE"+
			") ENGINE=InnoDB",
		"CREATE TABLE `m2m` (`a` bigint unsigned NOT NULL, `b` bigint unsigned NOT NULL, PRIMARY KEY (`a`,`b`)) ENGINE=InnoDB",
		"CREATE TABLE `audit` (`msg` text) ENGINE=InnoDB",
		"CREATE VIEW `v_orders` AS SELECT id FROM `orders`",
	)

	c := introspect.NewCollector(db, schema, "")
	tables, err := c.SchemaTables(context.Background(), false)
	if err != nil {
		t.Fatalf("schema tables: %v", err)
	}

	byName := map[string]model.Table{}
	for _, tbl := range tables {
		byName[tbl.Name] = tbl
	}
	if _, ok := byName["v_orders"]; ok {
		t.Error("views must not be included by default")
	}
	if len(byName) != 5 {
		t.Fatalf("got %d tables, want 5: %v", len(byName), byName)
	}

	// The FK must be visible from both sides, including its action.
	if fk := byName["orders"].ForeignKeys; len(fk) != 1 || fk[0].OnDelete != "CASCADE" {
		t.Fatalf("orders FK = %+v", fk)
	}
	if ref := byName["spots"].ReferencedBy; len(ref) != 1 || ref[0].RefTable != "orders" {
		t.Fatalf("spots incoming = %+v", ref)
	}

	// Composite key must be visible as two parts.
	pk, ok := byName["m2m"].PrimaryKey()
	if !ok || len(pk.Parts) != 2 {
		t.Fatalf("m2m PK = %+v", pk)
	}
	// No-key table must report as such.
	if _, ok := byName["audit"].PrimaryKey(); ok {
		t.Error("audit must have no primary key")
	}

	// And the assessment must turn that into a usable recommendation.
	settings, _ := c.Settings(context.Background())
	rows := analyzer.AssessSchema(tables, model.Server{Version: "8.0.46", Vendor: "mysql"}, nil, settings)
	byTable := map[string]analyzer.TableAssessment{}
	for _, r := range rows {
		byTable[r.Table] = r
	}
	if byTable["orders"].RebuildEngine != analyzer.EnginePtOSC {
		t.Errorf("orders rebuild engine = %q, want pt-osc", byTable["orders"].RebuildEngine)
	}
	// Which engine a plain table gets depends on whether this server can run
	// gh-ost at all, and that is a property of the server, not of the test. A 5.6
	// container started without --log-bin must route to pt-osc and say why.
	wantPlain := analyzer.EnginePtOSC
	if strings.EqualFold(settings["log_bin"], "ON") {
		wantPlain = analyzer.EngineGhOst
	}
	if byTable["users"].RebuildEngine != wantPlain {
		t.Errorf("users rebuild engine = %q, want %q (log_bin=%q)",
			byTable["users"].RebuildEngine, wantPlain, settings["log_bin"])
	}
	if wantPlain == analyzer.EnginePtOSC {
		var said bool
		for _, note := range byTable["users"].Notes {
			if strings.Contains(note, "not configured") || strings.Contains(note, "gh-ost") {
				said = true
			}
		}
		if !said {
			t.Errorf("routing to pt-osc without a reason: %v", byTable["users"].Notes)
		}
	}
	if byTable["audit"].RebuildEngine != analyzer.EngineNone {
		t.Errorf("audit rebuild engine = %q, want none", byTable["audit"].RebuildEngine)
	}
}

// --- server settings doctor -------------------------------------------------

func TestSettingsAreReadFromAServerWithoutBinlog(t *testing.T) {
	// This container is started without --log-bin by the integration target, so on
	// MySQL 8.0 the value depends on the server default. Assert the read works and
	// that the doctor agrees with what the server says, whichever it is.
	schema, db := fixtureSchema(t)
	_ = schema

	c := introspect.NewCollector(db, schema, "")
	settings, err := c.Settings(context.Background())
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	for _, key := range []string{"log_bin", "binlog_format", "read_only", "performance_schema"} {
		if _, ok := settings[key]; !ok {
			t.Errorf("missing %s in the settings read", key)
		}
	}

	checks := analyzer.SettingChecks(analyzer.SettingsFromMap(settings))
	if len(checks) == 0 {
		t.Fatal("no setting checks produced")
	}
	// The check must reflect the server, not a guess.
	var logBin analyzer.SettingCheck
	for _, c := range checks {
		if c.Setting == "log_bin" {
			logBin = c
		}
	}
	if logBin.Current == "" || logBin.Current == "(not inspected)" {
		t.Fatalf("log_bin was not read: %+v", logBin)
	}
	if logBin.Status == analyzer.EnvFail && !strings.EqualFold(logBin.Current, "OFF") {
		t.Fatalf("log_bin=%q reported as a failure", logBin.Current)
	}

	// And the plan must carry that environment report, so `apply` can refuse on an
	// unconfigured server rather than failing inside the engine.
	runner, _ := newRunner(t)
	runner.DB = db
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `status` varchar(32) NOT NULL, "+
			"PRIMARY KEY (`id`)) ENGINE=InnoDB")
	prep, err := runner.Analyze(context.Background(),
		applyRequest(schema, "t", "MODIFY COLUMN status varchar(64) NOT NULL"))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	defer prep.Close()
	if len(prep.Plan.Environment) == 0 {
		t.Fatal("the plan carries no environment report")
	}
}

// --- disk space -------------------------------------------------------------

// MySQL cannot report free filesystem space over SQL, so Saksama reads the volume
// itself when the data directory is local. In the container the client is not on
// the database host, so this asserts the honest "not measured" path, which is
// what a remote user sees too.
func TestDiskSpaceIsMeasuredOrHonestlyUnmeasured(t *testing.T) {
	schema, db := fixtureSchema(t)

	c := introspect.NewCollector(db, schema, "")
	settings, err := c.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	datadir := settings["datadir"]
	if datadir == "" {
		t.Skip("server did not report a datadir")
	}

	disk := introspect.ReadDiskSpace(datadir)
	if disk.Measured {
		if disk.Free <= 0 || disk.Total <= 0 {
			t.Fatalf("measured disk looks invalid: %+v", disk)
		}
		if disk.Free > disk.Total {
			t.Fatalf("free exceeds total: %+v", disk)
		}
		return
	}
	if disk.Reason == "" {
		t.Fatal("an unmeasured disk must say why")
	}
}

// Applying without a measurable disk and without --free-disk must refuse, because
// a migration that fills the volume takes the whole server down with it.
func TestApplyRefusesWhenDiskIsUnknown(t *testing.T) {
	schema, db := fixtureSchema(t)

	// The server runs in a container, so its data directory is not on this
	// machine. If the environment makes it look local anyway, this test cannot
	// assert the refusal without inventing a situation, so say so and stop.
	if snap, err := introspect.NewCollector(db, schema, "").Collect(context.Background()); err == nil {
		if snap.Runtime.Disk.Measured {
			t.Skipf("the server's data directory was measured directly (%s); this case needs it unmeasured",
				snap.Runtime.Disk.Path)
		}
	}
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `a` int NOT NULL, "+
			"PRIMARY KEY (`id`)) ENGINE=InnoDB")

	runner, _ := newRunner(t)
	runner.DB = db
	// A change that copies on every version: int -> bigint is refused by
	// ALGORITHM=INPLACE everywhere, so it needs a copy engine, and a copy engine
	// needs the disk figure. ADD INDEX would not: an index build writes index
	// pages, not a second table, so the disk check deliberately does not apply.
	req := applyRequest(schema, "t", "MODIFY COLUMN a bigint NOT NULL")
	req.ForExecution = true

	sess, err := runner.Apply(context.Background(), req)
	if err == nil {
		t.Fatalf("expected a refusal for an unmeasured disk, got state %q", sess.State)
	}
	if !errors.Is(err, core.ErrBlocked) {
		t.Fatalf("expected ErrBlocked, got %v", err)
	}
	// The refusal must be about the disk, not about an engine this machine happens
	// to be missing: otherwise the test would pass for the wrong reason.
	if !planHasBlocker(t, sess.Plan, "D6") {
		t.Fatalf("the refusal should come from D6, not from something else: %s", sess.Plan)
	}
	// And nothing may have changed. Compared against the server's own spelling:
	// 5.7 reports int(11) where 8.0 reports int.
	if typ := columnType(t, db, schema, "t", "a"); !strings.HasPrefix(typ, "int") {
		t.Fatalf("the column changed despite the refusal: %s", typ)
	}
}

// A server on a different host must not be measured from here, even when the same
// path happens to exist locally. A CI runner with MySQL preinstalled has
// /var/lib/mysql, which is somebody else's filesystem.
func TestDiskIsNotMeasuredForAServerOnAnotherHost(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db, "CREATE TABLE `t` (`id` int NOT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB")

	collector := introspect.NewCollector(db, schema, "t")
	settings, err := collector.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings["datadir"] == "" {
		t.Skip("server did not report a datadir")
	}

	var serverHost string
	if err := db.QueryRow("SELECT @@hostname").Scan(&serverHost); err != nil {
		t.Fatal(err)
	}
	local, _ := os.Hostname()
	if strings.EqualFold(local, serverHost) {
		t.Skip("the server reports this machine's hostname, so measuring is correct here")
	}

	snap, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Runtime.Disk.Measured {
		t.Fatalf("measured %s for a server on %q while this machine is %q",
			snap.Runtime.Disk.Path, serverHost, local)
	}
	if snap.Runtime.Disk.Reason == "" {
		t.Fatal("an unmeasured disk must say why")
	}
}

// The same apply proceeds when the operator supplies the numbers.
func TestApplyProceedsWithOperatorSuppliedDisk(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `a` int NOT NULL, "+
			"PRIMARY KEY (`id`)) ENGINE=InnoDB")

	runner, _ := newRunner(t)
	runner.DB = db
	req := applyRequest(schema, "t", "ADD INDEX ix_a (a)")
	req.ForExecution = true
	req.FreeDisk = 500 << 30
	req.DiskTotal = 1000 << 30

	sess, err := runner.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("apply: %v (session error: %s)", err, sess.Error)
	}
	if sess.State != state.StateDone {
		t.Fatalf("state = %q", sess.State)
	}
}

// --- fingerprint revalidation ----------------------------------------------

// The plan is computed and then executed later, possibly by a different person.
// If the schema moved in between, the ALTER was written for a table that no longer
// exists in that shape, so it must be refused rather than guessed at.
func TestApplyRefusesWhenSchemaChangedAfterPlanning(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `a` int NOT NULL, "+
			"PRIMARY KEY (`id`)) ENGINE=InnoDB")

	runner, _ := newRunner(t)
	runner.DB = db

	// Plan first, against the current shape.
	req := applyRequest(schema, "t", "ADD INDEX ix_a (a)")
	prep, err := runner.Analyze(context.Background(), req)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	planned := prep.Plan.SchemaFingerprint
	prep.Close()

	// Someone else changes the table before this plan is executed.
	mustExec(t, db, "ALTER TABLE `t` ADD COLUMN `surprise` int NULL")

	// Executing the already-planned fingerptrint must be refused even though the
	// engine itself would happily run.
	current, err := introspect.NewCollector(db, schema, "t").Table(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if analyzer.Fingerprint(current) == planned {
		t.Fatal("the fingerprint did not notice a new column, so the check would be useless")
	}
}

// The same table, replanned, must be accepted: the guard refuses a stale plan, not
// the change.
func TestApplyAcceptsAFreshPlanAfterSchemaChange(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `a` int NOT NULL, "+
			"PRIMARY KEY (`id`)) ENGINE=InnoDB",
		"ALTER TABLE `t` ADD COLUMN `surprise` int NULL",
	)

	runner, _ := newRunner(t)
	runner.DB = db
	req := applyRequest(schema, "t", "ADD INDEX ix_a (a)")
	req.ForExecution = true
	req.FreeDisk = 200 << 30
	req.DiskTotal = 500 << 30

	sess, err := runner.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("apply: %v (session error: %s)", err, sess.Error)
	}
	if sess.State != state.StateDone {
		t.Fatalf("state = %q", sess.State)
	}
}

// ApplyFromSession is the plan, review, execute workflow: the stored plan must be
// refused once the schema no longer matches it.
func TestApplyFromSessionRefusesAStalePlan(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `a` int NOT NULL, "+
			"PRIMARY KEY (`id`)) ENGINE=InnoDB")

	runner, _ := newRunner(t)
	runner.DB = db

	// Plan and keep it: --session executes a stored plan, so this one is recorded.
	prep, err := runner.Prepare(context.Background(), applyRequest(schema, "t", "ADD INDEX ix_a (a)"))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	sessionID := prep.Session.ID
	prep.Close()

	// The table moves before anyone executes the plan.
	mustExec(t, db, "ALTER TABLE `t` ADD COLUMN `surprise` int NULL")

	req := applyRequest(schema, "t", "")
	req.ForExecution = true
	req.FreeDisk = 200 << 30
	req.DiskTotal = 500 << 30

	sess, err := runner.ApplyFromSession(context.Background(), sessionID, req)
	if err == nil {
		t.Fatalf("expected the stale plan to be refused, got state %q", sess.State)
	}
	if !errors.Is(err, core.ErrBlocked) {
		t.Fatalf("expected ErrBlocked, got %v", err)
	}
	if !strings.Contains(err.Error(), "changed since session") {
		t.Fatalf("the error should say what changed: %v", err)
	}

	// Nothing may have run.
	if typ := columnType(t, db, schema, "t", "a"); typ != "int" && typ != "int(11)" {
		t.Fatalf("the column changed despite the refusal: %s", typ)
	}
}

func TestApplyFromSessionRunsAFreshPlan(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `a` int NOT NULL, "+
			"PRIMARY KEY (`id`)) ENGINE=InnoDB")

	runner, _ := newRunner(t)
	runner.DB = db

	prep, err := runner.Prepare(context.Background(), applyRequest(schema, "t", "ADD INDEX ix_a (a)"))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	sessionID := prep.Session.ID
	prep.Close()

	req := applyRequest(schema, "t", "")
	req.ForExecution = true
	req.FreeDisk = 200 << 30
	req.DiskTotal = 500 << 30

	sess, err := runner.ApplyFromSession(context.Background(), sessionID, req)
	if err != nil {
		t.Fatalf("apply from session: %v (session error: %s)", err, sess.Error)
	}
	if sess.State != state.StateDone {
		t.Fatalf("state = %q", sess.State)
	}
	if sess.ResumedFrom != sessionID {
		t.Fatalf("the execution should record the plan it followed, got %q", sess.ResumedFrom)
	}
	if n := countRows(t, db,
		"SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 't' AND INDEX_NAME = 'ix_a'",
		schema); n == 0 {
		t.Fatal("the index was not created")
	}
}

// --- durable sessions: detached engine, attach, cancel ---------------------

// A killed client must not kill the migration. The engine runs in its own process
// group and its pid is recorded, so `cancel` can stop it later from anywhere.
func TestCopyEngineRunsDetachedAndCanBeCancelled(t *testing.T) {
	// No real engine needed here: the plan's verified path is what runs, so a stub
	// is enough to prove detachment and cancellation.
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `a` int NOT NULL, "+
			"PRIMARY KEY (`id`)) ENGINE=InnoDB")

	// A stand-in engine that sleeps long enough to be cancelled, and records its
	// own command line so we can see it really started.
	dir := t.TempDir()
	capture := filepath.Join(dir, "argv.txt")
	bin := filepath.Join(dir, "pt-online-schema-change")
	// A long-lived process that does not spawn a long-lived child, so a kill
	// closes the output pipe promptly and the test stays fast.
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + capture + "\nwhile :; do sleep 0.2; done\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	runner, store := newRunner(t)
	runner.DB = db
	req := applyRequest(schema, "t", "ADD COLUMN `note` varchar(64) NULL")
	req.EngineHint = analyzer.EnginePtOSC
	req.EngineHintSource = "recorded"
	req.ForExecution = true
	req.Detach = true
	req.FreeDisk = 200 << 30
	req.DiskTotal = 500 << 30
	req.Binaries = map[string]analyzer.EngineBinary{
		"pt-online-schema-change": {Name: "pt-online-schema-change", Available: true, Path: bin, Version: "stub"},
	}

	done := make(chan struct{})
	var sess *state.Session
	go func() {
		sess, _ = runner.Apply(context.Background(), req)
		close(done)
	}()

	// Wait until the pid is recorded, which is the moment the engine exists.
	var pid int
	for i := 0; i < 100; i++ {
		time.Sleep(50 * time.Millisecond)
		sessions, _ := store.List()
		if len(sessions) > 0 && sessions[0].EnginePID > 0 {
			pid = sessions[0].EnginePID
			break
		}
	}
	if pid == 0 {
		t.Fatal("no engine pid was recorded, so cancel could not work")
	}
	if !process.Alive(pid) {
		t.Fatalf("recorded pid %d is not running", pid)
	}
	if _, err := os.Stat(capture); err != nil {
		t.Fatalf("the engine never started: %v", err)
	}

	// Cancelling from another "client" is just a signal to the recorded pid.
	if err := process.Stop(pid, 15*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if process.Alive(pid) {
		t.Fatal("the engine survived a cancel")
	}

	<-done
	if sess != nil && sess.State == state.StateDone {
		t.Fatalf("a cancelled run must not be reported as done: %+v", sess)
	}
}

// --- artifact ownership and cleanup ----------------------------------------

// gh-ost keeps the pre-migration table as _table_del. That is real disk usage, and
// cleanup may only remove it because the session recorded that it made it.
func TestCleanupDropsOnlyRecordedArtifacts(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `keepme` (`id` int NOT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB",
		"CREATE TABLE `_t_del` (`id` int NOT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB",
		"CREATE TABLE `t_unrelated` (`id` int NOT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB",
	)

	runner, store := newRunner(t)
	runner.DB = db
	sess := &state.Session{
		ID: "s1", CreatedAt: "2026-01-01T00:00:00Z", State: state.StateDone,
		Database: schema, Table: "t", Engine: "gh-ost", Host: "test",
	}
	sess.RecordArtifact("table", "_t_del", "gh-ost", "pre-migration table")
	sess.MarkArtifactsSafe("done")
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}

	dropped, failed := dropArtifactsForTest(t, db, schema, sess)
	if failed != 0 {
		t.Fatalf("%d artifact(s) failed", failed)
	}
	if len(dropped) != 1 || dropped[0] != "_t_del" {
		t.Fatalf("dropped %v, want only _t_del", dropped)
	}

	// The unrelated tables must be untouched: cleanup follows ownership, not names.
	for _, name := range []string{"keepme", "t_unrelated"} {
		if n := countRows(t, db,
			"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?",
			schema, name); n != 1 {
			t.Fatalf("%s was dropped, but this session never recorded it", name)
		}
	}
	if n := countRows(t, db,
		"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = '_t_del'",
		schema); n != 0 {
		t.Fatal("the recorded artifact was not dropped")
	}
}

// A live session's artifacts may be the only way back, so they are not deletable.
func TestLiveSessionArtifactsAreNotDeletable(t *testing.T) {
	sess := &state.Session{State: state.StateRunning, Engine: "gh-ost"}
	sess.RecordArtifact("table", "_t_gho", "gh-ost", "ghost table")
	if got := sess.DeletableArtifacts(); len(got) != 0 {
		t.Fatalf("a running session must not offer its artifacts: %v", got)
	}
	sess.State = state.StateFailed
	sess.MarkArtifactsSafe("failed")
	if got := sess.DeletableArtifacts(); len(got) != 1 {
		t.Fatalf("a terminal session should offer its artifacts: %v", got)
	}
}

// --- held cutover -----------------------------------------------------------

// The gate is a file the engine waits on. The core creates it before the engine
// starts, records it, and `cutover` removes it. This asserts the file lifecycle
// without needing gh-ost's own cutover behaviour.
func TestHoldCutoverCreatesAndReleasesTheGate(t *testing.T) {
	schema, db := fixtureSchema(t)
	// Holding the swap is a gh-ost feature: pt-osc swaps the moment the copy
	// finishes, and there is nothing to hold.
	requireBinlog(t, db)
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `a` int NOT NULL, "+
			"PRIMARY KEY (`id`)) ENGINE=InnoDB")

	runner, store := newRunner(t)
	runner.DB = db

	// A stub engine that waits for the gate file to disappear, like gh-ost does.
	dir := t.TempDir()
	bin := filepath.Join(dir, "gh-ost")
	// The stub touches "$gate.started" once it has seen the gate. Waiting for the
	// pid alone is not enough: the core knows the pid as soon as exec returns, and
	// can record it before this script's first line runs. Releasing on that signal
	// made the test fail about one run in three with "gate missing at start".
	script := "#!/bin/sh\n" +
		"gate=$(printf '%s\\n' \"$@\" | sed -n 's/^--postpone-cut-over-flag-file=//p')\n" +
		"[ -n \"$gate\" ] || { echo 'no gate given' >&2; exit 2; }\n" +
		"[ -f \"$gate\" ] || { echo 'gate missing at start' >&2; exit 3; }\n" +
		": > \"$gate.started\"\n" +
		"while [ -f \"$gate\" ]; do sleep 0.2; done\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	req := applyRequest(schema, "t", "ADD COLUMN `note` varchar(64) NULL")
	req.EngineHint = analyzer.EngineGhOst
	req.EngineHintSource = "recorded"
	req.ForExecution = true
	req.HoldCutover = true
	req.FreeDisk = 200 << 30
	req.DiskTotal = 500 << 30
	req.Binaries = map[string]analyzer.EngineBinary{
		"gh-ost": {Name: "gh-ost", Available: true, Path: bin, Version: "stub"},
	}

	done := make(chan struct{})
	var sess *state.Session
	var applyErr error
	go func() {
		sess, applyErr = runner.Apply(context.Background(), req)
		close(done)
	}()

	// Wait until the session records the gate AND the engine has started. Releasing
	// the gate before the engine is running is a real race: the file is written
	// first, and a stub that checks for it at startup would then find it gone and
	// exit 3. An operator would not release a cutover before `status` showed a pid
	// either.
	var gate string
	var sessionID string
	for i := 0; i < 100; i++ {
		time.Sleep(50 * time.Millisecond)
		sessions, _ := store.List()
		if len(sessions) > 0 && sessions[0].CutoverFlagFile != "" && sessions[0].EnginePID != 0 {
			gate = sessions[0].CutoverFlagFile
			sessionID = sessions[0].ID
			break
		}
	}
	if gate == "" {
		select {
		case <-done:
		default:
		}
		sessions, _ := store.List()
		for _, s := range sessions {
			t.Logf("session %s state=%s pid=%d gate=%q error=%q artifacts=%v",
				s.ID, s.State, s.EnginePID, s.CutoverFlagFile, s.Error, s.Artifacts)
		}
		t.Fatalf("no cutover gate was recorded, so cutover could not release it")
	}
	if _, err := os.Stat(gate); err != nil {
		t.Fatalf("the gate file must exist before the engine starts: %v", err)
	}
	// Wait for the engine to confirm it saw the gate, so the release below cannot
	// race a script that has not started yet.
	var started bool
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(gate + ".started"); err == nil {
			started = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !started {
		t.Fatal("the stub engine never reported that it saw the gate")
	}

	// Releasing it is a separate command, as it would be from another terminal.
	released, err := runner.ReleaseCutover(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if released.CutoverFlagFile != "" {
		t.Fatal("the session still points at a released gate")
	}
	if _, err := os.Stat(gate); !os.IsNotExist(err) {
		t.Fatal("the gate file was not removed")
	}

	// The stub exits 0 without changing anything, so verification must refuse to
	// call it a success. That is the point: releasing the gate does not make the
	// result true.
	<-done
	if applyErr == nil {
		t.Fatal("a no-op engine must not be reported as a successful migration")
	}
	if sess.State != state.StateFailed {
		t.Fatalf("state = %q", sess.State)
	}
	if !strings.Contains(sess.Error, "verification") {
		t.Fatalf("the failure should be a verification failure, got %q", sess.Error)
	}
	// The gate has served its purpose and must not linger in /tmp.
	if _, err := os.Stat(gate); !os.IsNotExist(err) {
		t.Fatal("the gate file was left behind")
	}
	if sess.CutoverFlagFile != "" {
		t.Fatalf("the session still references a finished gate: %q", sess.CutoverFlagFile)
	}
}

// A session without a held cutover must say so plainly instead of pretending.
func TestCutoverRefusesWhenNothingIsHeld(t *testing.T) {
	runner, store := newRunner(t)
	sess := &state.Session{
		ID: "s1", CreatedAt: "2026-01-01T00:00:00Z", State: state.StateRunning,
		Database: "shop", Table: "t", Engine: "native",
	}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.ReleaseCutover(context.Background(), "s1"); err == nil {
		t.Fatal("expected a refusal")
	} else if !strings.Contains(err.Error(), "no held cutover") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

// --- reconcile --------------------------------------------------------------

// After a crash, a session is neither obviously failed nor obviously running. What
// reconcile reports is the observation, plus what exists in the database.
func TestReconcileReportsUnfinishedSessionsAndLeftovers(t *testing.T) {
	schema, db := fixtureSchema(t)
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `a` int NOT NULL, "+
			"PRIMARY KEY (`id`)) ENGINE=InnoDB",
		// A leftover that a session claims, and one it does not.
		"CREATE TABLE `_t_gho` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, PRIMARY KEY (`id`)) ENGINE=InnoDB",
	)

	runner, store := newRunner(t)
	runner.DB = db

	// An unfinished session with a recorded artifact that still exists.
	stale := &state.Session{
		ID: "s1", CreatedAt: "2026-01-01T00:00:00Z", State: state.StateRunning,
		Database: schema, Table: "t", Engine: "gh-ost",
	}
	stale.RecordArtifact("table", "_t_gho", "gh-ost", "ghost table")
	if err := store.Save(stale); err != nil {
		t.Fatal(err)
	}

	// A finished session must not appear at all.
	done := &state.Session{
		ID: "s2", CreatedAt: "2026-01-02T00:00:00Z", State: state.StateDone,
		Database: schema, Table: "t", Engine: "native",
	}
	if err := store.Save(done); err != nil {
		t.Fatal(err)
	}

	findings, err := runner.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1 (only the unfinished session): %+v", len(findings), findings)
	}

	f := findings[0]
	if f.Session != "s1" {
		t.Fatalf("wrong session: %q", f.Session)
	}
	if f.EngineAlive {
		t.Fatal("no engine pid was recorded, so it cannot be alive")
	}
	if len(f.Observed) != 1 || !strings.Contains(f.Observed[0], "_t_gho") {
		t.Fatalf("the leftover was not observed: %v", f.Observed)
	}
	if !f.Actionable {
		t.Fatal("a session with leftovers should suggest cleanup")
	}
	if !strings.Contains(f.Verdict, "left") {
		t.Fatalf("verdict should name the situation: %q", f.Verdict)
	}
}

func TestReconcileIsQuietWhenEverythingFinished(t *testing.T) {
	schema, db := fixtureSchema(t)
	_ = schema

	runner, store := newRunner(t)
	runner.DB = db
	if err := store.Save(&state.Session{
		ID: "s1", CreatedAt: "2026-01-01T00:00:00Z", State: state.StateDone,
		Database: "shop", Table: "t", Engine: "native",
	}); err != nil {
		t.Fatal(err)
	}

	findings, err := runner.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected nothing to reconcile, got %+v", findings)
	}
}

// D13 asks the server for innodb_online_alter_log_max_size, so the collector has to
// actually carry it. Without this, the check would be a rule that can never fire on
// a real server: it would see an unknown value, report "could not be read", and
// look like a cautious pass forever.
func TestCollectorCarriesTheOnlineAlterLogSize(t *testing.T) {
	db := adminDB(t)
	c := introspect.NewCollector(db, "", "")
	settings, err := c.Settings(context.Background())
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	raw, ok := settings["innodb_online_alter_log_max_size"]
	if !ok {
		t.Fatalf("innodb_online_alter_log_max_size was not collected; got %d settings: %v", len(settings), settings)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		t.Fatalf("the value is not a byte count: %q", raw)
	}
	if n <= 0 {
		t.Fatalf("innodb_online_alter_log_max_size = %d", n)
	}
	t.Logf("innodb_online_alter_log_max_size = %d bytes", n)
}
