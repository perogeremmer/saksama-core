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

	res, err := (&Runner{Store: store}).Cancel(context.Background(), "s1")
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

	res, err := (&Runner{Store: store}).Cancel(context.Background(), "s2")
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

	res, err := (&Runner{Store: store}).Cancel(context.Background(), "s3")
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

	res, err := (&Runner{Store: store}).Cancel(context.Background(), "s4")
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
	if _, err := (&Runner{}).Cancel(context.Background(), "s1"); err == nil {
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
