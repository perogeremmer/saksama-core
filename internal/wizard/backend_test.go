package wizard

import (
	"context"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// The live backend is the seam between the menu and the core. Every method is a
// one-line delegation, which is exactly why nobody wrote a test: the compiler
// cannot see a delegation that passes the arguments in the wrong order, and this
// package's tests all use the fake.
func TestLiveBackendSatisfiesTheInterface(t *testing.T) {
	// A compile-time assertion, in test form so it fails loudly if the seam drifts
	// while the fake keeps compiling.
	var backend Backend = (*live)(nil)
	if backend == nil {
		t.Fatal("the live backend must satisfy Backend")
	}
}

// The three methods below touch nothing but the state directory, so they are the
// part of the seam that can be exercised without a server.
func TestLiveBackendRecordsEngineChoices(t *testing.T) {
	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := newLive(store, map[string]analyzer.EngineBinary{"gh-ost": {Name: "gh-ost", Version: "1.1.11"}})

	if got := b.Binaries()["gh-ost"].Version; got != "1.1.11" {
		t.Fatalf("binaries = %v", b.Binaries())
	}

	choices, err := b.EngineChoices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(choices) != 0 {
		t.Fatalf("a fresh directory has no choices: %v", choices)
	}

	choice, err := b.SetEngine("gh-ost", "shop", "orders", false, "a copy keeps the table writable")
	if err != nil {
		t.Fatal(err)
	}
	if choice.Engine != "gh-ost" || choice.Database != "shop" || choice.Table != "orders" {
		t.Fatalf("choice = %+v", choice)
	}
	// The note is why the decision was made; it has to survive the write.
	if choice.Note != "a copy keeps the table writable" {
		t.Fatalf("note = %q", choice.Note)
	}
	// The menu records who decided, the same way the CLI does.
	if choice.Actor == "" {
		t.Fatal("a recorded choice must name its actor")
	}

	choices, err = b.EngineChoices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(choices) != 1 || choices[0].Engine != "gh-ost" {
		t.Fatalf("choices = %+v", choices)
	}

	if _, err := b.ClearEngine("shop", "orders", false); err != nil {
		t.Fatal(err)
	}
	choices, err = b.EngineChoices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(choices) != 0 {
		t.Fatalf("clearing a narrow scope must leave nothing behind: %+v", choices)
	}
}

// A closed or never-opened backend must not panic: the menu closes it on every
// exit path, including the ones where connecting failed.
func TestLiveBackendCloseIsSafeWithoutAConnection(t *testing.T) {
	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := newLive(store, nil)
	if err := b.Close(); err != nil {
		t.Fatalf("closing a backend that never connected: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("closing twice must stay quiet: %v", err)
	}
}

// Doctor answers from the local machine when no server is attached, and says so
// per checklist instead of pretending a server was asked.
func TestLiveBackendDoctorWithoutAServer(t *testing.T) {
	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := newLive(store, nil)
	lists, err := b.Doctor(context.Background())
	if err != nil {
		t.Fatalf("doctor must answer offline: %v", err)
	}
	if len(lists) != 2 {
		t.Fatalf("doctor covers gh-ost and pt-osc, got %d", len(lists))
	}
}
