package stream

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeLog(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// A crash can leave a half-written line. Losing the sixty events before it would
// make the audit trail useless exactly when someone needs it.
func TestReadSkipsUnparseableLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.events.jsonl")
	writeLog(t, path,
		`{"contract_version":1,"seq":1,"type":"session.created"}`,
		`{"contract_version":1,"seq":2,"ty`,
		`{"contract_version":1,"seq":3,"type":"plan.computed"}`,
	)

	events, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2 (the partial line is skipped)", len(events))
	}
	if events[0].Seq != 1 || events[1].Seq != 3 {
		t.Fatalf("unexpected sequence: %d, %d", events[0].Seq, events[1].Seq)
	}
}

func TestReadMissingLogIsAnError(t *testing.T) {
	if _, err := Read(filepath.Join(t.TempDir(), "nope.jsonl")); err == nil {
		t.Fatal("a missing log must not read as an empty one")
	}
}

// afterSeq is what makes a reconnect cheap: the viewer says how far it got, and
// the tail starts after that.
func TestFollowStartsAfterTheGivenSeqAndTailsNewLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.events.jsonl")
	writeLog(t, path,
		`{"contract_version":1,"seq":1,"type":"session.created"}`,
		`{"contract_version":1,"seq":2,"type":"plan.computed"}`,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := Follow(ctx, path, 2)

	writeLog(t, path, `{"contract_version":1,"seq":3,"type":"phase.started"}`)

	select {
	case ev := <-s.Events:
		if ev.Seq != 3 {
			t.Fatalf("seq = %d, want 3; earlier events must not be replayed", ev.Seq)
		}
		if ev.Type != "phase.started" {
			t.Fatalf("type = %q", ev.Type)
		}
	case err := <-s.Err:
		t.Fatalf("unexpected error: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the appended event")
	}
}

func TestFollowReportsAMissingLog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := Follow(ctx, filepath.Join(t.TempDir(), "nope.jsonl"), 0)
	select {
	case err, ok := <-s.Err:
		if !ok || err == nil {
			t.Fatal("expected an error for a missing log")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

func TestFollowSkipsLinesAtOrBelowTheWatermark(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.events.jsonl")
	writeLog(t, path,
		`{"contract_version":1,"seq":1,"type":"a"}`,
		`{"contract_version":1,"seq":2,"type":"b"}`,
		`{"contract_version":1,"seq":3,"type":"c"}`,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := Follow(ctx, path, 1)

	select {
	case ev := <-s.Events:
		if ev.Seq != 2 {
			t.Fatalf("first event seq = %d, want 2", ev.Seq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}
