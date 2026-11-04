//go:build integration

package integration

import (
	"context"
	"database/sql"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/core"
	"github.com/perogeremmer/saksama-core/internal/introspect"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// Recovery is the loudest claim in the README - "honest recovery" - and until now
// it was tested against a stub shell script and hand-written session records. A
// stub creates none of the objects a real engine does, so nothing verified that
// the tool recognises the debris it will actually meet.
//
// These tests kill a real engine mid-copy and then hold reconcile and cleanup to
// what the database really contains. The interesting assertion is the ownership
// one: every object the engine left behind must be recorded by the session, or
// cleanup can never remove it and the debris silently outlives the incident.

// copyEngine is one engine that can actually run here, with the objects it is
// known to create. The names are not guesses: they were observed on the versions
// this repository verifies (gh-ost 1.1.11, pt-osc 3.7.2).
type copyEngine struct {
	name string
	// hint is the analyzer engine name used to pin the plan to this engine.
	hint analyzer.Engine
	// leaves are the table suffixes the engine creates during a copy.
	leaves []string
	// triggers says whether the engine creates triggers on the original table.
	triggers bool
}

func copyEngines(t *testing.T) []copyEngine {
	t.Helper()
	var out []copyEngine
	if _, err := exec.LookPath("gh-ost"); err == nil {
		out = append(out, copyEngine{
			name: "gh-ost", hint: analyzer.EngineGhOst,
			leaves: []string{"_gho", "_ghc"}, triggers: false,
		})
	}
	if _, err := exec.LookPath("pt-online-schema-change"); err == nil {
		out = append(out, copyEngine{
			name: "pt-osc", hint: analyzer.EnginePtOSC,
			leaves: []string{"_new"}, triggers: true,
		})
	}
	if len(out) == 0 {
		t.Skip("neither gh-ost nor pt-online-schema-change is on PATH")
	}
	return out
}

// slowCopyRequest is a rebuild pinned to one engine, detached, and chunked one row
// at a time so the copy is still running when the test kills it. A copy that
// finishes first would test nothing.
func slowCopyRequest(schema, engine string, hint analyzer.Engine) core.Request {
	req := applyRequest(schema, "t", "MODIFY COLUMN `status` varchar(64) NOT NULL")
	req.EngineHint = hint
	req.EngineHintSource = "recorded"
	req.ForExecution = true
	req.Detach = true
	req.ChunkSize = 1
	req.FreeDisk = 200 << 30
	req.DiskTotal = 500 << 30
	return req
}

// startSlowCopy runs the application in the background and waits until the engine
// has created its shadow objects, which is the point of no return for this test:
// from here on there is debris whether or not the process survives.
func startSlowCopy(t *testing.T, db *sql.DB, store *state.Store, req core.Request, eng copyEngine, table string) (sessionID string, pid int, done <-chan error) {
	t.Helper()

	runner, _ := newRunner(t)
	runner.DB = db
	runner.Store = store

	finished := make(chan error, 1)
	go func() {
		finished <- func() error {
			_, err := runner.Apply(context.Background(), req)
			return err
		}()
	}()

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		sessions, err := store.List()
		if err != nil {
			t.Fatalf("list sessions: %v", err)
		}
		if len(sessions) == 1 && sessions[0].EnginePID != 0 {
			if artifactsPresent(db, table, eng.leaves) {
				return sessions[0].ID, sessions[0].EnginePID, finished
			}
		}
		select {
		case err := <-finished:
			t.Fatalf("%s finished before the test could interrupt it (%v); the copy needs to be slower than the test", eng.name, err)
		default:
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s did not create its shadow objects within 90s", eng.name)
	return "", 0, nil
}

func artifactsPresent(db *sql.DB, table string, suffixes []string) bool {
	for _, s := range suffixes {
		var n int
		_ = db.QueryRow(
			"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?",
			"_"+table+s).Scan(&n)
		if n == 0 {
			return false
		}
	}
	return true
}

// realArtifacts is what the server actually holds, by the engine's own naming,
// read from information_schema rather than from anything the tool recorded.
func realArtifacts(db *sql.DB, eng copyEngine) []string {
	var out []string
	rows, err := db.Query(`
		SELECT TABLE_NAME FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME LIKE '\\_%\\_%'
		ORDER BY TABLE_NAME`)
	if err == nil {
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil && matchesEngine(eng, n) {
				out = append(out, n)
			}
		}
		rows.Close()
	}
	if eng.triggers {
		rows, err = db.Query(`
			SELECT TRIGGER_NAME FROM information_schema.TRIGGERS
			WHERE TRIGGER_SCHEMA = DATABASE() ORDER BY TRIGGER_NAME`)
		if err == nil {
			for rows.Next() {
				var n string
				if rows.Scan(&n) == nil {
					out = append(out, n)
				}
			}
			rows.Close()
		}
	}
	return out
}

func matchesEngine(eng copyEngine, name string) bool {
	for _, s := range []string{"_gho", "_ghc", "_ghk", "_del", "_new", "_old"} {
		if !strings.HasSuffix(name, s) {
			continue
		}
		// _new/_old belong to pt-osc, _gho/_ghc/_ghk/_del to gh-ost.
		if strings.HasPrefix(s, "_gh") || s == "_del" {
			return eng.hint == analyzer.EngineGhOst
		}
		return eng.hint == analyzer.EnginePtOSC
	}
	return false
}

// TestKilledCopyLeavesOnlyOwnedDebris kills a real copy engine with SIGKILL and
// then asks the tool to deal with the mess.
func TestKilledCopyLeavesOnlyOwnedDebris(t *testing.T) {
	for _, eng := range copyEngines(t) {
		t.Run(eng.name, func(t *testing.T) {
			schema, db := fixtureSchema(t)
			// Large enough that one-row chunks cannot finish inside the window between
			// seeing the shadow table and killing the process. A smaller fixture made
			// this test fail on a fast runner for the right reason - the copy was
			// already over - which is a test that proves nothing.
			createCopyFixture(t, db, 200000)

			store, err := state.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			id, pid, finished := startSlowCopy(t, db, store, slowCopyRequest(schema, eng.name, eng.hint), eng, "t")

			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
				t.Fatalf("kill engine pid %d: %v", pid, err)
			}
			select {
			case <-finished:
			case <-time.After(60 * time.Second):
				t.Fatal("the wrapper did not notice the killed engine")
			}

			// 1. Honest recovery starts with the production table being untouched.
			if n := countRows(t, db, "SELECT COUNT(*) FROM `t`"); n == 0 {
				t.Fatal("the production table lost its rows")
			}
			if col := columnType(t, db, schema, "t", "status"); !strings.HasPrefix(col, "varchar(32)") {
				t.Fatalf("the table was swapped before the kill: status is %q", col)
			}

			// 2. The debris is real, and every piece of it must be owned.
			real := realArtifacts(db, eng)
			if len(real) == 0 {
				t.Fatalf("%s left no visible artifacts; the test is not exercising a crash", eng.name)
			}
			sess, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			recorded := map[string]bool{}
			for _, a := range sess.Artifacts {
				recorded[a.Name] = true
			}
			for _, name := range real {
				if !recorded[name] {
					t.Errorf("%s left %q behind and the session does not own it, so cleanup cannot remove it (recorded: %v)",
						eng.name, name, keysOf(recorded))
				}
			}

			// 3. reconcile must observe the same thing the database says.
			runner, _ := newRunner(t)
			runner.DB = db
			runner.Store = store
			findings, err := runner.Reconcile(context.Background())
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			var found *core.Finding
			for i := range findings {
				if findings[i].Session == id {
					found = &findings[i]
				}
			}
			if found == nil {
				t.Fatalf("reconcile did not report the killed session %s: %+v", id, findings)
			}
			if found.EngineAlive {
				t.Error("reconcile reports a killed engine as alive")
			}
			if len(found.Observed) == 0 {
				t.Errorf("reconcile saw none of the artifacts the database holds: %v", real)
			}

			// 4. cleanup drops what the session owns, and stops when nothing is left.
			dry, err := runner.Cleanup(context.Background(), id, true)
			if err != nil {
				t.Fatalf("cleanup dry run: %v", err)
			}
			if len(dry.Items) == 0 {
				t.Fatalf("cleanup dry run listed nothing (refused: %q)", dry.Refused)
			}
			clean, err := runner.Cleanup(context.Background(), id, false)
			if err != nil {
				t.Fatalf("cleanup: %v", err)
			}
			if left := realArtifacts(db, eng); len(left) > 0 {
				var detail []string
				for _, it := range clean.Items {
					state := "dropped"
					if it.Err != "" {
						state = "error: " + it.Err
					} else if !it.Dropped {
						state = "not dropped"
					}
					detail = append(detail, it.Kind+" "+it.Name+" -> "+state)
				}
				t.Errorf("cleanup left %v behind; it reported: %v", left, detail)
			}
			if _, err := introspect.NewCollector(db, schema, "t").Collect(context.Background()); err != nil {
				t.Fatalf("the table must still be readable after cleanup: %v", err)
			}
		})
	}
}

// TestCancelledCopyIsReportedHonestly is the same interruption done politely. A
// graceful stop often lets the engine drop its own objects, so the assertion is
// not "debris exists" but "whatever exists is owned and described".
func TestCancelledCopyIsReportedHonestly(t *testing.T) {
	for _, eng := range copyEngines(t) {
		t.Run(eng.name, func(t *testing.T) {
			schema, db := fixtureSchema(t)
			createCopyFixture(t, db, 200000)

			store, err := state.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			id, _, finished := startSlowCopy(t, db, store, slowCopyRequest(schema, eng.name, eng.hint), eng, "t")

			runner, _ := newRunner(t)
			runner.DB = db
			runner.Store = store
			res, err := runner.Cancel(context.Background(), id, false)
			if err != nil {
				t.Fatalf("cancel: %v", err)
			}
			if res.Outcome != core.CancelStopped {
				t.Fatalf("cancel outcome %q, want stopped", res.Outcome)
			}
			if !strings.Contains(res.Note, "not swapped") {
				t.Errorf("the note must say the production table was not swapped: %q", res.Note)
			}
			select {
			case <-finished:
			case <-time.After(60 * time.Second):
				t.Fatal("the wrapper did not notice the cancelled engine")
			}

			if col := columnType(t, db, schema, "t", "status"); !strings.HasPrefix(col, "varchar(32)") {
				t.Fatalf("a cancelled copy swapped the table: status is %q", col)
			}

			// Whatever survives the graceful stop has to be owned, or the operator
			// is told the incident is over while the disk usage is not.
			sess, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			recorded := map[string]bool{}
			for _, a := range sess.Artifacts {
				recorded[a.Name] = true
			}
			for _, name := range realArtifacts(db, eng) {
				if !recorded[name] {
					t.Errorf("%s survived the cancel with %q, which the session does not own (recorded: %v)",
						eng.name, name, keysOf(recorded))
				}
			}
		})
	}
}

// createCopyFixture builds a table the copy engine has real work to do on: enough
// rows that a one-row chunk size keeps it busy for minutes.
func createCopyFixture(t *testing.T, db *sql.DB, rows int) {
	t.Helper()
	mustExec(t, db, "CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, "+
		"`status` varchar(32) NOT NULL DEFAULT 'pending', PRIMARY KEY (`id`)) ENGINE=InnoDB")
	mustExec(t, db, "INSERT INTO `t` (`status`) VALUES ('x')")
	for inserted := 1; inserted < rows; inserted *= 2 {
		mustExec(t, db, "INSERT INTO `t` (`status`) SELECT `status` FROM `t`")
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `t`"); n < rows {
		t.Fatalf("fixture has %d rows, want at least %d", n, rows)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
