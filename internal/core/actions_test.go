package core

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/state"
)

func newTestStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func saveSession(t *testing.T, store *state.Store, sess *state.Session) {
	t.Helper()
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
}

func TestCancelOnAFinishedSessionChangesNothing(t *testing.T) {
	store := newTestStore(t)
	saveSession(t, store, &state.Session{ID: "s1", State: state.StateDone, Database: "shop", Table: "orders"})

	res, err := (&Runner{Store: store}).Cancel(context.Background(), "s1", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != CancelAlreadyEnded {
		t.Fatalf("outcome = %q, want %q", res.Outcome, CancelAlreadyEnded)
	}
	if !strings.Contains(res.Note, "already ended") {
		t.Fatalf("note = %q", res.Note)
	}
}

func TestCancelWithoutAnEngineSaysSoInsteadOfPretending(t *testing.T) {
	store := newTestStore(t)
	saveSession(t, store, &state.Session{ID: "s2", State: state.StateRunning, Database: "shop", Table: "orders"})

	res, err := (&Runner{Store: store}).Cancel(context.Background(), "s2", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != CancelNoEngine {
		t.Fatalf("outcome = %q", res.Outcome)
	}
	if !strings.Contains(res.Note, "native DDL") {
		t.Fatalf("note = %q", res.Note)
	}

	// Nothing was decided for the operator: a native DDL belongs to the server.
	reloaded, err := store.Load("s2")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != state.StateRunning {
		t.Fatalf("state changed to %q, but cancel cannot stop a server-side DDL", reloaded.State)
	}
}

func TestCancelOnADeadPIDIsNotReportedAsStopped(t *testing.T) {
	store := newTestStore(t)
	saveSession(t, store, &state.Session{
		ID: "s3", State: state.StateRunning, Database: "shop", Table: "orders", EnginePID: 999_999,
	})

	res, err := (&Runner{Store: store}).Cancel(context.Background(), "s3", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != CancelNotRunning {
		t.Fatalf("outcome = %q, want not-running", res.Outcome)
	}
	// The honest answer is "run reconcile": the session is in exactly the state
	// that command exists for.
	if !strings.Contains(res.Note, "reconcile") {
		t.Fatalf("note = %q", res.Note)
	}
}

func TestCancelStopsTheProcessAndRecordsTheOutcome(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep is not available on this machine")
	}

	cmd := exec.Command(sleep, "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Reap the child so it does not linger as a zombie on platforms without a
	// portable zombie check.
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	defer func() { _ = cmd.Process.Kill(); <-done }()

	store := newTestStore(t)
	saveSession(t, store, &state.Session{
		ID: "s4", State: state.StateRunning, Phase: "ddl",
		Database: "shop", Table: "orders", Engine: "gh-ost",
		EnginePID: cmd.Process.Pid, Actor: "tester",
	})

	res, err := (&Runner{Store: store}).Cancel(context.Background(), "s4", false)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if res.Outcome != CancelStopped {
		t.Fatalf("outcome = %q, want stopped (%s)", res.Outcome, res.Note)
	}

	reloaded, err := store.Load("s4")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != state.StateCancelled {
		t.Fatalf("state = %q, want cancelled", reloaded.State)
	}
	if !strings.Contains(reloaded.Error, "cancelled by") {
		t.Fatalf("the audit trail must say who stopped it: %q", reloaded.Error)
	}
	if reloaded.LastSeq == 0 {
		t.Fatal("the cancellation must be appended to the event log")
	}
}

func TestCancelWithoutAStateDirectoryIsARefusal(t *testing.T) {
	if _, err := (&Runner{}).Cancel(context.Background(), "s1", false); err == nil {
		t.Fatal("expected an error without a store")
	}
}

func TestCleanupDryRunListsOnlyWhatTheSessionOwns(t *testing.T) {
	store := newTestStore(t)
	sess := &state.Session{ID: "s5", State: state.StateFailed, Database: "shop", Table: "orders", Engine: "pt-osc"}
	sess.RecordArtifact("table", "_orders_new", "pt-osc", "shadow table")
	sess.RecordArtifact("trigger", "pt_osc_shop_orders_ins", "pt-osc", "copy trigger")
	sess.MarkArtifactsSafe("leftover from a failed migration")
	saveSession(t, store, sess)

	// No database handle: a dry run answers from the session alone.
	res, err := (&Runner{Store: store}).Cleanup(context.Background(), "s5", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Refused != "" {
		t.Fatalf("unexpected refusal: %s", res.Refused)
	}
	if len(res.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(res.Items))
	}
	if !res.Items[0].Dropped {
		t.Fatal("a dry run reports what it would drop")
	}
}

func TestCleanupRefusesObjectsARecoveryStillNeeds(t *testing.T) {
	store := newTestStore(t)
	sess := &state.Session{ID: "s6", State: state.StateRunning, Database: "shop", Table: "orders", Engine: "gh-ost"}
	sess.RecordArtifact("table", "_orders_gho", "gh-ost", "ghost table")
	saveSession(t, store, sess)

	res, err := (&Runner{Store: store}).Cleanup(context.Background(), "s6", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Refused == "" {
		t.Fatal("cleanup must refuse while the session is live")
	}
	if !strings.Contains(res.Refused, "still running") {
		t.Fatalf("refusal = %q", res.Refused)
	}
	if len(res.Items) != 0 {
		t.Fatalf("nothing may be offered for dropping: %d item(s)", len(res.Items))
	}
}

func TestCleanupWithNoArtifactsSaysThereIsNothingToDo(t *testing.T) {
	store := newTestStore(t)
	saveSession(t, store, &state.Session{ID: "s7", State: state.StateDone, Database: "shop", Table: "orders", Engine: "native"})

	res, err := (&Runner{Store: store}).Cleanup(context.Background(), "s7", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Refused, "nothing to clean up") {
		t.Fatalf("refusal = %q", res.Refused)
	}
}

// An unknown artifact kind is a refusal, not a guess: dropping the wrong object
// cannot be undone.
func TestCleanupRefusesAnUnknownArtifactKind(t *testing.T) {
	if _, err := dropStatement("shop", state.ArtifactRecord{Kind: "view", Name: "v1"}); err == nil {
		t.Fatal("expected a refusal for an unknown kind")
	}
	stmt, err := dropStatement("shop", state.ArtifactRecord{Kind: "table", Name: "_orders_new"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stmt, "`shop`.`_orders_new`") {
		t.Fatalf("identifiers must be quoted: %s", stmt)
	}
}

// A session that finished is not the end of the story: an engine killed mid-copy
// is recorded as failed, which is terminal, while its shadow table is still on the
// primary. Reconcile has to say so, or the operator is told the incident is over
// while pt-osc triggers keep firing on every write.
func TestReconcileReportsAFinishedSessionThatLeftObjects(t *testing.T) {
	sess := &state.Session{
		ID: "s9", CreatedAt: "2026-01-01T00:00:00Z", State: state.StateFailed,
		Database: "shop", Table: "orders", Engine: "pt-osc",
	}
	sess.RecordArtifact("table", "_orders_new", "pt-osc", "shadow table")

	r := &Runner{}
	findings := r.reconcileSessions(context.Background(), []*state.Session{sess},
		func(_ *state.Session, a state.ArtifactRecord) bool { return a.Name == "_orders_new" })
	if len(findings) != 1 {
		t.Fatalf("a finished session with leftover objects must be reported, got %+v", findings)
	}
	if !findings[0].Actionable {
		t.Error("cleanup is the next step, so the finding must be actionable")
	}
	if !strings.Contains(findings[0].Verdict, "still here") {
		t.Errorf("verdict must say the objects remain: %q", findings[0].Verdict)
	}

	// And a finished session with nothing left stays quiet: that is the property
	// that makes a nightly reconcile usable.
	quiet := r.reconcileSessions(context.Background(), []*state.Session{sess},
		func(*state.Session, state.ArtifactRecord) bool { return false })
	if len(quiet) != 0 {
		t.Fatalf("a clean finish must not be reported, got %+v", quiet)
	}
}

// An out-of-band writer (a cancel from another terminal) must continue the
// sequence the log actually carries, not the one the session JSON remembered:
// the running apply has emitted events since its last save, and a collision
// would make a follower skip or duplicate an event.
func TestRecordEventContinuesFromTheLogNotTheSession(t *testing.T) {
	store := newTestStore(t)
	// The log carries five events; the session was last saved knowing about two.
	if err := store.AppendEvents("s1", []byte(
		`{"contract_version":1,"seq":1,"type":"phase.started"}`+"\n"+
			`{"contract_version":1,"seq":2,"type":"phase.progress"}`+"\n"+
			`{"contract_version":1,"seq":3,"type":"phase.progress"}`+"\n"+
			`{"contract_version":1,"seq":4,"type":"phase.progress"}`+"\n"+
			`{"contract_version":1,"seq":5,"type":"phase.progress"}`+"\n")); err != nil {
		t.Fatal(err)
	}
	sess := &state.Session{ID: "s1", State: state.StateRunning, Database: "shop", Table: "orders", LastSeq: 2}
	saveSession(t, store, sess)

	r := &Runner{Store: store}
	r.recordEvent(sess, "test.event", map[string]any{"note": "from another terminal"})

	raw, err := store.ReadEvents("s1")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `"seq":6`) {
		t.Fatalf("the new event must carry seq 6, log is:\n%s", text)
	}
	if strings.Count(text, `"seq":3`) != 1 {
		t.Fatalf("no existing sequence may be reused:\n%s", text)
	}
	if sess.LastSeq != 6 {
		t.Fatalf("the session must learn the new sequence: %d", sess.LastSeq)
	}
}
