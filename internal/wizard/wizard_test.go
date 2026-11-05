package wizard

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/core"
	"github.com/perogeremmer/saksama-core/internal/cred"
	"github.com/perogeremmer/saksama-core/internal/engine"
	"github.com/perogeremmer/saksama-core/internal/enginesetup"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// script is a Prompter that answers from a list. No pty, no timing, no flakes.
type script struct {
	out     *bytes.Buffer
	answers []string
	next    int
}

func newScript(answers ...string) *script {
	return &script{out: &bytes.Buffer{}, answers: answers}
}

func (s *script) take() (string, error) {
	if s.next >= len(s.answers) {
		return "", io.EOF
	}
	a := s.answers[s.next]
	s.next++
	return a, nil
}

func (s *script) Printf(format string, a ...any) { fmt.Fprintf(s.out, format, a...) }

func (s *script) Ask(label, def string) (string, error) {
	fmt.Fprintf(s.out, "? %s\n", label)
	v, err := s.take()
	if err != nil {
		return "", err
	}
	if v == "" {
		return def, nil
	}
	return v, nil
}

func (s *script) Secret(label string) (string, error) { return s.Ask(label, "") }

func (s *script) Choose(label string, items []Item, def string) (string, error) {
	return s.Ask(label, def)
}

func (s *script) Confirm(label string, def bool) (bool, error) {
	fmt.Fprintf(s.out, "? %s\n", label)
	v, err := s.take()
	if err != nil {
		return def, err
	}
	switch strings.ToLower(v) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

func (s *script) output() string { return s.out.String() }

// fakeBackend records what the wizard asked for and returns canned answers. It is
// also the proof that the wizard needs nothing from a database to be exercised.
type fakeBackend struct {
	connected  bool
	conn       cred.Conn
	analyzed   []core.Request
	prepared   []core.Request
	applied    []core.Request
	approvals  []state.Approval
	cleanups   []bool
	cancelled  []string
	prepareErr error
	applyErr   error
	schema     core.SchemaAdvice
	prep       *core.Prepared
	lastSess   *state.Session
	choices    []state.EngineChoice
	setEngines []state.EngineChoice
	cleared    []state.EngineChoice
}

func (f *fakeBackend) Connect(c cred.Conn) error {
	f.connected, f.conn = true, c
	return nil
}
func (f *fakeBackend) Close() error { return nil }

func (f *fakeBackend) Schema(ctx context.Context, database, table string) (core.SchemaAdvice, error) {
	return f.schema, nil
}

func (f *fakeBackend) Doctor(ctx context.Context) ([]enginesetup.Checklist, error) {
	return []enginesetup.Checklist{{Engine: enginesetup.GhOst, Target: "linux/amd64"}}, nil
}

func (f *fakeBackend) Analyze(ctx context.Context, req core.Request) (*core.Prepared, error) {
	f.analyzed = append(f.analyzed, req)
	if f.prepareErr != nil {
		return nil, f.prepareErr
	}
	return f.prep, nil
}

func (f *fakeBackend) Prepare(ctx context.Context, req core.Request) (*core.Prepared, error) {
	f.prepared = append(f.prepared, req)
	if f.prepareErr != nil {
		return nil, f.prepareErr
	}
	return f.prep, nil
}

func (f *fakeBackend) Apply(ctx context.Context, req core.Request) (*state.Session, error) {
	f.applied = append(f.applied, req)
	return f.lastSess, f.applyErr
}

func (f *fakeBackend) ApplyFromSession(ctx context.Context, id string, req core.Request) (*state.Session, error) {
	f.applied = append(f.applied, req)
	if req.Approve != nil {
		approval, err := req.Approve(core.ApprovalRequest{
			Action:          "apply",
			PlanFingerprint: "sha256:abc",
			Host:            "db.prod",
			RiskNote:        "index only",
			Statements:      []engine.Statement{{SQL: "ALTER TABLE `orders` ADD INDEX `ix` (`status`)", Algorithm: "INPLACE"}},
		})
		if err != nil {
			return f.lastSess, err
		}
		f.approvals = append(f.approvals, approval)
	}
	return f.lastSess, f.applyErr
}

func (f *fakeBackend) Cancel(ctx context.Context, id string, force bool) (core.CancelResult, error) {
	f.cancelled = append(f.cancelled, id)
	return core.CancelResult{ID: id, Outcome: core.CancelAlreadyEnded, Note: "already ended"}, nil
}

func (f *fakeBackend) Cleanup(ctx context.Context, id string, dryRun bool) (core.CleanupResult, error) {
	f.cleanups = append(f.cleanups, dryRun)
	return core.CleanupResult{ID: id, DryRun: dryRun, Refused: "nothing to clean up"}, nil
}

func (f *fakeBackend) ReleaseCutover(ctx context.Context, id string) (*state.Session, error) {
	return &state.Session{ID: id}, nil
}

func (f *fakeBackend) Reconcile(ctx context.Context) ([]core.Finding, error) { return nil, nil }

func (f *fakeBackend) Binaries() map[string]analyzer.EngineBinary { return nil }

func (f *fakeBackend) EngineChoices(ctx context.Context) ([]state.EngineChoice, error) {
	return f.choices, nil
}

func (f *fakeBackend) SetEngine(engine, database, table string, global bool, note string) (state.EngineChoice, error) {
	choice, err := state.NewChoice(engine, global, database, table)
	if err != nil {
		return state.EngineChoice{}, err
	}
	choice.Note = note
	f.setEngines = append(f.setEngines, choice)
	return choice, nil
}

func (f *fakeBackend) ClearEngine(database, table string, global bool) (state.EngineChoice, error) {
	choice, err := state.NewChoice("native", global, database, table)
	if err != nil {
		return state.EngineChoice{}, err
	}
	f.cleared = append(f.cleared, choice)
	return choice, nil
}

func newWizard(t *testing.T, b Backend, answers ...string) (*Wizard, *script) {
	t.Helper()
	store, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := newScript(answers...)
	return &Wizard{
		p:     p,
		b:     b,
		out:   p.out,
		store: store,
		actor: "tester",
		conn:  cred.Conn{Host: "db.prod", Port: "3306", User: "deployer"},
	}, p
}

// drive swaps in a new script and routes renderer output to the same buffer. A
// wizard writes through two paths (prompts and renderers), and a test that splits
// them reads half the story.
func drive(w *Wizard, answers ...string) *script {
	p := newScript(answers...)
	w.p = p
	w.out = p.out
	return p
}

func TestMenuExitsOnZero(t *testing.T) {
	w, p := newWizard(t, &fakeBackend{}, "0")
	if code := w.loop(); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(p.output(), "What do you want to do?") {
		t.Fatalf("the menu must be shown:\n%s", p.output())
	}
}

func TestScreensRequireAConnectionBeforeReadingAServer(t *testing.T) {
	w, p := newWizard(t, &fakeBackend{}, "3", "0")
	w.loop()
	if !strings.Contains(p.output(), "connect to a server first") {
		t.Fatalf("a screen must not invent a connection:\n%s", p.output())
	}
}

func TestConnectAsksOnceAndSaysWhatItRan(t *testing.T) {
	b := &fakeBackend{}
	w, p := newWizard(t, b,
		"1",           // connect
		"db.internal", // host
		"3307",        // port
		"deployer",    // user
		"shop",        // database
		"",            // password: empty, the user has none
		"0",           // exit
	)
	w.loop()

	if !b.connected {
		t.Fatal("the backend was never asked to connect")
	}
	if b.conn.Host != "db.internal" || b.conn.Port != "3307" || b.conn.Database != "shop" {
		t.Fatalf("unexpected connection: %+v", b.conn)
	}
	if !strings.Contains(p.output(), "equivalent: saksama advise --live -h db.internal -P 3307 -u deployer -d shop") {
		t.Fatalf("the equivalent command must be printed:\n%s", p.output())
	}
}

func TestPlanBuildsAnAlterFromChoicesThenRecordsOnlyWhenAsked(t *testing.T) {
	b := &fakeBackend{
		schema: core.SchemaAdvice{
			Database: "shop",
			Tables:   []analyzer.TableAssessment{{Table: "orders", Rows: 59_000_000, RebuildEngine: analyzer.EnginePtOSC}},
		},
		prep: &core.Prepared{
			Plan:    analyzer.Plan{Target: analyzer.Target{Database: "shop", Table: "orders"}},
			Session: &state.Session{ID: "s9"},
		},
	}
	w, p := newWizard(t, b,
		"1", "", "", "", "shop", "", // connect, keep defaults
		"4",         // plan
		"orders",    // table
		"1",         // ADD INDEX
		"ix_status", // index name
		"status",    // columns
		"n",         // not production
		"n",         // do not record
		"0",         // exit
	)
	w.loop()

	if len(b.analyzed) != 1 {
		t.Fatalf("analyze calls = %d, want 1", len(b.analyzed))
	}
	req := b.analyzed[0]
	if req.Alter != "ADD INDEX ix_status (status)" {
		t.Fatalf("alter = %q", req.Alter)
	}
	if req.Production {
		t.Fatal("production must be what the operator said, not a default")
	}
	if len(b.prepared) != 0 {
		t.Fatal("a preview must not record a session unless the operator asks")
	}
	if !strings.Contains(p.output(), "saksama plan --live -d shop -t orders -a 'ADD INDEX ix_status (status)'") {
		t.Fatalf("the equivalent command must be printed:\n%s", p.output())
	}
}

func TestPlanRecordsTheSessionWhenTheOperatorSaysYes(t *testing.T) {
	b := &fakeBackend{
		schema: core.SchemaAdvice{
			Database: "shop",
			Tables:   []analyzer.TableAssessment{{Table: "orders", RebuildEngine: analyzer.EngineNative}},
		},
		prep: &core.Prepared{
			Plan:    analyzer.Plan{Target: analyzer.Target{Database: "shop", Table: "orders"}},
			Session: &state.Session{ID: "s9"},
		},
	}
	w, p := newWizard(t, b,
		"1", "", "", "", "shop", "",
		"4", "orders", "7", "ADD COLUMN note text", "n", "y",
		"0",
	)
	w.loop()

	if len(b.prepared) != 1 {
		t.Fatalf("prepare calls = %d, want 1", len(b.prepared))
	}
	if !strings.Contains(p.output(), "SESSION    s9") {
		t.Fatalf("the recorded session must be named:\n%s", p.output())
	}
}

func TestRunningAPlanShowsTheStatementsBeforeApproving(t *testing.T) {
	b := &fakeBackend{lastSess: &state.Session{ID: "s1", State: state.StateDone}}
	w, p := newWizard(t, b, "0")
	w.connected = true

	// A session has to exist on disk for the runner to be offered.
	if err := w.store.Save(&state.Session{
		ID: "s1", CreatedAt: "2026-01-01T00:00:00Z", State: state.StatePlanned,
		Database: "shop", Table: "orders", Engine: "native", Alter: "ADD INDEX ix (status)",
	}); err != nil {
		t.Fatal(err)
	}

	// Drive only the apply screen; the loop would ask for the menu first.
	p = drive(w, "s1", "y")
	w.apply()

	if len(b.applied) != 1 {
		t.Fatalf("apply calls = %d, want 1", len(b.applied))
	}
	if !b.applied[0].ForExecution {
		t.Fatal("an execution must say it is one: a plan tolerates an unmeasured disk")
	}
	if len(b.approvals) != 1 || b.approvals[0].Decision != "granted" {
		t.Fatalf("approvals = %+v", b.approvals)
	}
	if b.approvals[0].PlanFingerprint != "sha256:abc" {
		t.Fatalf("the approval must be tied to the plan it approved: %+v", b.approvals[0])
	}
	out := p.output()
	if !strings.Contains(out, "STATEMENTS") || !strings.Contains(out, "ADD INDEX") {
		t.Fatalf("the statements must be shown before the confirmation:\n%s", out)
	}
	if !strings.Contains(out, "saksama apply --session s1") {
		t.Fatalf("the equivalent command must be printed:\n%s", out)
	}
}

func TestDecliningApprovalStopsWithoutExecuting(t *testing.T) {
	b := &fakeBackend{lastSess: &state.Session{ID: "s1"}, applyErr: errDeclined}
	w, p := newWizard(t, b, "0")
	w.connected = true
	if err := w.store.Save(&state.Session{
		ID: "s1", CreatedAt: "2026-01-01T00:00:00Z", State: state.StatePlanned,
		Database: "shop", Table: "orders", Alter: "ADD INDEX ix (status)",
	}); err != nil {
		t.Fatal(err)
	}

	p = drive(w, "s1", "n")
	w.apply()

	if len(b.approvals) != 0 {
		t.Fatalf("a declined run must not record an approval: %+v", b.approvals)
	}
	if !strings.Contains(p.output(), "stopped: declined") {
		t.Fatalf("declining must be reported:\n%s", p.output())
	}
}

func TestProductionAskTypesTheHostName(t *testing.T) {
	b := &fakeBackend{lastSess: &state.Session{ID: "s1", State: state.StateDone}}
	w, p := newWizard(t, b, "0")
	w.connected = true
	if err := w.store.Save(&state.Session{
		ID: "s1", CreatedAt: "2026-01-01T00:00:00Z", State: state.StatePlanned,
		Production: true, Database: "shop", Table: "orders", Alter: "ADD INDEX ix (status)",
	}); err != nil {
		t.Fatal(err)
	}

	// The wrong hostname aborts before anything is executed.
	p = drive(w, "s1", "db.staging")
	w.apply()

	if len(b.applied) != 0 {
		t.Fatal("a mismatched confirmation must stop before execution")
	}
	if !strings.Contains(p.output(), "not confirmed") {
		t.Fatalf("unexpected output:\n%s", p.output())
	}
}

func TestWizardRefusesToPromptWithoutATerminal(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(nil, t.TempDir(), &out, &errOut)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(errOut.String(), "needs an interactive terminal") {
		t.Fatalf("stderr = %s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "saksama plan --live") {
		t.Fatalf("the non-interactive equivalent must be named: %s", errOut.String())
	}
}

func TestEndOfInputExitsQuietly(t *testing.T) {
	w, _ := newWizard(t, &fakeBackend{}) // no answers at all: stdin closed
	if code := w.loop(); code != ExitOK {
		t.Fatalf("exit = %d, want 0 for ctrl-d", code)
	}
}

// A 5.6 production plan arrives blocked by E5b, and the only thing missing is the
// operator saying the risk is theirs. Without this screen the menu is a dead end:
// the CLI can pass that flag and the menu cannot.
func TestBlockedMDLIsOfferedForAcceptanceAndThenReanalysed(t *testing.T) {
	blocked := &core.Prepared{Plan: analyzer.Plan{
		Target: analyzer.Target{Database: "shop", Table: "orders"},
		Checks: []analyzer.Check{{
			ID: "E5b", Severity: analyzer.SeverityBlocker,
			Title: "the metadata lock queue cannot be observed on MySQL 5.6",
		}},
		Recommendation: analyzer.Recommendation{Blockers: 1},
	}}
	accepted := &core.Prepared{Plan: analyzer.Plan{
		Target:         analyzer.Target{Database: "shop", Table: "orders"},
		Recommendation: analyzer.Recommendation{},
	}}

	b := &countingBackend{fakeBackend: fakeBackend{}, plans: []*core.Prepared{blocked, accepted}}
	w, p := newWizard(t, b, "0")
	w.connected = true
	w.database = "shop"
	w.tables = []analyzer.TableAssessment{{Table: "orders"}}

	// table choice, ALTER builder (index on a column), production yes, accept the
	// risk yes, then record no.
	p = drive(w, "orders", "1", "ix_status", "status", "y", "y", "n")
	w.plan()

	if len(b.analyzed) != 2 {
		t.Fatalf("expected a second analysis after accepting the risk, got %d", len(b.analyzed))
	}
	if !b.analyzed[1].AcceptUnknownMDL {
		t.Fatalf("the second analysis must carry --accept-unknown-mdl: %+v", b.analyzed[1])
	}
	out := p.output()
	if !strings.Contains(out, "Accept that risk?") {
		t.Fatalf("the risk must be asked about:\n%s", out)
	}
	if !strings.Contains(out, "--accept-unknown-mdl") {
		t.Fatalf("the equivalent command must show the flag:\n%s", out)
	}
}

// A plan on a stock 5.6/5.7 server arrives with a copy engine it cannot install and
// a rebuild the server could do itself. The analyzer names the second door with R13,
// so the menu asks about it - otherwise the CLI could pass --allow-native-rebuild and
// the menu could not, which is the same dead end E5b used to be.
func TestOfferedNativeRebuildIsAcceptedAndThenReanalysed(t *testing.T) {
	blocked := &core.Prepared{Plan: analyzer.Plan{
		Target: analyzer.Target{Database: "shop", Table: "orders"},
		Checks: []analyzer.Check{{
			ID: "R13", Severity: analyzer.SeverityWarn,
			Title: "native could rebuild this table, but that needs a decision",
		}},
		Recommendation: analyzer.Recommendation{Blockers: 1},
	}}
	accepted := &core.Prepared{Plan: analyzer.Plan{
		Target:         analyzer.Target{Database: "shop", Table: "orders"},
		Checks:         []analyzer.Check{{ID: "R12", Severity: analyzer.SeverityWarn}},
		Recommendation: analyzer.Recommendation{},
	}}

	b := &countingBackend{fakeBackend: fakeBackend{}, plans: []*core.Prepared{blocked, accepted}}
	w, p := newWizard(t, b, "0")
	w.connected = true
	w.database = "shop"
	w.tables = []analyzer.TableAssessment{{Table: "orders"}}

	p = drive(w, "orders", "1", "ix_status", "status", "y", "y", "n")
	w.plan()

	if len(b.analyzed) != 2 {
		t.Fatalf("expected a second analysis after allowing the rebuild, got %d", len(b.analyzed))
	}
	if !b.analyzed[1].AllowNativeRebuild {
		t.Fatalf("the second analysis must carry --allow-native-rebuild: %+v", b.analyzed[1])
	}
	out := p.output()
	if !strings.Contains(out, "Allow it?") {
		t.Fatalf("the rebuild must be asked about:\n%s", out)
	}
	if !strings.Contains(out, "--allow-native-rebuild") {
		t.Fatalf("the equivalent command must show the flag:\n%s", out)
	}
}

// Declining keeps the plan as it was: the copy engine stays the answer.
func TestDecliningNativeRebuildLeavesThePlanAlone(t *testing.T) {
	blocked := &core.Prepared{Plan: analyzer.Plan{
		Target: analyzer.Target{Database: "shop", Table: "orders"},
		Checks: []analyzer.Check{{
			ID: "R13", Severity: analyzer.SeverityWarn,
			Title: "native could rebuild this table, but that needs a decision",
		}},
		Recommendation: analyzer.Recommendation{Blockers: 1},
	}}
	b := &countingBackend{fakeBackend: fakeBackend{}, plans: []*core.Prepared{blocked}}
	w, _ := newWizard(t, b, "0")
	w.connected = true
	w.database = "shop"
	w.tables = []analyzer.TableAssessment{{Table: "orders"}}

	drive(w, "orders", "1", "ix_status", "status", "y", "n")
	w.plan()

	for _, req := range b.analyzed {
		if req.AllowNativeRebuild {
			t.Fatalf("a declined rebuild must not reach the analyzer: %+v", req)
		}
	}
}

// Declining leaves the plan refused and records nothing.
func TestDecliningMDLAcceptanceKeepsThePlanRefused(t *testing.T) {
	blocked := &core.Prepared{Plan: analyzer.Plan{
		Target:         analyzer.Target{Database: "shop", Table: "orders"},
		Checks:         []analyzer.Check{{ID: "E5b", Severity: analyzer.SeverityBlocker}},
		Recommendation: analyzer.Recommendation{Blockers: 1},
	}}
	b := &countingBackend{fakeBackend: fakeBackend{}, plans: []*core.Prepared{blocked}}
	w, p := newWizard(t, b, "0")
	w.connected = true
	w.database = "shop"
	w.tables = []analyzer.TableAssessment{{Table: "orders"}}

	p = drive(w, "orders", "1", "ix_status", "status", "y", "n")
	w.plan()

	if len(b.prepared) != 0 {
		t.Fatal("a refused plan must not be recorded")
	}
	if len(b.analyzed) != 1 {
		t.Fatalf("declining must not trigger a re-analysis, got %d", len(b.analyzed))
	}
	if !strings.Contains(p.output(), "would be refused") {
		t.Fatalf("the refusal must be stated:\n%s", p.output())
	}
}

// The engine screen records a decision; it never installs anything.
func TestEngineChoiceRecordsWithoutInstalling(t *testing.T) {
	b := &fakeBackend{}
	w, p := newWizard(t, b, "0")

	// record a choice: table scope -> orders -> pt-osc -> note
	w.connected = true
	p = drive(w, "1", "1", "orders", "3", "foreign keys must be copied")
	w.database = "shop"
	w.tables = []analyzer.TableAssessment{{Table: "orders"}}
	w.engineChoice()

	if len(b.setEngines) != 1 {
		t.Fatalf("recorded %d choices", len(b.setEngines))
	}
	got := b.setEngines[0]
	if got.Engine != "pt-osc" || got.Database != "shop" || got.Table != "orders" {
		t.Fatalf("choice = %+v", got)
	}
	out := p.output()
	if !strings.Contains(out, "saksama engine set pt-osc -d shop -t orders") {
		t.Fatalf("the equivalent command must be printed:\n%s", out)
	}
	if strings.Contains(out, "install") && !strings.Contains(out, "nothing installed here") {
		t.Fatalf("the screen must not offer to install:\n%s", out)
	}
}

func TestEngineChoiceCanBeForgotten(t *testing.T) {
	b := &fakeBackend{choices: []state.EngineChoice{
		{Engine: "gh-ost", Scope: state.ScopeDatabase, Database: "shop"},
	}}
	w, p := newWizard(t, b, "0")

	// forget: database scope
	w.connected = true
	p = drive(w, "2", "2")
	w.database = "shop"
	w.engineChoice()

	if len(b.cleared) != 1 || b.cleared[0].Database != "shop" {
		t.Fatalf("cleared = %+v", b.cleared)
	}
	out := p.output()
	if !strings.Contains(out, "saksama engine clear -d shop") {
		t.Fatalf("the equivalent command must be printed:\n%s", out)
	}
	if !strings.Contains(out, "gh-ost") {
		t.Fatalf("existing choices must be listed:\n%s", out)
	}
}

// countingBackend returns a different canned plan per Analyze call, so a test can
// assert what happens between the first read and the second.
type countingBackend struct {
	fakeBackend
	plans []*core.Prepared
}

func (c *countingBackend) Analyze(ctx context.Context, req core.Request) (*core.Prepared, error) {
	c.analyzed = append(c.analyzed, req)
	if len(c.plans) == 0 {
		return nil, nil
	}
	p := c.plans[0]
	if len(c.plans) > 1 {
		c.plans = c.plans[1:]
	}
	return p, nil
}

func (c *countingBackend) EngineChoices(ctx context.Context) ([]state.EngineChoice, error) {
	return c.choices, nil
}
