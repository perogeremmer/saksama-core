//go:build integration

package integration

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// What the plan promises about a native ALTER, in the terms the server can be
// asked about:
//
//	INSTANT           the server accepts ALGORITHM=INSTANT, and no rebuild
//	INPLACE, no cost  the server accepts ALGORITHM=INPLACE (with the promised
//	                  LOCK), and no rebuild
//	INPLACE + rebuild the server accepts ALGORITHM=INPLACE and the rebuild is
//	                  real: the tablespace id changes
//	COPY              the server refuses ALGORITHM=INPLACE, or accepts it and
//	                  visibly rebuilds the table
//
// The COPY direction is the one nobody checked, and it is the one that matters on
// 5.6 and 5.7: if the matrix says COPY where the server can do the change in
// place, Saksama sends the operator to install gh-ost for something MySQL would
// have done itself - and on 5.6 that also means accepting the unobservable
// metadata lock queue. (The INSTANT direction is covered by boundary_test.go.)
//
// "Rebuilt" is measured, not assumed: an in-place rebuild swaps in a new
// tablespace, which changes the table's id in information_schema. A secondary
// index build does not. Write volume was tried first and thrown away: an index
// build writes as much as a rebuild does, so it cannot tell the two apart.
//
// This runs on whatever server SAKSAMA_TEST_DSN points at - 5.6, 5.7, 8.0, 8.4 -
// so a matrix row is wrong on the server it is wrong about. It found two, both on
// 5.6/5.7: default changes and varchar widening were called rebuilds on versions
// whose server does them in place. The byte-prefix line that decides the second
// one (varchar(63) is in place, varchar(64) is not, on a utf8mb4 column) was
// found the same way and is now what R-MODIFY-COLUMN-WIDEN-INPLACE encodes.

type nativeCase struct {
	name  string
	ddl   string
	alter string
}

func nativeCases() []nativeCase {
	base := "CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, " +
		"`a` int NOT NULL, `s` varchar(32) NOT NULL DEFAULT 'x', " +
		"`legacy` int NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB"
	withIndex := "CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, " +
		"`a` int NOT NULL, `s` varchar(32) NOT NULL DEFAULT 'x', PRIMARY KEY (`id`), " +
		"INDEX `ix` (`a`)) ENGINE=InnoDB"
	return []nativeCase{
		{"add column appended", base, "ADD COLUMN `b` int NULL"},
		{"add column in the middle", base, "ADD COLUMN `m` int NULL AFTER `id`"},
		{"drop column", base, "DROP COLUMN `legacy`"},
		{"add index", base, "ADD INDEX `ix` (`a`)"},
		{"drop index", withIndex, "DROP INDEX `ix`"},
		{"widen varchar 32 to 40", base, "MODIFY COLUMN `s` varchar(40) NOT NULL DEFAULT 'x'"},
		{"widen varchar past the byte prefix", base, "MODIFY COLUMN `s` varchar(64) NOT NULL DEFAULT 'x'"},
		{"narrow varchar 32 to 16", base, "MODIFY COLUMN `s` varchar(16) NOT NULL DEFAULT 'x'"},
		{"relax not null to null", base, "MODIFY COLUMN `s` varchar(32) NULL DEFAULT 'x'"},
		{"change column default", base, "ALTER COLUMN `s` SET DEFAULT 'y'"},
		{"int to bigint", base, "MODIFY COLUMN `a` bigint NOT NULL"},
	}
}

func TestNativeClassificationMatchesTheServer(t *testing.T) {
	schema, db := fixtureSchema(t)
	version := serverVersion(t, db)

	native, rebuilds := 0, 0
	for _, c := range nativeCases() {
		c := c
		t.Run(c.name, func(t *testing.T) {
			mustExec(t, db, "DROP TABLE IF EXISTS `t`", c.ddl)
			fillTable(t, db, "t")

			op := predictVerbose(t, db, schema, "t", c.alter)
			method, lock, ruleID := op.Method, op.Lock, op.RuleID
			t.Logf("%s: plan says %s (rule %s, lock %q, rebuild %v)", version, method, ruleID, lock, op.Rebuild)

			switch method {
			case analyzer.MethodInstant:
				native++
				if lock != "" {
					t.Errorf("INSTANT cannot carry a LOCK clause (MySQL error 1221), got %q", lock)
				}
				before := tableID(t, db, schema, "t")
				mustAlter(t, db, "t", c.alter+", ALGORITHM=INSTANT",
					"the plan promised INSTANT and the server refused it")
				if after := tableID(t, db, schema, "t"); after != before {
					t.Errorf("the plan promised a metadata-only INSTANT change, but the server rebuilt the table (table id %s -> %s)",
						before, after)
				}

			case analyzer.MethodInplace:
				native++
				clause := ", ALGORITHM=INPLACE"
				if lock != "" {
					clause += ", LOCK=" + lock
				}
				before := tableID(t, db, schema, "t")
				if err := tryAlter(db, "t", c.alter+clause); err != nil {
					t.Fatalf("the plan promised INPLACE%s and the server refused it: %v", clause, err)
				}
				// The table id is the evidence, not the bytes: a secondary index
				// build writes as much as a rebuild. Rebuild is a separate field
				// from Method precisely because INPLACE does not mean free.
				rebuilt := tableID(t, db, schema, "t") != before
				if op.Rebuild && !rebuilt {
					t.Errorf("the plan says this INPLACE change rewrites every row, but the server did it without rebuilding the table")
				}
				if !op.Rebuild && rebuilt {
					t.Errorf("the plan promised INPLACE without a table copy, but the server rebuilt the table (table id %s -> %s)",
						before, tableID(t, db, schema, "t"))
				}

			case analyzer.MethodCopy:
				rebuilds++
				before := tableID(t, db, schema, "t")
				err := tryAlter(db, "t", c.alter+", ALGORITHM=INPLACE, LOCK=NONE")
				if err != nil {
					// The server cannot avoid a copy, so the price is right.
					t.Logf("server refused in place, as the plan predicted: %v", err)
					return
				}
				after := tableID(t, db, schema, "t")
				if after == before {
					t.Fatalf("the plan said COPY (rule %s) and routed to a copy engine, but %s performs this in place without rebuilding the table (table id %s unchanged): that advice costs an operator a gh-ost install for nothing",
						ruleID, version, before)
				}
				t.Logf("plan said COPY, the server accepted in place and rebuilt the table (table id %s -> %s): the price is right, the label is loose", before, after)
			}
		})
	}

	// A suite that skipped everything proves nothing, and silence looks the same
	// as success.
	if native == 0 {
		t.Fatalf("%s: no operation was classified native; the native path on this version is untested", version)
	}
	if rebuilds == 0 {
		t.Fatalf("%s: no operation was classified as a rebuild; the pessimism half of the matrix is untested", version)
	}
}

// TestNativeApplyRunsOnThisServer is the other half of the claim: the tool can
// execute what it classified as native on this version, not merely describe it.
func TestNativeApplyRunsOnThisServer(t *testing.T) {
	schema, db := fixtureSchema(t)
	version := serverVersion(t, db)

	// Widening past the byte prefix is a rebuild on every version, and on 5.6 a
	// widening is a rebuild outright, so the case list is filtered by what this
	// server's plan says rather than hardcoded per version.
	candidates := []string{
		"ADD INDEX `ix` (`a`)",
		"ALTER COLUMN `s` SET DEFAULT 'y'",
		"MODIFY COLUMN `s` varchar(40) NOT NULL DEFAULT 'x'",
	}

	ran := 0
	for _, alter := range candidates {
		alter := alter
		t.Run(alter, func(t *testing.T) {
			mustExec(t, db, "DROP TABLE IF EXISTS `t`",
				"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, "+
					"`a` int NOT NULL, `s` varchar(32) NOT NULL DEFAULT 'x', PRIMARY KEY (`id`)) ENGINE=InnoDB")
			mustExec(t, db, "INSERT INTO `t` (`a`) VALUES (1), (2), (3), (4), (5)")

			method, lock, ruleID := predict(t, db, schema, "t", alter)
			if method == analyzer.MethodCopy || method == analyzer.MethodUnsupported {
				t.Skipf("%s: this version plans %s as %s (rule %s), so there is nothing native to run", version, alter, method, ruleID)
			}
			if lock != "" && method == analyzer.MethodInplace {
				// expected, logged below
			}

			runner, store := newRunner(t)
			sess, runErr := runApply(t, runner, db, applyRequest(schema, "t", alter))
			if runErr != nil {
				t.Fatalf("%s: apply refused a change the plan calls native: %v", version, runErr)
			}
			if sess.State != state.StateDone {
				t.Fatalf("%s: state = %q, error = %q", version, sess.State, sess.Error)
			}
			if len(sess.Statements) == 0 {
				t.Fatal("no statement recorded")
			}
			want := "ALGORITHM=" + string(method)
			if got := sess.Statements[0].SQL; !strings.Contains(got, want) {
				t.Errorf("expected %q in the executed statement, got %s", want, got)
			}
			for _, v := range sess.Verification {
				if !v.OK {
					t.Errorf("verify after failed: %s", v.Detail)
				}
			}
			if _, loadErr := store.Load(sess.ID); loadErr != nil {
				t.Fatalf("session was not persisted: %v", loadErr)
			}
			ran++
		})
	}

	if ran < 2 {
		t.Fatalf("%s: only %d native change(s) ran end to end; the native path is barely covered", version, ran)
	}
}

// fillTable gives the server rows to rewrite, so a rebuild is a real rebuild.
func fillTable(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	mustExec(t, db, "INSERT INTO `"+table+"` (`a`, `s`) VALUES (1,'x'),(2,'x'),(3,'x'),(4,'x'),(5,'x')")
	for i := 0; i < 11; i++ {
		mustExec(t, db, "INSERT INTO `"+table+"` (`a`, `s`) SELECT `a`, `s` FROM `"+table+"`")
	}
}

// tableID reads InnoDB's internal id for a table.
//
// An in-place rebuild creates a new tablespace and swaps it in, so the id
// changes. Anything that only touches metadata or builds a secondary index
// leaves it alone. This is the deterministic evidence of "no table copy"; the
// byte counters are not, because an index build writes as much as a rebuild.
func tableID(t *testing.T, db *sql.DB, schema, table string) string {
	t.Helper()
	name := schema + "/" + table
	// 8.0 renamed the table; 5.6 and 5.7 only have the old name.
	for _, query := range []string{
		"SELECT TABLE_ID FROM information_schema.INNODB_TABLES WHERE NAME = ?",
		"SELECT TABLE_ID FROM information_schema.INNODB_SYS_TABLES WHERE NAME = ?",
	} {
		var id sql.NullString
		err := db.QueryRow(query, name).Scan(&id)
		if err == nil {
			return id.String
		}
	}
	t.Fatalf("cannot read the table id for %s from information_schema", name)
	return ""
}
