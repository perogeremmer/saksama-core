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
	opts.CutoverGate = "/tmp/saksama-s3.cutover"

	argv := argvOf(t, New(GhOst(), nil, conn(), opts, nil))
	if !strings.Contains(joined(argv), "--postpone-cut-over-flag-file=/tmp/saksama-s3.cutover") {
		t.Fatalf("the engine must wait on the gate the core created: %v", argv)
	}

	// Without a gate path there is nothing to wait on, so the flag must not appear:
	// an engine waiting on a file that does not exist would hang forever.
	opts.CutoverGate = ""
	argv = argvOf(t, New(GhOst(), nil, conn(), opts, nil))
	if strings.Contains(joined(argv), "--postpone-cut-over-flag-file") {
		t.Fatalf("a hold without a gate path must not be passed to the engine: %v", argv)
	}
}

func TestPtOSCHasNoCutoverGate(t *testing.T) {
	// pt-osc swaps as soon as the copy finishes, so claiming otherwise would be a
	// promise the engine cannot keep.
	if gate := New(PtOSC(), nil, conn(), testOptions(), nil).CutoverGate(); gate != "" {
		t.Fatalf("pt-osc has no cutover gate, got %q", gate)
	}
	if gate := New(GhOst(), nil, conn(), testOptions(), nil).CutoverGate(); !strings.HasSuffix(gate, ".cutover") {
		t.Fatalf("gh-ost gate looks wrong: %q", gate)
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

// gh-ost refuses to start when performance_schema.metadata_locks is missing:
// "bailing out because metadata lock instrument not enabled". That table arrived in
// MySQL 5.7, so on 5.6 the flag is not optional - without it the engine exits
// before copying anything, and recommending gh-ost there would be advice nobody
// could follow.
func TestGhOstSkipsTheMetadataLockCheckOnlyWhereTheInstrumentIsMissing(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"5.6.51", true},
		{"5.6.51-log", true},
		{"5.7.44", false},
		{"8.0.46", false},
		{"8.4.13", false},
		{"", true}, // unparseable: assume absent, the plan explains why
	}

	for _, c := range cases {
		opts := testOptions()
		opts.Plan.Server.Version = c.version
		e := &Engine{spec: GhOst(), opts: opts}
		argv, err := e.argv()
		if err != nil {
			t.Fatalf("%s: %v", c.version, err)
		}
		got := strings.Contains(strings.Join(argv, " "), "--skip-metadata-lock-check")
		if got != c.want {
			t.Errorf("server %q: --skip-metadata-lock-check present=%v, want %v", c.version, got, c.want)
		}
	}
}

// A table that already has triggers must be sent to pt-osc with the flag that
// keeps them: without it the tool refuses to run, and the plan said it could.
func TestPtOSCAddsPreserveTriggersForTriggeredTables(t *testing.T) {
	opts := testOptions()
	opts.TableModel.Triggers = []string{"orders_audit"}

	e := New(PtOSC(), nil, conn(), opts, nil)
	if got := joined(argvOf(t, e)); !strings.Contains(got, "--preserve-triggers") {
		t.Fatalf("missing --preserve-triggers: %s", got)
	}

	// A triggerless table must not carry the flag: passing it where nothing
	// needs preserving is a promise about versions we would rather not make.
	e = New(PtOSC(), nil, conn(), testOptions(), nil)
	if got := joined(argvOf(t, e)); strings.Contains(got, "--preserve-triggers") {
		t.Fatalf("a triggerless table must not pass --preserve-triggers: %s", got)
	}
}

// The recorded command is written to disk and handed to export. The password
// has no business surviving the run that used it; the argv must stay exact,
// because Execute runs the engine from it.
func TestStatementRecordRedactsPasswordButArgvKeepsIt(t *testing.T) {
	c := conn()
	c.Password = "s3cret-hunter2"

	ghOpts := testOptions()
	ghOpts.AllowArgvPassword = true
	e := New(GhOst(), nil, c, ghOpts, nil)
	stmts, err := e.Statements()
	if err != nil {
		t.Fatalf("statements: %v", err)
	}
	if strings.Contains(stmts[0].SQL, c.Password) {
		t.Fatal("the recorded command carries the plaintext password")
	}
	if !strings.Contains(stmts[0].SQL, "[REDACTED]") {
		t.Fatalf("the redaction must be visible in the record: %s", stmts[0].SQL)
	}
	if !strings.Contains(joined(stmts[0].Argv), c.Password) {
		t.Fatal("the executable argv must stay exact")
	}

	// pt-osc keeps the password out of argv entirely, via the option file.
	e = New(PtOSC(), nil, c, testOptions(), nil)
	stmts, err = e.Statements()
	if err != nil {
		t.Fatalf("statements: %v", err)
	}
	if strings.Contains(joined(stmts[0].Argv), c.Password) {
		t.Fatal("pt-osc argv must not carry the plaintext password")
	}
	if strings.Contains(stmts[0].SQL, c.Password) {
		t.Fatal("the pt-osc record carries the plaintext password")
	}
	// And the staged file goes when Cleanup runs, even before Execute did.
	if e.defaultsFile == "" {
		t.Fatal("the option file should have been staged")
	}
	e.Cleanup()
	if _, err := os.Stat(e.defaultsFile); !os.IsNotExist(err) {
		t.Fatal("Cleanup must remove the staged option file")
	}
	// Cleanup twice is fine: every caller defers it.
	e.Cleanup()
}

// Statements() runs before approval and again inside Execute. The option file
// must be staged once, not once per call - the first file would otherwise be
// orphaned in /tmp with the password inside.
func TestDefaultsFileStagedOnceAcrossStatements(t *testing.T) {
	c := conn()
	c.Password = "s3cret-hunter2"
	e := New(PtOSC(), nil, c, testOptions(), nil)

	if _, err := e.Statements(); err != nil {
		t.Fatal(err)
	}
	first := e.defaultsFile
	if first == "" {
		t.Fatal("first Statements() must stage the option file")
	}
	if _, err := e.Statements(); err != nil {
		t.Fatal(err)
	}
	if e.defaultsFile != first {
		t.Fatalf("a second Statements() staged a new file %q and orphaned %q", e.defaultsFile, first)
	}
	e.Cleanup()
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatal("the staged file must be gone after Cleanup")
	}
}
