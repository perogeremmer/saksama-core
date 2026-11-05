//go:build integration

package integration

import (
	"context"
	"database/sql"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/model"
)

// The capability matrix is a claim about what MySQL does at a given version, and
// a claim nobody checks is a guess with a rule id attached. These tests ask the
// server to perform the ALTER the plan predicted, and hold the prediction to the
// answer.
//
// They are version-independent on purpose: the same assertions run against 5.6,
// 5.7, 8.0.28, 8.0.29 and 8.4, and a wrong row in the matrix fails on the server
// it is wrong about. The boundary these exist for is 8.0.29:
//
//	8.0.12 - 8.0.28  ADD COLUMN INSTANT only when appended last
//	8.0.29+          ADD COLUMN INSTANT in any position, and DROP COLUMN INSTANT
//
// Verified combinations live in docs/COMPATIBILITY.md.

type instantBoundaryCase struct {
	name string
	// ddl builds a fresh table per case, so a failure names one ALTER and nothing
	// else.
	ddl   string
	alter string
}

func TestInstantBoundaryMatchesTheServer(t *testing.T) {
	cases := []instantBoundaryCase{
		{
			name:  "add column appended",
			ddl:   "`id` bigint unsigned NOT NULL AUTO_INCREMENT, `status` varchar(32) NOT NULL DEFAULT 'pending', PRIMARY KEY (`id`)",
			alter: "ADD COLUMN `note` varchar(64) NULL",
		},
		{
			name:  "add column in the middle",
			ddl:   "`id` bigint unsigned NOT NULL AUTO_INCREMENT, `status` varchar(32) NOT NULL DEFAULT 'pending', PRIMARY KEY (`id`)",
			alter: "ADD COLUMN `mid` int NULL AFTER `id`",
		},
		{
			name:  "drop column",
			ddl:   "`id` bigint unsigned NOT NULL AUTO_INCREMENT, `status` varchar(32) NOT NULL DEFAULT 'pending', `legacy` int NULL, PRIMARY KEY (`id`)",
			alter: "DROP COLUMN `legacy`",
		},
		{
			name:  "change column default",
			ddl:   "`id` bigint unsigned NOT NULL AUTO_INCREMENT, `status` varchar(32) NOT NULL DEFAULT 'pending', PRIMARY KEY (`id`)",
			alter: "ALTER COLUMN `status` SET DEFAULT 'queued'",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			schema, db := fixtureSchema(t)
			mustExec(t, db, "CREATE TABLE `t` ("+c.ddl+") ENGINE=InnoDB")
			// Rows matter: a rebuild is a rebuild whether or not anyone is looking,
			// and INSTANT has to be metadata-only on a populated table.
			mustExec(t, db, "INSERT INTO `t` (`status`) VALUES ('a'), ('b'), ('c')")

			method, lock, ruleID := predict(t, db, schema, "t", c.alter)
			t.Logf("plan says %s (rule %s, lock %q)", method, ruleID, lock)

			switch method {
			case analyzer.MethodInstant:
				// MySQL rejects ALGORITHM=INSTANT combined with a LOCK clause
				// (error 1221), so the matrix must not emit one.
				if lock != "" {
					t.Errorf("INSTANT must not carry a LOCK clause: %q", lock)
				}
				mustAlter(t, db, "t", c.alter+", ALGORITHM=INSTANT",
					"the plan promised INSTANT and the server refused it")
			case analyzer.MethodInplace:
				clause := ", ALGORITHM=INPLACE"
				if lock != "" {
					clause += ", LOCK=" + lock
				}
				mustAlter(t, db, "t", c.alter+clause,
					"the plan promised INPLACE and the server refused it")
			case analyzer.MethodCopy:
				// The useful half of "this needs a rebuild" is that MySQL cannot do
				// it instantly. If INSTANT works, the matrix is pessimistic and the
				// tool is recommending a copy nobody needs.
				if err := tryAlter(db, "t", c.alter+", ALGORITHM=INSTANT"); err == nil {
					t.Fatalf("the plan said COPY, but the server performed the change with ALGORITHM=INSTANT")
				}
			default:
				t.Fatalf("unexpected method %q for %s", method, c.alter)
			}
		})
	}
}

// The boundary itself, stated explicitly instead of only round-tripped: whichever
// side of 8.0.29 the server is on, the middle-column case and the drop case must
// follow the matrix for that version.
func TestInstantBoundarySidesAgreeWithTheVersion(t *testing.T) {
	schema, db := fixtureSchema(t)
	raw := serverVersion(t, db)
	srv, err := model.ParseVersion(raw)
	if err != nil {
		t.Fatalf("server version %q: %v", raw, err)
	}

	mustExec(t, db, "CREATE TABLE `mid` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, "+
		"`status` varchar(32) NOT NULL DEFAULT 'pending', PRIMARY KEY (`id`)) ENGINE=InnoDB")
	mustExec(t, db, "CREATE TABLE `dropme` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, "+
		"`legacy` int NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB")

	mid := predictVerbose(t, db, schema, "mid", "ADD COLUMN `mid` int NULL AFTER `id`")
	drop := predictVerbose(t, db, schema, "dropme", "DROP COLUMN `legacy`")

	// The cost is the claim that has to hold on every version: below the INSTANT
	// boundary both of these rewrite the table, whatever algorithm the server uses
	// for it. Method only says INSTANT from 8.0.29 on.
	switch {
	case srv.AtLeast(8, 0, 29):
		if mid.Method != analyzer.MethodInstant {
			t.Errorf("%s: middle ADD COLUMN must be INSTANT (rule %s), got %s", raw, mid.RuleID, mid.Method)
		}
		if drop.Method != analyzer.MethodInstant {
			t.Errorf("%s: DROP COLUMN must be INSTANT (rule %s), got %s", raw, drop.RuleID, drop.Method)
		}
		if mid.Rebuild || drop.Rebuild {
			t.Errorf("%s: INSTANT changes must not be priced as rebuilds", raw)
		}
	case srv.AtLeast(8, 0, 12):
		// 8.0.12 - 8.0.28: the appended case is INSTANT, these two rebuild.
		if !mid.Rebuild || !drop.Rebuild {
			t.Errorf("%s: both cases must be a rebuild before 8.0.29, got rebuild=%v / %v",
				raw, mid.Rebuild, drop.Rebuild)
		}
	default:
		if !mid.Rebuild || !drop.Rebuild {
			t.Errorf("%s: both cases must be a rebuild before 8.0.12, got rebuild=%v / %v (methods %s / %s)",
				raw, mid.Rebuild, drop.Rebuild, mid.Method, drop.Method)
		}
	}
}

// predict asks the analyzer what it thinks of one ALTER on a live table.
//
// The third value is the algorithm, the second the lock; callers that care about
// cost read plan.Rebuild through predictVerbose.
func predict(t *testing.T, db *sql.DB, schema, table, alter string) (analyzer.Method, string, string) {
	t.Helper()
	op := predictVerbose(t, db, schema, table, alter)
	return op.Method, op.Lock, op.RuleID
}

// predictVerbose returns the whole analyzer verdict, including whether the
// statement rebuilds the table - which is a different fact from the algorithm the
// server runs, and the one that decides between native and a copy engine.
func predictVerbose(t *testing.T, db *sql.DB, schema, table, alter string) analyzer.OpPlan {
	t.Helper()
	runner, _ := newRunner(t)
	runner.DB = db

	prep, err := runner.Analyze(context.Background(), applyRequest(schema, table, alter))
	if err != nil {
		t.Fatalf("analyze %q: %v", alter, err)
	}
	if len(prep.Plan.Operations) != 1 {
		t.Fatalf("expected one operation for %q, got %d", alter, len(prep.Plan.Operations))
	}
	return prep.Plan.Operations[0]
}

func mustAlter(t *testing.T, db *sql.DB, table, alter, failure string) {
	t.Helper()
	if err := tryAlter(db, table, alter); err != nil {
		t.Fatalf("%s: %v", failure, err)
	}
}

func tryAlter(db *sql.DB, table, alter string) error {
	_, err := db.Exec("ALTER TABLE `" + table + "` " + alter)
	return err
}

func serverVersion(t *testing.T, db *sql.DB) string {
	t.Helper()
	var v string
	if err := db.QueryRow("SELECT @@version").Scan(&v); err != nil {
		t.Fatalf("server version: %v", err)
	}
	return v
}
