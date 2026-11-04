//go:build integration

package integration

import (
	"context"
	"database/sql"
	"os/exec"
	"strings"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// requirePtOSC skips when the engine is not installed.
//
// The whole toolkit is not needed: pt-online-schema-change is a single Perl file.
// A CI job installs perl, DBD::mysql, and that one file.
func requirePtOSC(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("pt-online-schema-change")
	if err != nil {
		t.Skip("pt-online-schema-change is not on PATH")
	}
	return path
}

func ptOSCVersion(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("pt-osc --version: %v (%s)", err, out)
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

// runPtOSC applies one change with the real engine and returns the session.
func runPtOSC(t *testing.T, db *sql.DB, schema, table, alter string, ptoscPath string) (*state.Session, error) {
	t.Helper()

	runner, store := newRunner(t)
	runner.DB = db
	req := applyRequest(schema, table, alter)
	req.EngineHint = analyzer.EnginePtOSC
	req.EngineHintSource = "recorded"
	req.ForExecution = true
	// The data directory is inside a container, so the volume cannot be measured
	// from the test process; supply the numbers instead of forcing past the check.
	req.FreeDisk = 200 << 30
	req.DiskTotal = 500 << 30
	req.Binaries = map[string]analyzer.EngineBinary{
		"pt-online-schema-change": {
			Name: "pt-online-schema-change", Available: true,
			Path: ptoscPath, Version: ptOSCVersion(t, ptoscPath),
		},
	}
	_ = store
	return runner.Apply(context.Background(), req)
}

func TestPtOSCAddColumnOnPlainTable(t *testing.T) {
	ptosc := requirePtOSC(t)
	schema, db := fixtureSchema(t)

	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `status` varchar(32) NOT NULL, "+
			"PRIMARY KEY (`id`)) ENGINE=InnoDB",
		"INSERT INTO `t` (`status`) VALUES ('a'), ('b'), ('c')",
	)

	sess, err := runPtOSC(t, db, schema, "t", "ADD COLUMN `note` varchar(64) NULL", ptosc)
	if err != nil {
		t.Fatalf("apply: %v (session error: %s)", err, sess.Error)
	}
	if sess.State != state.StateDone {
		t.Fatalf("state = %q", sess.State)
	}
	if sess.Engine != "pt-osc" {
		t.Fatalf("engine = %q", sess.Engine)
	}

	if n := countRows(t, db,
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 't' AND COLUMN_NAME = 'note'",
		schema); n != 1 {
		t.Fatal("the column was not created")
	}
	// A copy that loses rows is worse than a failure.
	if n := countRows(t, db, "SELECT COUNT(*) FROM `"+schema+"`.`t`"); n != 3 {
		t.Fatalf("rows after the copy = %d, want 3", n)
	}
	// The engine must not leave anything behind on success.
	if n := countRows(t, db,
		"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME LIKE '\\_t\\_%'",
		schema); n != 0 {
		t.Fatalf("leftover shadow tables: %d", n)
	}
	if n := countRows(t, db,
		"SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? AND TRIGGER_NAME LIKE 'pt_osc\\_%'",
		schema); n != 0 {
		t.Fatalf("leftover triggers: %d", n)
	}
}

// A table with foreign keys is exactly why pt-osc exists: gh-ost cannot do it.
func TestPtOSCKeepsForeignKeys(t *testing.T) {
	ptosc := requirePtOSC(t)
	schema, db := fixtureSchema(t)

	mustExec(t, db,
		"CREATE TABLE `p` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, PRIMARY KEY (`id`)) ENGINE=InnoDB",
		"CREATE TABLE `c` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `pid` bigint unsigned NOT NULL, "+
			"PRIMARY KEY (`id`), KEY `k_pid` (`pid`), "+
			"CONSTRAINT `fk_c_p` FOREIGN KEY (`pid`) REFERENCES `p` (`id`) ON DELETE CASCADE) ENGINE=InnoDB",
		"INSERT INTO `p` VALUES (1), (2)",
		"INSERT INTO `c` (`pid`) VALUES (1), (2)",
	)

	sess, err := runPtOSC(t, db, schema, "c", "ADD COLUMN `note` varchar(64) NULL", ptosc)
	if err != nil {
		t.Fatalf("apply: %v (session error: %s)", err, sess.Error)
	}
	if sess.State != state.StateDone {
		t.Fatalf("state = %q: %s", sess.State, sess.Error)
	}

	// The constraint must survive, with its action, whatever it is called now.
	// Constraint names are not stable across pt-osc versions, which is why the
	// verifier matches on columns and referenced table.
	var deleteRule string
	if err := db.QueryRow(`
		SELECT DELETE_RULE FROM information_schema.REFERENTIAL_CONSTRAINTS
		WHERE CONSTRAINT_SCHEMA = ? AND TABLE_NAME = 'c' AND REFERENCED_TABLE_NAME = 'p'`,
		schema).Scan(&deleteRule); err != nil {
		t.Fatalf("the foreign key is gone: %v", err)
	}
	if deleteRule != "CASCADE" {
		t.Fatalf("ON DELETE became %q", deleteRule)
	}

	if n := countRows(t, db, "SELECT COUNT(*) FROM `"+schema+"`.`c`"); n != 2 {
		t.Fatalf("rows after the copy = %d, want 2", n)
	}
}

// gh-ost cannot chunk a composite key. pt-osc can, so this is the case where the
// engine matrix sends you to pt-osc for a table with no foreign keys at all.
func TestPtOSCHandlesCompositePrimaryKey(t *testing.T) {
	ptosc := requirePtOSC(t)
	schema, db := fixtureSchema(t)

	mustExec(t, db,
		"CREATE TABLE `m` (`a` bigint unsigned NOT NULL, `b` bigint unsigned NOT NULL, PRIMARY KEY (`a`,`b`)) ENGINE=InnoDB",
		"INSERT INTO `m` VALUES (1,1), (1,2), (2,1)",
	)

	sess, err := runPtOSC(t, db, schema, "m", "ADD COLUMN `note` varchar(16) NULL", ptosc)
	if err != nil {
		t.Fatalf("apply: %v (session error: %s)", err, sess.Error)
	}
	if sess.State != state.StateDone {
		t.Fatalf("state = %q", sess.State)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `"+schema+"`.`m`"); n != 3 {
		t.Fatalf("rows after the copy = %d, want 3", n)
	}
}

// A dry run must describe the work, not perform it.
func TestPtOSCDryRunChangesNothing(t *testing.T) {
	ptosc := requirePtOSC(t)
	schema, db := fixtureSchema(t)

	mustExec(t, db,
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, PRIMARY KEY (`id`)) ENGINE=InnoDB")

	runner, _ := newRunner(t)
	runner.DB = db
	req := applyRequest(schema, "t", "ADD COLUMN `note` varchar(64) NULL")
	req.EngineHint = analyzer.EnginePtOSC
	req.EngineHintSource = "recorded"
	req.DryRun = true
	req.ForExecution = true
	req.FreeDisk = 200 << 30
	req.DiskTotal = 500 << 30
	req.Binaries = map[string]analyzer.EngineBinary{
		"pt-online-schema-change": {Name: "pt-online-schema-change", Available: true, Path: ptosc},
	}

	sess, err := runner.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(sess.Statements) == 0 {
		t.Fatal("dry run produced no statement")
	}
	if !strings.Contains(sess.Statements[0].SQL, "--print") {
		t.Fatalf("a dry run must use --print: %s", sess.Statements[0].SQL)
	}
	if n := countRows(t, db,
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 't' AND COLUMN_NAME = 'note'",
		schema); n != 0 {
		t.Fatal("the dry run created the column")
	}
}
