package copyeng

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/engine"
	"github.com/perogeremmer/saksama-core/internal/model"
)

func testOptions() engine.Options {
	return engine.Options{
		Schema: "shop", Table: "orders",
		Alter: "ADD INDEX ix_paid (paid_at)",
		Ops: []model.Op{{
			Kind: model.OpAddIndex, Raw: "ADD INDEX ix_paid (paid_at)",
			IndexName: "ix_paid",
			Parts:     []model.IndexPart{{Column: "paid_at"}},
		}},
		Plan:        analyzer.Plan{},
		TableModel:  model.Table{Schema: "shop", Name: "orders"},
		HoldCutover: false,
	}
}

func conn() Conn {
	return Conn{Host: "db.prod", Port: "3306", User: "deployer"}
}

func argvOf(t *testing.T, e *Engine) []string {
	t.Helper()
	stmts, err := e.Statements()
	if err != nil {
		t.Fatalf("statements: %v", err)
	}
	if len(stmts) != 1 {
		t.Fatalf("got %d statements", len(stmts))
	}
	return stmts[0].Argv
}

func joined(argv []string) string { return strings.Join(argv, " ") }

func TestGhOstArgv(t *testing.T) {
	e := New(GhOst(), nil, conn(), testOptions(), nil)
	argv := argvOf(t, e)
	got := joined(argv)

	for _, want := range []string{
		"gh-ost",
		"--host=db.prod",
		"--database=shop",
		"--table=orders",
		"--alter=ADD INDEX ix_paid (paid_at)",
		"--assume-rbr",
		"--allow-on-master",
		"--execute",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in: %s", want, got)
		}
	}
	if strings.Contains(got, "--password=") {
		t.Error("no password should appear without an explicit opt-in")
	}
}

func TestGhOstRefusesArgvPasswordByDefault(t *testing.T) {
	c := conn()
	c.Password = "hunter2"
	e := New(GhOst(), nil, c, testOptions(), nil)

	_, err := e.Statements()
	if err == nil {
		t.Fatal("expected a refusal: gh-ost has no option file, so the password would be in argv")
	}
	if !strings.Contains(err.Error(), "--allow-argv-password") {
		t.Fatalf("the error must tell the operator how to opt in: %v", err)
	}
}

func TestGhOstAllowsArgvPasswordOnlyWhenAsked(t *testing.T) {
	opts := testOptions()
	opts.AllowArgvPassword = true
	c := conn()
	c.Password = "hunter2"

	argv := argvOf(t, New(GhOst(), nil, c, opts, nil))
	if !strings.Contains(joined(argv), "--password=hunter2") {
		t.Fatalf("expected the opt-in password in argv: %v", argv)
	}
}

func TestGhOstHoldCutoverAddsPostponeFlag(t *testing.T) {
	opts := testOptions()
	opts.HoldCutover = true
	argv := argvOf(t, New(GhOst(), nil, conn(), opts, nil))
	if !strings.Contains(joined(argv), "--postpone-cutover-file=") {
		t.Fatalf("expected a cut-over gate: %v", argv)
	}
}

func TestGhOstSkipsAllowOnMasterWhenReplicaExists(t *testing.T) {
	opts := testOptions()
	opts.HasReplica = true
	argv := argvOf(t, New(GhOst(), nil, conn(), opts, nil))
	if strings.Contains(joined(argv), "--allow-on-master") {
		t.Fatalf("a replica exists, so the copy should read from it: %v", argv)
	}
}

func TestPtOSCUsesDefaultsFileNotArgvPassword(t *testing.T) {
	c := conn()
	c.Password = "hunter2"
	e := New(PtOSC(), nil, c, testOptions(), nil)

	argv := argvOf(t, e)
	got := joined(argv)

	if strings.Contains(got, "hunter2") {
		t.Fatalf("pt-osc password must never be in argv: %s", got)
	}
	if !strings.Contains(got, "--defaults-file=") {
		t.Fatalf("expected an option file: %s", got)
	}
	// An execution must actually execute: --print would only describe the work.
	if !strings.Contains(got, "--execute") {
		t.Fatalf("expected --execute: %s", got)
	}
	if strings.Contains(got, "--print") {
		t.Fatalf("--print must not be sent when executing: %s", got)
	}
	if strings.Contains(got, "--max-flow-control-on") {
		t.Fatalf("--max-flow-control-on is not a pt-osc option: %s", got)
	}
	if !strings.Contains(got, "--no-version-check") {
		t.Fatalf("a CI run must not depend on percona.com: %s", got)
	}

	// The temporary file must be 0600 and contain the password.
	var path string
	for _, a := range argv {
		if strings.HasPrefix(a, "--defaults-file=") {
			path = strings.TrimPrefix(a, "--defaults-file=")
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("defaults file missing: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("defaults file mode = %#o, want 0600", mode)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "password=hunter2") {
		t.Fatalf("defaults file missing the password: %s", data)
	}
	os.Remove(path)
}

func TestPtOSCAddsForeignKeyMethodForIncomingConstraints(t *testing.T) {
	opts := testOptions()
	opts.TableModel.ReferencedBy = []model.ForeignKey{{
		Name: "fk_items_order", RefTable: "order_items",
		Columns: []string{"order_id"}, RefColumns: []string{"id"},
	}}
	argv := argvOf(t, New(PtOSC(), nil, conn(), opts, nil))
	if !strings.Contains(joined(argv), "--alter-foreign-keys-method=auto") {
		t.Fatalf("incoming FKs need the method flag: %v", argv)
	}
}

func TestPtOSCWithoutIncomingForeignKeys(t *testing.T) {
	argv := argvOf(t, New(PtOSC(), nil, conn(), testOptions(), nil))
	if strings.Contains(joined(argv), "--alter-foreign-keys-method") {
		t.Fatalf("no incoming FKs, so the flag is unnecessary: %v", argv)
	}
}

func TestPtOSCConnectionString(t *testing.T) {
	argv := argvOf(t, New(PtOSC(), nil, conn(), testOptions(), nil))
	if !strings.Contains(joined(argv), "D=shop,t=orders,h=db.prod,P=3306,u=deployer") {
		t.Fatalf("bad DSN: %v", argv)
	}
}

func TestEngineNames(t *testing.T) {
	if got := New(GhOst(), nil, conn(), testOptions(), nil).Name(); got != engine.GhOst {
		t.Errorf("gh-ost name = %q", got)
	}
	if got := New(PtOSC(), nil, conn(), testOptions(), nil).Name(); got != engine.PtOSC {
		t.Errorf("pt-osc name = %q", got)
	}
}

func TestDryRunExecutesNothing(t *testing.T) {
	opts := testOptions()
	opts.DryRun = true
	e := New(GhOst(), nil, conn(), opts, nil)
	ex, err := e.Execute(nil, nil)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(ex.Statements) != 1 {
		t.Fatalf("got %d statements", len(ex.Statements))
	}
}

// A fake binary on PATH proves the wrapper really invokes the engine with the
// argv it claims, without needing the real tool installed.
func TestExecuteRunsTheEngineWithTheBuiltArgv(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "argv.txt")
	bin := filepath.Join(dir, "gh-ost")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + capture + "\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	e := New(GhOst(), nil, conn(), testOptions(), nil)
	err := func() error {
		_, err := e.Execute(nil, nil)
		return err
	}()
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("the engine was not invoked: %v", err)
	}
	got := string(data)
	for _, want := range []string{"--database=shop", "--table=orders", "--execute"} {
		if !strings.Contains(got, want) {
			t.Errorf("engine did not receive %q; got:\n%s", want, got)
		}
	}
}
