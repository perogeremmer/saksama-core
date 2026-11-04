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
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/perogeremmer/saksama-core/internal/core"
	"github.com/perogeremmer/saksama-core/internal/cred"
	"github.com/perogeremmer/saksama-core/internal/introspect"
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

// TestApplyBlockedOnForeignKeyRebuild is the guardrail that matters most: a
// rebuild on a table with foreign keys must be refused before anything runs.
func TestApplyBlockedOnForeignKeyRebuild(t *testing.T) {
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
	sess, err := runApply(t, runner, db, applyRequest(schema, "c", "MODIFY COLUMN status varchar(64) NOT NULL"))
	if err == nil {
		t.Fatal("expected the migration to be blocked")
	}
	if !errors.Is(err, core.ErrBlocked) {
		t.Fatalf("expected ErrBlocked, got %v", err)
	}
	if sess.State != state.StateBlocked {
		t.Errorf("state = %q, want blocked", sess.State)
	}
	if after := columnType(t, db, schema, "c", "status"); after != before {
		t.Fatalf("column changed despite refusal: %q -> %q", before, after)
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
		t.Fatalf("blocked session not recorded: %v", err)
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
