//go:build integration

package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/core"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// This is the only test in the repository that runs a migration at a size the
// product is written for.
//
// Everything else is fixture-sized: the volume test copies about 13 MiB, which
// proves the engine loses nothing but proves nothing about minutes of work. The
// checks that matter for a real table - the online alter log filling, the duration
// becoming measurable, the event log staying small, cleanup returning the disk - only
// exist once the copy takes minutes and the table is bigger than memory.
//
// It is opt-in and nightly, because it writes a few GiB and takes minutes:
//
//	SAKSAMA_SCALE_TESTS=1 SAKSAMA_SCALE_GB=2 go test -tags=integration \
//	  -run TestProductionShapeCopy ./test/integration/
//
// The default is 2 GiB, which is a real multi-minute copy for gh-ost and still fits
// a hosted runner's disk: the table, its ghost copy, and the binary log the copy
// itself generates all land in the same volume, so the peak is roughly three times
// the table. Raise SAKSAMA_SCALE_GB on a machine with room, and note that D13's
// 8 GiB threshold only becomes reachable above 8 - it is covered by unit test with a
// 22 GiB fixture, not here.
func TestProductionShapeCopy(t *testing.T) {
	if os.Getenv("SAKSAMA_SCALE_TESTS") != "1" {
		t.Skip("set SAKSAMA_SCALE_TESTS=1 to run the scale test (nightly, not per pull request)")
	}
	engines := copyEngines(t)
	var gh *copyEngine
	for i := range engines {
		if engines[i].hint == analyzer.EngineGhOst {
			gh = &engines[i]
		}
	}
	if gh == nil {
		t.Skip("gh-ost is not installed; the held-cutover half of this test needs it")
	}

	// Fractional on purpose: 0.05 GiB is enough to exercise every assertion in this
	// test in seconds, which is how it gets debugged without waiting fifteen minutes.
	gb := 2.0
	if v := os.Getenv("SAKSAMA_SCALE_GB"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n <= 0 {
			t.Fatalf("SAKSAMA_SCALE_GB=%q is not a positive number", v)
		}
		gb = n
	}

	schema, db := fixtureSchema(t)
	created := time.Now()
	rows := createScaleFixture(t, db, gb)
	t.Logf("fixture: %d rows, %.1f GiB, built in %s", rows, float64(tableBytes(t, db, schema, "t"))/(1<<30), time.Since(created).Round(time.Second))

	// --- the plan before any history ---------------------------------------
	//
	// No measurement exists yet, so the estimate is the heuristic and says so. This
	// half is what makes the second assertion meaningful: the same input has to
	// change after one real run.
	runner, store := newRunner(t)
	runner.DB = db
	before, err := runner.Analyze(context.Background(), scaleRequest(schema, gh))
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if before.Plan.Duration.Observations != 0 {
		t.Fatalf("a fresh state directory cannot have observations: %+v", before.Plan.Duration)
	}
	if strings.Contains(before.Plan.Duration.Basis, "measured") {
		t.Fatalf("an estimate with no history must not claim a measurement: %q", before.Plan.Duration.Basis)
	}
	if before.Plan.Recommendation.Engine != analyzer.EngineGhOst {
		t.Fatalf("engine = %q, want gh-ost for a rebuild under hold-cutover", before.Plan.Recommendation.Engine)
	}
	t.Logf("planned estimate before any run: %s - %s (%s)",
		analyzer.HumanDuration(before.Plan.Duration.FromSeconds),
		analyzer.HumanDuration(before.Plan.Duration.ToSeconds),
		before.Plan.Duration.Confidence)

	// --- the run, with the swap held ---------------------------------------
	//
	// A writer runs throughout: the point of a production-shaped copy is that the
	// table is not quiet. It stops only when the migration is over, so the last
	// write races the swap deliberately.
	stopWriter := make(chan struct{})
	writerDone := make(chan int64, 1)
	var written int64
	go func() {
		n := int64(0)
		for {
			select {
			case <-stopWriter:
				writerDone <- n
				return
			default:
			}
			if _, err := db.Exec("INSERT INTO `t` (`status`, `payload`) VALUES ('live', REPEAT('y', 200))"); err != nil {
				break
			}
			n++
			time.Sleep(20 * time.Millisecond)
		}
		writerDone <- n
	}()
	defer func() {
		select {
		case <-stopWriter:
		default:
			close(stopWriter)
		}
		select {
		case n := <-writerDone:
			written = n
		case <-time.After(10 * time.Second):
		}
	}()

	applied := make(chan applyResult, 1)
	go func() {
		sess, err := runner.Apply(context.Background(), scaleRequest(schema, gh))
		applied <- applyResult{sess: sess, err: err}
	}()

	// Wait for the copy to finish and the swap to be held: the ghost table reaching
	// the production row count is the observable an operator would use, and it does
	// not depend on any wording in the engine's log.
	sessionID := waitForSession(t, store, 5*time.Minute)
	waitForCopyComplete(t, db, schema, "t", sessionID, 30*time.Minute)

	sess, err := store.Load(sessionID)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if sess.CutoverFlagFile == "" {
		t.Fatalf("the copy finished but no cutover gate is recorded; the run was not held: %+v", sess)
	}
	if _, statErr := os.Stat(sess.CutoverFlagFile); statErr != nil {
		t.Fatalf("the recorded gate does not exist: %v", statErr)
	}
	// The swap must not have happened yet. This is the product's whole promise, at a
	// size where the copy is worth pausing for.
	if got := columnType(t, db, schema, "t", "status"); !strings.HasPrefix(got, "varchar(32)") {
		t.Fatalf("the table was swapped while the gate was held: status is %q", got)
	}

	// --- release ------------------------------------------------------------
	if _, err := runner.ReleaseCutover(context.Background(), sessionID); err != nil {
		t.Fatalf("release cutover: %v", err)
	}

	select {
	case res := <-applied:
		if res.err != nil {
			t.Fatalf("apply after the release: %v", res.err)
		}
	case <-time.After(15 * time.Minute):
		t.Fatal("apply never returned after the cutover was released")
	}

	close(stopWriter)
	select {
	case written = <-writerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the writer did not stop")
	}

	// --- what a production-shaped run has to be true ------------------------
	if got := columnType(t, db, schema, "t", "status"); !strings.HasPrefix(got, "varchar(64)") {
		t.Fatalf("the release did not swap the table: status is %q", got)
	}
	// Every row the writer committed before its last statement, plus the fixture.
	got := int64(countRows(t, db, "SELECT COUNT(*) FROM `t`"))
	if got < rows {
		t.Fatalf("the swap lost rows: %d after, at least %d before", got, rows)
	}
	// The writer's own count is the claim that matters: nothing it committed may be
	// missing after the swap. gh-ost tolerates a boundary race on the final chunk
	// the way any online copy does, so the assertion is against what it reported
	// committing, minus the one statement that may have raced the swap.
	if got < rows+written-1 {
		t.Fatalf("committed writes were lost: %d rows, want at least %d (fixture %d + writer %d)",
			got, rows+written-1, rows, written)
	}
	t.Logf("post-swap: %d rows, writer committed %d during the copy", got, written)

	// The finished session is the measurement the estimator reads next time.
	if sess, err := store.Load(sessionID); err != nil || sess.State != state.StateDone {
		t.Fatalf("the session did not finish cleanly: %+v err=%v", sess, err)
	}

	// --- the old table is kept, and cleanup is what returns it --------------
	//
	// gh-ost keeps the pre-migration table as _t_del after the swap: that is the
	// rollback window, and it is also disk the operator wants back. Two claims meet
	// here - the leftover must be recorded as owned, or cleanup would never remove
	// it, and removing it must actually return the space.
	const delTable = "_t_del"
	if !tableExists(t, db, schema, delTable) {
		t.Fatalf("gh-ost did not keep %s after the swap; the rollback window is missing", delTable)
	}
	finished, err := store.Load(sessionID)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	owned := false
	for _, a := range finished.Artifacts {
		if a.Name == delTable {
			owned = true
		}
	}
	if !owned {
		t.Fatalf("the leftover table is not recorded as owned, so cleanup could never remove it: %+v", finished.Artifacts)
	}

	beforeCleanup := schemaBytes(t, db, schema)
	clean, err := runner.Cleanup(context.Background(), sessionID, false)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	dropped := false
	for _, it := range clean.Items {
		if it.Name == delTable && it.Dropped {
			dropped = true
		}
	}
	if !dropped {
		t.Fatalf("cleanup did not drop %s; items: %+v", delTable, clean.Items)
	}
	if tableExists(t, db, schema, delTable) {
		t.Fatalf("%s survived cleanup", delTable)
	}
	afterCleanup := schemaBytes(t, db, schema)
	if afterCleanup >= beforeCleanup {
		t.Fatalf("cleanup did not return any space: %s before, %s after", humanSize(beforeCleanup), humanSize(afterCleanup))
	}
	t.Logf("cleanup: dropped %s, %s -> %s in the schema", delTable, humanSize(beforeCleanup), humanSize(afterCleanup))
	if got := int64(countRows(t, db, "SELECT COUNT(*) FROM `t`")); got < rows {
		t.Fatalf("cleanup removed rows from the production table: %d", got)
	}
	if left := realArtifacts(db, *gh); len(left) > 0 {
		t.Fatalf("artifacts remain after cleanup: %v", left)
	}

	// --- the estimate now comes from the run --------------------------------
	//
	// The second plan is a different rebuild on the same table, because planning the
	// change that just landed answers "no execution is planned" - which is correct,
	// and would make this assertion pass for the wrong reason. It also has to come
	// after cleanup: a leftover _t_del is a D8 blocker, by design.
	second := scaleRequest(schema, gh)
	second.Alter = "MODIFY COLUMN `payload` varchar(2048) NOT NULL"
	after, err := runner.Analyze(context.Background(), second)
	if err != nil {
		t.Fatalf("analyze after the run: %v", err)
	}
	// One run is a measurement, not a range: the plan quotes it and keeps the
	// confidence honest. Two runs would produce the range, and that is asserted in
	// the analyzer's own tests; running the copy twice here would double the job for
	// a property already covered.
	if after.Plan.Duration.Observations < 1 {
		t.Fatalf("a finished run left no observation: %+v\nengine %q, blockers %d, reasons %v",
			after.Plan.Duration, after.Plan.Recommendation.Engine,
			after.Plan.Recommendation.Blockers, after.Plan.Recommendation.Reasons)
	}
	if !strings.Contains(after.Plan.Duration.Basis, "measured") {
		t.Fatalf("the estimate does not say it was measured: %q", after.Plan.Duration.Basis)
	}
	if after.Plan.Duration.Confidence == "high" {
		t.Fatalf("one run cannot be high confidence: %+v", after.Plan.Duration)
	}
	t.Logf("planned estimate after one run: %s - %s (%s): %s",
		analyzer.HumanDuration(after.Plan.Duration.FromSeconds),
		analyzer.HumanDuration(after.Plan.Duration.ToSeconds),
		after.Plan.Duration.Confidence, after.Plan.Duration.Basis)

	// --- the audit trail stays readable at this size -----------------------
	//
	// Progress events are rate-limited, and a copy that logged every chunk would
	// leave a file nobody can open.
	events := readEvents(t, store, sessionID)
	if len(events) == 0 {
		t.Fatal("the run wrote no events")
	}
	for i := 1; i < len(events); i++ {
		if events[i].Seq != events[i-1].Seq+1 {
			t.Fatalf("event log has a gap before %d: %d then %d", i, events[i-1].Seq, events[i].Seq)
		}
	}
	raw, err := store.ReadEvents(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("event log: %d events, %s", len(events), humanSize(int64(len(raw))))
	if len(raw) > 8<<20 {
		t.Fatalf("the event log is %s for one migration; progress verbosity has regressed", humanSize(int64(len(raw))))
	}

	if findings, err := runner.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	} else if len(findings) != 0 {
		t.Fatalf("a clean run left findings: %+v", findings)
	}
}

type applyResult struct {
	sess *state.Session
	err  error
}

// scaleRequest is a rebuild of a table that is too big for an instant change, with
// the swap held and the engine told to stay well under the load ceiling.
func scaleRequest(schema string, eng *copyEngine) core.Request {
	req := applyRequest(schema, "t", "MODIFY COLUMN `status` varchar(64) NOT NULL")
	req.EngineHint = eng.hint
	req.EngineHintSource = "recorded"
	req.ForExecution = true
	req.HoldCutover = true
	req.FreeDisk = 40 << 30
	req.DiskTotal = 200 << 30
	return req
}

// createScaleFixture builds a table whose on-disk size is roughly `gb`: a ~1 KB
// payload per row, doubled until the size is reached. It returns the row count it
// actually got, measured rather than assumed.
func createScaleFixture(t *testing.T, db *sql.DB, gb float64) int64 {
	t.Helper()
	if _, err := db.Exec("CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, " +
		"`status` varchar(32) NOT NULL DEFAULT 'pending', " +
		"`payload` varchar(1024) NOT NULL DEFAULT '', " +
		"PRIMARY KEY (`id`)) ENGINE=InnoDB"); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	if _, err := db.Exec("INSERT INTO `t` (`status`, `payload`) VALUES ('seed', REPEAT('x', 900))"); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}
	target := int64(gb * float64(int64(1)<<30))
	for {
		size := tableBytes(t, db, "", "t")
		if size >= target {
			break
		}
		if _, err := db.Exec("INSERT INTO `t` (`status`, `payload`) SELECT `status`, `payload` FROM `t`"); err != nil {
			t.Fatalf("grow fixture at %s: %v", humanSize(size), err)
		}
	}
	return int64(countRows(t, db, "SELECT COUNT(*) FROM `t`"))
}

// tableBytes is the server's own figure, which is the one the analyzer uses and the
// one a copy has to move.
func tableBytes(t *testing.T, db *sql.DB, schema, table string) int64 {
	t.Helper()
	var data, idx sql.NullInt64
	if err := db.QueryRow(`SELECT SUM(DATA_LENGTH), SUM(INDEX_LENGTH) FROM information_schema.TABLES
		WHERE TABLE_NAME = ? AND (? = '' OR TABLE_SCHEMA = ?)`, table, schema, schema).Scan(&data, &idx); err != nil {
		t.Fatalf("table size: %v", err)
	}
	return data.Int64 + idx.Int64
}

// waitForSession waits until the runner has recorded a session, which is how the
// test learns the id of a run it started in a goroutine.
func waitForSession(t *testing.T, store *state.Store, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sessions, err := store.List()
		if err == nil && len(sessions) == 1 {
			return sessions[0].ID
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no session appeared within %s", timeout)
	return ""
}

// waitForCopyComplete watches the ghost table, not the log: the row count converging
// on the production table is the same signal an operator watching `status` sees, and
// it does not depend on any engine's wording.
func waitForCopyComplete(t *testing.T, db *sql.DB, schema, table, sessionID string, timeout time.Duration) {
	t.Helper()
	ghost := "_" + table + "_gho"
	deadline := time.Now().Add(timeout)
	var last int64 = -1
	stableSince := time.Time{}
	for time.Now().Before(deadline) {
		var want, got sql.NullInt64
		if err := db.QueryRow("SELECT COUNT(*) FROM `" + table + "`").Scan(&want); err != nil {
			t.Fatalf("count production table: %v", err)
		}
		err := db.QueryRow("SELECT COUNT(*) FROM `" + ghost + "`").Scan(&got)
		if err != nil {
			// The ghost table appears when the copy starts; before that, keep waiting.
			time.Sleep(time.Second)
			continue
		}
		// The production table keeps growing, so "equal" is a moving target: the copy
		// is done when the ghost table has caught up and the gap between them stops
		// closing. A held cutover is what makes this observable at all.
		if got.Int64 >= want.Int64 {
			return
		}
		if got.Int64 == last {
			if stableSince.IsZero() {
				stableSince = time.Now()
			}
		} else {
			stableSince = time.Time{}
		}
		last = got.Int64
		if !stableSince.IsZero() && time.Since(stableSince) > 2*time.Minute {
			t.Fatalf("the copy stopped making progress: ghost %d, production %d (session %s)",
				got.Int64, want.Int64, sessionID)
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("the copy did not finish within %s", timeout)
}

type eventRecord struct {
	Seq  int64  `json:"seq"`
	Type string `json:"type"`
}

func readEvents(t *testing.T, store *state.Store, id string) []eventRecord {
	t.Helper()
	raw, err := store.ReadEvents(id)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var out []eventRecord
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev eventRecord
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("event log line is not JSON: %v", err)
		}
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// tableExists is asked of the server, not of our own record: the record is what we
// are testing.
func tableExists(t *testing.T, db *sql.DB, schema, table string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, schema, table).Scan(&n); err != nil {
		t.Fatalf("look up %s.%s: %v", schema, table, err)
	}
	return n > 0
}

// schemaBytes is the space the server says one database occupies, which is the
// figure an operator watches when a copy finishes and 40 GB of temporary table is
// still on the disk.
func schemaBytes(t *testing.T, db *sql.DB, schema string) int64 {
	t.Helper()
	var total sql.NullInt64
	if err := db.QueryRow(`SELECT SUM(DATA_LENGTH + INDEX_LENGTH) FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ?`, schema).Scan(&total); err != nil {
		t.Fatalf("schema size: %v", err)
	}
	return total.Int64
}

func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGT"[exp])
}
