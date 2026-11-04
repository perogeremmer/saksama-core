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
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/core"
	"github.com/perogeremmer/saksama-core/internal/cred"
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

func applyRequest(schema, table, alter string) core.Request {
	return core.Request{
		Conn:     cred.Conn{User: "root", Host: "127.0.0.1", Port: "33306"},
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
	return runner.Apply(context.Background(), req)
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
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC",
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
	if snap.Table.RowFormat != "DYNAMIC" {
		t.Errorf("row format = %q", snap.Table.RowFormat)
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
		"CREATE TABLE `p` (`id` varchar(36) NOT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci",
		"CREATE TABLE `c` (`pid` varchar(36) NOT NULL, KEY `ix` (`pid`), "+
			"CONSTRAINT `fk_c_p` FOREIGN KEY (`pid`) REFERENCES `p` (`id`)) "+
			"ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci",
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
	req := applyRequest(schema, "c", "MODIFY COLUMN status varchar(64) NOT NULL")
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
	// A TEXT index without a prefix is blocked; but even a valid rebuild request
	// must not silently pretend gh-ost ran.
	req := applyRequest(schema, "t", "ADD INDEX ix_b (b(16))")
	req.EngineHint = "gh-ost"
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
	req := applyRequest(schema, "c", "MODIFY COLUMN status varchar(64) NOT NULL")
	req.Binaries = map[string]analyzer.EngineBinary{
		"gh-ost":                  {Name: "gh-ost", Available: true, Path: "/usr/bin/gh-ost", Version: "1.1.7"},
		"pt-online-schema-change": {Name: "pt-online-schema-change", Available: true, Path: "/usr/bin/pt-online-schema-change", Version: "3.5.1"},
	}

	prep, err := runner.PlanOnly(context.Background(), req)
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
	prep, err := runner.PlanOnly(context.Background(), applyRequest(schema, "t", "ADD INDEX ix_at (at)"))
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
	for _, want := range []string{"--alter", "--print", "D=" + schema, "t=t"} {
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
	if byTable["users"].RebuildEngine != analyzer.EngineGhOst {
		t.Errorf("users rebuild engine = %q, want gh-ost", byTable["users"].RebuildEngine)
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
	prep, err := runner.PlanOnly(context.Background(),
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
	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `a` int NOT NULL, "+
			"PRIMARY KEY (`id`)) ENGINE=InnoDB")

	runner, _ := newRunner(t)
	runner.DB = db
	req := applyRequest(schema, "t", "ADD INDEX ix_a (a)")
	req.ForExecution = true

	sess, err := runner.Apply(context.Background(), req)
	if err == nil {
		t.Fatalf("expected a refusal for an unmeasured disk, got state %q", sess.State)
	}
	if !errors.Is(err, core.ErrBlocked) {
		t.Fatalf("expected ErrBlocked, got %v", err)
	}
	// And nothing may have changed.
	if n := countRows(t, db,
		"SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 't' AND INDEX_NAME = 'ix_a'",
		schema); n != 0 {
		t.Fatal("the index was created despite the refusal")
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
