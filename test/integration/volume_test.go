//go:build integration

package integration

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/engine"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// Everything up to here used tables with a handful of rows, which means the copy
// engines never chunked, never throttled, and never had to keep up with writes
// while they copied. This is the test for the claim that actually matters in
// production - no committed write is lost at cut-over - and it is the slowest and
// most environment-sensitive test in the repository, so it runs nightly rather
// than on every pull request.
//
// It is opt-in: SAKSAMA_VOLUME_TESTS=1.

// writerInterval is how often the concurrent writer commits: about 200 rows per
// second, which is a busy table and still leaves the copy room to finish.
const writerInterval = 5 * time.Millisecond

func TestCopyWithConcurrentWritesLosesNothing(t *testing.T) {
	if os.Getenv("SAKSAMA_VOLUME_TESTS") != "1" {
		t.Skip("set SAKSAMA_VOLUME_TESTS=1 to run the volume test (nightly, not per pull request)")
	}

	engines := copyEngines(t)
	eng := engines[0]
	for _, e := range engines {
		if e.hint == analyzer.EngineGhOst {
			eng = e
		}
	}

	schema, db := fixtureSchema(t)
	createVolumeFixture(t, db, 100000)
	// Measured, not assumed: the fixture doubles, so the real row count is the
	// power of two above the request, and an assumed number would make this test
	// fail for arithmetic reasons instead of losing data.
	initial := int64(countRows(t, db, "SELECT COUNT(*) FROM `t`"))

	runner, _ := newRunner(t)
	runner.DB = db

	// The writer is the point of the test: rows committed while the copy runs must
	// exist afterwards. It stops only when the migration has finished, so the last
	// write and the swap race on purpose.
	//
	// It commits with a pause between rows. A writer with no pause at all starves
	// gh-ost's cut-over: the swap needs an exclusive moment on the table, and a loop
	// that re-acquires the row lock immediately never gives it one - the copy then
	// sits in cut-over until the test times out, which is what the nightly did twice
	// while the same test passed in 50 seconds locally. Bounding the rate keeps the
	// claim intact (thousands of rows still commit during a copy) and stops the test
	// from measuring its own harness.
	stop := make(chan struct{})
	var committed int64
	var writerErr atomic.Value
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// A steady rate, not a busy loop. Each row is committed and then the writer
			// waits: the pause is what lets gh-ost's cut-over take an exclusive moment
			// on the table, and without it the copy sat in cut-over until the nightly
			// timed out - twice - while the same test passed in 50 seconds locally. The
			// rate is what a busy table looks like; a busy loop is what a benchmark
			// looks like, and it was measuring this harness rather than the copy.
			if _, err := db.Exec("INSERT INTO `t` (`status`, `origin`) VALUES ('copying', 'writer')"); err != nil {
				// A duplicate or a lock timeout at cut-over is not data loss; the write
				// simply did not commit, and committed is only incremented on success.
				time.Sleep(writerInterval)
				continue
			}
			atomic.AddInt64(&committed, 1)
			time.Sleep(writerInterval)
		}
	}()

	var mu sync.Mutex
	var samples []float64
	var peakThreads int
	var throttled bool

	req := applyRequest(schema, "t", "MODIFY COLUMN `status` varchar(64) NOT NULL")
	req.EngineHint = eng.hint
	req.EngineHintSource = "recorded"
	req.ForExecution = true
	req.FreeDisk = 500 << 30
	req.DiskTotal = 1000 << 30
	req.Progress = func(p engine.Progress) {
		mu.Lock()
		defer mu.Unlock()
		samples = append(samples, p.Percent)
		if p.ThreadsRunning > peakThreads {
			peakThreads = p.ThreadsRunning
		}
		if p.Throttled {
			throttled = true
		}
	}

	sess, err := runner.Apply(context.Background(), req)
	close(stop)
	wg.Wait()

	if v := writerErr.Load(); v != nil {
		t.Fatalf("writer: %v", v)
	}
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	written := atomic.LoadInt64(&committed)
	if written == 0 {
		t.Fatal("the writer committed nothing, so the test proved nothing")
	}
	// A floor, not just a non-zero check: the copy takes a couple of seconds now that
	// the writer no longer starves it, and a copy that finishes before the writer has
	// committed anything worth counting would leave this test passing without ever
	// overlapping the two. On CI it commits a few hundred; fifty is the line where the
	// overlap stops being incidental.
	if written < 50 {
		t.Fatalf("only %d rows committed during the copy; the copy was too fast for this test to mean anything", written)
	}

	// 1. No committed write was lost at cut-over.
	got := int64(countRows(t, db, "SELECT COUNT(*) FROM `t`"))
	if want := initial + written; got != want {
		t.Fatalf("row count after the migration: got %d, want %d (%d initial + %d written); %d rows were lost",
			got, want, initial, written, want-got)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `t` WHERE `origin` = 'writer'"); int64(n) != written {
		t.Fatalf("writer rows: got %d, want %d; committed writes went missing", n, written)
	}

	// 2. The change landed, and it landed as the plan said it would.
	if col := columnType(t, db, schema, "t", "status"); !strings.HasPrefix(col, "varchar(64)") {
		t.Fatalf("status is %q, want varchar(64)", col)
	}
	if sess.State != state.StateDone {
		t.Fatalf("session state %q, want done", sess.State)
	}

	// 3. It copied in chunks, not in one go: a single sample means the engine was
	// never actually doing chunked work, and this test would be lying.
	mu.Lock()
	defer mu.Unlock()
	if len(samples) < 2 {
		t.Fatalf("progress was sampled %d time(s); the copy did not run in chunks", len(samples))
	}
	if samples[len(samples)-1] < 99.0 {
		t.Errorf("the last progress sample was %.1f%%, so the copy never finished", samples[len(samples)-1])
	}

	// 4. The plan's throttle ceiling held. The wrapper emits
	// --critical-load Threads_running=100; a copy that pushed past it would have
	// been aborted by gh-ost, or would have hurt the application on pt-osc.
	if peakThreads > 100 {
		t.Errorf("Threads_running peaked at %d, above the critical load of 100 the wrapper emits", peakThreads)
	}
	t.Logf("%s copied %d rows plus %d concurrent writes; %d progress samples, peak Threads_running %d, throttled=%v",
		eng.name, initial, written, len(samples), peakThreads, throttled)
}

// createVolumeFixture builds a table large enough that the copy is real work.
// Doubling inserts keep it quick: each one copies what exists, so the total work
// is about twice the final size.
func createVolumeFixture(t *testing.T, db *sql.DB, rows int) {
	t.Helper()
	mustExec(t, db, "CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, "+
		"`status` varchar(32) NOT NULL DEFAULT 'pending', `origin` varchar(16) NOT NULL DEFAULT 'seed', "+
		"PRIMARY KEY (`id`)) ENGINE=InnoDB")
	mustExec(t, db, "INSERT INTO `t` (`status`) VALUES ('pending')")
	for inserted := 1; inserted < rows; inserted *= 2 {
		mustExec(t, db, "INSERT INTO `t` (`status`) SELECT `status` FROM `t`")
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM `t`"); n < rows {
		t.Fatalf("volume fixture has %d rows, want at least %d", n, rows)
	}
}
