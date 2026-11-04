package state

import (
	"os"
	"testing"
)

func TestNormalizeEngineAcceptsOperatorSpellings(t *testing.T) {
	cases := map[string]string{
		"gh-ost": "gh-ost", "ghost": "gh-ost", "GH": "gh-ost",
		"pt-osc": "pt-osc", "ptosc": "pt-osc", "percona": "pt-osc", "percona-toolkit": "pt-osc",
		"native": "native", "ddl": "native", "sql": "native",
	}
	for in, want := range cases {
		got, err := NormalizeEngine(in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	if _, err := NormalizeEngine("mysql-clone"); err == nil {
		t.Fatal("expected an error for an unknown engine")
	}
}

func TestSetAndResolveEngineChoice(t *testing.T) {
	s := newStore(t)

	if err := s.SetEngineChoice(EngineChoice{Engine: "gh-ost", Target: "shop.orders", Actor: "hudya"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	got, err := s.ResolveEngine("shop", "orders")
	if err != nil || got == nil {
		t.Fatalf("resolve: %v %v", got, err)
	}
	if got.Engine != "gh-ost" || got.Actor != "hudya" {
		t.Fatalf("got %+v", got)
	}
	// Normalisation must make this case-insensitive.
	if got, _ := s.ResolveEngine("SHOP", "Orders"); got == nil || got.Engine != "gh-ost" {
		t.Fatal("target lookup is not case-insensitive")
	}
}

func TestTableChoiceBeatsDefault(t *testing.T) {
	s := newStore(t)
	if err := s.SetEngineChoice(EngineChoice{Engine: "gh-ost"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEngineChoice(EngineChoice{Engine: "pt-osc", Target: "shop.orders"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ResolveEngine("shop", "orders")
	if err != nil || got == nil || got.Engine != "pt-osc" {
		t.Fatalf("table choice should win, got %+v (%v)", got, err)
	}

	// Another table falls back to the default.
	other, err := s.ResolveEngine("shop", "users")
	if err != nil || other == nil || other.Engine != "gh-ost" {
		t.Fatalf("expected the default, got %+v (%v)", other, err)
	}
}

func TestNoChoiceResolvesToNil(t *testing.T) {
	s := newStore(t)
	got, err := s.ResolveEngine("shop", "orders")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected no choice, got %+v", got)
	}
}

func TestClearEngineChoice(t *testing.T) {
	s := newStore(t)
	if err := s.SetEngineChoice(EngineChoice{Engine: "gh-ost", Target: "shop.orders"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearEngineChoice("shop.orders"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ := s.ResolveEngine("shop", "orders")
	if got != nil {
		t.Fatalf("choice survived clear: %+v", got)
	}
	// Clearing something that is not there is not an error.
	if err := s.ClearEngineChoice("shop.nothing"); err != nil {
		t.Fatalf("clear missing: %v", err)
	}
}

func TestEngineChoiceFileIsOwnerOnly(t *testing.T) {
	s := newStore(t)
	if err := s.SetEngineChoice(EngineChoice{Engine: "gh-ost"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.engineChoicePath(""))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %#o, want 0600", mode)
	}
}

func TestListEngineChoices(t *testing.T) {
	s := newStore(t)
	if err := s.SetEngineChoice(EngineChoice{Engine: "gh-ost"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEngineChoice(EngineChoice{Engine: "native", Target: "shop.orders"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListEngineChoices()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d choices", len(list))
	}
}

func TestSetEngineChoiceRejectsUnknownEngine(t *testing.T) {
	s := newStore(t)
	if err := s.SetEngineChoice(EngineChoice{Engine: "nope"}); err == nil {
		t.Fatal("expected an error")
	}
}
