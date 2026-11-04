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

func TestNewChoiceDerivesScope(t *testing.T) {
	cases := []struct {
		name     string
		global   bool
		database string
		table    string
		want     Scope
	}{
		{"explicit global", true, "", "", ScopeGlobal},
		{"bare engine means global", false, "", "", ScopeGlobal},
		{"database only", false, "shop", "", ScopeDatabase},
		{"table", false, "shop", "orders", ScopeTable},
	}
	for _, tc := range cases {
		c, err := NewChoice("gh-ost", tc.global, tc.database, tc.table)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if c.Scope != tc.want {
			t.Errorf("%s: scope = %q, want %q", tc.name, c.Scope, tc.want)
		}
	}
}

func TestNewChoiceRejectsConflictingScope(t *testing.T) {
	if _, err := NewChoice("gh-ost", true, "shop", ""); err == nil {
		t.Fatal("--global with -d must be rejected")
	}
	if _, err := NewChoice("gh-ost", false, "", "orders"); err == nil {
		t.Fatal("-t without -d must be rejected")
	}
	if _, err := NewChoice("nope", false, "", ""); err == nil {
		t.Fatal("unknown engine must be rejected")
	}
}

func TestChoiceLabel(t *testing.T) {
	cases := []struct {
		c    EngineChoice
		want string
	}{
		{EngineChoice{Scope: ScopeGlobal}, "global"},
		{EngineChoice{Scope: ScopeDatabase, Database: "shop"}, "shop.*"},
		{EngineChoice{Scope: ScopeTable, Database: "shop", Table: "orders"}, "shop.orders"},
	}
	for _, tc := range cases {
		if got := tc.c.Label(); got != tc.want {
			t.Errorf("Label() = %q, want %q", got, tc.want)
		}
	}
}

func TestSetAndResolveEngineChoice(t *testing.T) {
	s := newStore(t)

	choice, err := NewChoice("gh-ost", false, "shop", "orders")
	if err != nil {
		t.Fatal(err)
	}
	choice.Actor = "hudya"
	if err := s.SetEngineChoice(choice); err != nil {
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

// The whole point of --global: a table choice beats it, but nothing else does.
func TestResolutionOrder(t *testing.T) {
	s := newStore(t)

	globalChoice, _ := NewChoice("native", true, "", "")
	if err := s.SetEngineChoice(globalChoice); err != nil {
		t.Fatal(err)
	}
	dbChoice, _ := NewChoice("pt-osc", false, "shop", "")
	if err := s.SetEngineChoice(dbChoice); err != nil {
		t.Fatal(err)
	}
	tableChoice, _ := NewChoice("gh-ost", false, "shop", "orders")
	if err := s.SetEngineChoice(tableChoice); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		database, table string
		want            string
	}{
		{"shop", "orders", "gh-ost"},  // table wins
		{"shop", "users", "pt-osc"},   // database wins
		{"other", "orders", "native"}, // global wins
	}
	for _, tc := range cases {
		got, err := s.ResolveEngine(tc.database, tc.table)
		if err != nil {
			t.Fatalf("%s.%s: %v", tc.database, tc.table, err)
		}
		if got == nil {
			t.Fatalf("%s.%s: no choice resolved", tc.database, tc.table)
		}
		if got.Engine != tc.want {
			t.Errorf("%s.%s -> %q, want %q", tc.database, tc.table, got.Engine, tc.want)
		}
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
	choice, _ := NewChoice("gh-ost", false, "shop", "orders")
	if err := s.SetEngineChoice(choice); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearEngineChoice(choice); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ := s.ResolveEngine("shop", "orders")
	if got != nil {
		t.Fatalf("choice survived clear: %+v", got)
	}
	// Clearing something that is not there is not an error.
	missing, _ := NewChoice("native", false, "shop", "nothing")
	if err := s.ClearEngineChoice(missing); err != nil {
		t.Fatalf("clear missing: %v", err)
	}
}

func TestClearingGlobalLeavesTableChoice(t *testing.T) {
	s := newStore(t)
	g, _ := NewChoice("native", true, "", "")
	_ = s.SetEngineChoice(g)
	tc, _ := NewChoice("gh-ost", false, "shop", "orders")
	_ = s.SetEngineChoice(tc)

	if err := s.ClearEngineChoice(g); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ResolveEngine("shop", "orders"); got == nil || got.Engine != "gh-ost" {
		t.Fatal("table choice must survive clearing the global one")
	}
	if got, _ := s.ResolveEngine("shop", "users"); got != nil {
		t.Fatalf("global should be gone, got %+v", got)
	}
}

func TestEngineChoiceFileIsOwnerOnly(t *testing.T) {
	s := newStore(t)
	c, _ := NewChoice("gh-ost", true, "", "")
	if err := s.SetEngineChoice(c); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.engineChoicePath(c))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %#o, want 0600", mode)
	}
}

func TestListEngineChoicesOrdersBySpecificity(t *testing.T) {
	s := newStore(t)
	g, _ := NewChoice("native", true, "", "")
	db, _ := NewChoice("pt-osc", false, "shop", "")
	tb, _ := NewChoice("gh-ost", false, "shop", "orders")
	for _, c := range []EngineChoice{g, db, tb} {
		if err := s.SetEngineChoice(c); err != nil {
			t.Fatal(err)
		}
	}

	list, err := s.ListEngineChoices()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d choices", len(list))
	}
	if list[0].Scope != ScopeTable || list[1].Scope != ScopeDatabase || list[2].Scope != ScopeGlobal {
		t.Fatalf("unexpected order: %v %v %v", list[0].Scope, list[1].Scope, list[2].Scope)
	}
}

func TestSetEngineChoiceRejectsUnknownEngine(t *testing.T) {
	s := newStore(t)
	if err := s.SetEngineChoice(EngineChoice{Engine: "nope", Scope: ScopeGlobal}); err == nil {
		t.Fatal("expected an error")
	}
}
