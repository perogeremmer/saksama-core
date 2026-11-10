package core

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// observationSession writes what a finished migration leaves behind: a session
// whose plan records the size it faced, and an event log whose ddl phase brackets
// the work.
func observationSession(t *testing.T, store *state.Store, id string, engine analyzer.Engine, dataBytes, indexBytes int64, startedAt float64, seconds int) {
	t.Helper()
	plan, err := json.Marshal(map[string]any{
		"contract_version": 1,
		"schema": map[string]any{
			"data_bytes":  dataBytes,
			"index_bytes": indexBytes,
			"source":      "live",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&state.Session{
		ID: id, CreatedAt: "2026-11-09T10:00:00Z", UpdatedAt: "2026-11-09T10:05:00Z",
		State: state.StateDone, Database: "shop", Table: "orders",
		Engine: string(engine), Alter: "ADD INDEX (spot_id)", Plan: plan,
	}); err != nil {
		t.Fatal(err)
	}
	log := fmt.Sprintf(
		`{"contract_version":1,"seq":1,"ts":%f,"type":"phase.started","session":%q,"data":{"phase":"preflight"}}`+"\n"+
			`{"contract_version":1,"seq":2,"ts":%f,"type":"phase.started","session":%q,"data":{"phase":"ddl"}}`+"\n"+
			`{"contract_version":1,"seq":3,"ts":%f,"type":"phase.completed","session":%q,"data":{"phase":"ddl"}}`+"\n",
		startedAt, id, startedAt, id, startedAt+float64(seconds), id)
	if err := store.AppendEvents(id, []byte(log)); err != nil {
		t.Fatal(err)
	}
}

// The estimator's only data source is the state directory, so what it reads from
// one has to be exactly what the plan says happened.
func TestObservationsComeFromFinishedRuns(t *testing.T) {
	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 1,5 GB of data plus 0,5 GB of indexes, and 60 seconds of ddl phase.
	observationSession(t, store, "s1", analyzer.EnginePtOSC, 1500<<20, 500<<20, 1000, 60)

	got := Observations(store)
	if len(got) != 1 {
		t.Fatalf("observations = %d, want 1: %+v", len(got), got)
	}
	o := got[0]
	if o.Engine != analyzer.EnginePtOSC {
		t.Errorf("engine = %q", o.Engine)
	}
	if want := int64(2000 << 20); o.DataBytes != want {
		t.Errorf("data bytes = %d, want %d (data plus indexes: the copy moves both)", o.DataBytes, want)
	}
	if o.Seconds != 60 {
		t.Errorf("seconds = %d, want 60 (the ddl phase, not the whole session)", o.Seconds)
	}
	if o.At == "" {
		t.Error("an observation must say when it happened")
	}
}

// Everything that is not a finished measurement is skipped. Each case here is a
// real thing a state directory accumulates, and any of them feeding the estimator
// would make the tool confidently wrong.
func TestObservationsSkipWhatWasNotMeasured(t *testing.T) {
	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A completed run: the control, so a bug that skips everything is caught.
	observationSession(t, store, "ok", analyzer.EngineGhOst, 100<<20, 0, 1000, 10)

	// Cancelled: measured the copy up to the point it stopped.
	observationSession(t, store, "cancelled", analyzer.EngineGhOst, 100<<20, 0, 1000, 10)
	if err := store.Update("cancelled", func(s *state.Session) error {
		s.State = state.StateCancelled
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Failed.
	observationSession(t, store, "failed", analyzer.EngineGhOst, 100<<20, 0, 1000, 10)
	if err := store.Update("failed", func(s *state.Session) error {
		s.State = state.StateFailed
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// No native DDL ran, so there is no throughput to read.
	observationSession(t, store, "noengine", analyzer.EngineNone, 100<<20, 0, 1000, 10)

	// A session from a format that did not store the plan: no size, no observation.
	if err := store.Save(&state.Session{
		ID: "noplan", CreatedAt: "2026-11-08T10:00:00Z", State: state.StateDone,
		Database: "shop", Table: "orders", Engine: string(analyzer.EnginePtOSC),
	}); err != nil {
		t.Fatal(err)
	}

	// A log without the ddl bracket - a run that never reached the change, or one
	// from a format that did not emit phase events. Nothing measured, nothing
	// claimed.
	plan, err := json.Marshal(map[string]any{"schema": map[string]any{"data_bytes": 100 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&state.Session{
		ID: "nodll", CreatedAt: "2026-11-07T10:00:00Z", State: state.StateDone,
		Database: "shop", Table: "orders", Engine: string(analyzer.EnginePtOSC), Plan: plan,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvents("nodll", []byte(
		`{"contract_version":1,"seq":1,"ts":1000,"type":"phase.started","session":"nodll","data":{"phase":"preflight"}}`+"\n"+
			`{"contract_version":1,"seq":2,"ts":1010,"type":"phase.completed","session":"nodll","data":{"phase":"preflight"}}`+"\n")); err != nil {
		t.Fatal(err)
	}

	got := Observations(store)
	if len(got) != 1 || got[0].Seconds != 10 {
		t.Fatalf("observations = %+v, want only the completed gh-ost run", got)
	}
}

// A missing state directory is not a failure: it means there is no local history,
// which the plan must be able to say without an error in the way.
func TestObservationsWithoutAStore(t *testing.T) {
	if got := Observations(nil); got != nil {
		t.Fatalf("no store must yield no observations, got %+v", got)
	}
}

// A single truncated event line must not throw away the observations around it.
// The log is append-only and a killed process can leave half a line behind.
func TestObservationsSurviveATruncatedLog(t *testing.T) {
	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	observationSession(t, store, "s1", analyzer.EnginePtOSC, 100<<20, 0, 1000, 25)
	if err := store.AppendEvents("s1", []byte(`{"contract_version":1,"seq":9,"ts":12`)); err != nil {
		t.Fatal(err)
	}

	got := Observations(store)
	if len(got) != 1 || got[0].Seconds != 25 {
		t.Fatalf("a truncated trailing line broke the read: %+v", got)
	}
}
