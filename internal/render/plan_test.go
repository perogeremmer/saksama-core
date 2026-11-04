package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/model"
)

func samplePlan() analyzer.Plan {
	return analyzer.Plan{
		ContractVersion:   1,
		Alter:             "ADD INDEX ix_created (created_at)",
		Target:            analyzer.Target{Host: "db.prod", Database: "shop", Table: "orders"},
		Server:            analyzer.ServerInfo{Version: "8.0.36", Vendor: "mysql"},
		SchemaFingerprint: "sha256:deadbeef",
		Schema:            analyzer.SchemaInfo{EstimatedRows: 59_000_000, DataBytes: 18 << 30, Columns: 8, Indexes: 3, OutgoingFK: 2},
		Operations: []analyzer.OpPlan{{
			Kind: model.OpAddIndex, Raw: "ADD INDEX ix_created (created_at)",
			RuleID: "R-ADD-INDEX-INPLACE", Method: analyzer.MethodInplace, Lock: "NONE",
			Reason: "in-place",
		}},
		Recommendation: analyzer.Recommendation{
			Engine:     analyzer.EngineNative,
			Executable: true,
			Reasons:    []string{"every operation is supported by a verified native DDL rule"},
			Fallbacks:  []analyzer.Engine{analyzer.EngineGhOst},
		},
		Checks: []analyzer.Check{
			{ID: "D1", Severity: analyzer.SeverityWarn, Title: "long-running query", Detail: "127s", Remediation: "wait", MySQLError: 1205},
			{ID: "A5", Severity: analyzer.SeverityInfo, Title: "key length fine"},
		},
		Duration: analyzer.DurationEstimate{FromSeconds: 209, ToSeconds: 1049, Confidence: "medium", Basis: "in-place"},
		Recovery: analyzer.Recovery{
			BeforeExecution: "nothing has been mutated",
			DuringExecution: "server owns the DDL",
			AfterSuccess:    "forward migration only",
		},
		Verification: []analyzer.Verification{{Kind: "index_exists", Name: "ix_created"}},
	}
}

func TestPlanCardContainsRequiredSections(t *testing.T) {
	var buf bytes.Buffer
	if err := PlanCard(&buf, samplePlan()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{
		"ALTER", "TARGET", "ENGINE", "DURATION", "WHY",
		"OPERATIONS", "FINDINGS", "RECOVERY", "VERIFY AFTER",
		"RECOMMEND  native", "BLOCKERS   0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing section %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "shop.orders @ db.prod") {
		t.Errorf("expected qualified target:\n%s", out)
	}
	if !strings.Contains(out, "RESULT     ready") {
		t.Errorf("expected ready result:\n%s", out)
	}
}

func TestPlanCardIsASCIIOnly(t *testing.T) {
	var buf bytes.Buffer
	if err := PlanCard(&buf, samplePlan()); err != nil {
		t.Fatal(err)
	}
	for _, r := range buf.String() {
		if r > 127 {
			t.Fatalf("non-ASCII rune %q found; output must stay plain for TERM=dumb", r)
		}
	}
}

func TestBlockedPlanCardSaysRefused(t *testing.T) {
	p := samplePlan()
	p.Recommendation.Engine = analyzer.EngineNone
	p.Recommendation.Blockers = 1
	p.Recovery = analyzer.Recovery{BeforeExecution: "no execution is planned"}

	var buf bytes.Buffer
	if err := PlanCard(&buf, p); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "RESULT     refused") {
		t.Fatalf("expected refused:\n%s", out)
	}
	if !strings.Contains(out, "none (blocked)") {
		t.Fatalf("expected blocked engine label:\n%s", out)
	}
}

func TestProductionTargetIsMarked(t *testing.T) {
	p := samplePlan()
	p.Target.Production = true

	var buf bytes.Buffer
	if err := PlanCard(&buf, p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "[production]") {
		t.Fatalf("expected production tag:\n%s", buf.String())
	}
}

func TestMultiLineInputIsFlattened(t *testing.T) {
	p := samplePlan()
	p.Alter = "ADD INDEX ix (\n  created_at\n)"

	var buf bytes.Buffer
	if err := PlanCard(&buf, p); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "ALTER      ADD INDEX ix (\n") {
		t.Fatal("multi-line ALTER leaked into the header")
	}
}

func TestEngineMatrixShowsEveryEngine(t *testing.T) {
	p := samplePlan()
	p.Engines = []analyzer.EngineAssessment{
		{Engine: analyzer.EngineNative, Verdict: analyzer.VerdictSuitable, Executable: true,
			Summary: "plain ALTER TABLE",
			Requirements: []analyzer.Requirement{
				{Name: "no table copy required", Met: true, Blocking: true, Detail: "in place"},
			}},
		{Engine: analyzer.EngineGhOst, Verdict: analyzer.VerdictBlocked,
			Summary: "ghost table copy", BlockedBy: []string{"no foreign keys in or out"},
			Requirements: []analyzer.Requirement{
				{Name: "no foreign keys in or out", Met: false, Blocking: true, Detail: "2 constraints"},
			}},
		{Engine: analyzer.EnginePtOSC, Verdict: analyzer.VerdictNotExecutable,
			Summary: "Percona Toolkit",
			Requirements: []analyzer.Requirement{
				{Name: "primary or unique key", Met: true, Blocking: true, Detail: "chunkable"},
			}},
	}
	p.Recommendation = analyzer.Recommendation{Engine: analyzer.EngineNative, Executable: true}

	var buf bytes.Buffer
	if err := EngineMatrix(&buf, p); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{
		"ENGINE SUITABILITY", "NATIVE", "GH-OST", "PT-OSC", "VERDICT",
		"SUITABLE", "BLOCKED by no foreign keys in or out", "NOT EXECUTABLE HERE",
		"recommended: native (executable now)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, r := range out {
		if r > 127 {
			t.Fatalf("non-ASCII rune %q in engine matrix", r)
		}
	}
}

func TestEngineMatrixExplainsNonExecutableRecommendation(t *testing.T) {
	p := samplePlan()
	p.Engines = []analyzer.EngineAssessment{
		{Engine: analyzer.EngineGhOst, Verdict: analyzer.VerdictNotExecutable, Summary: "copy"},
	}
	p.Recommendation = analyzer.Recommendation{
		Engine: analyzer.EngineGhOst, Executable: false,
		ExecutableReason: "the gh-ost adapter is not implemented in this build",
	}

	var buf bytes.Buffer
	if err := EngineMatrix(&buf, p); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "best fit for this change: gh-ost") {
		t.Fatalf("expected the best-fit line:\n%s", out)
	}
	if !strings.Contains(out, "not implemented in this build") {
		t.Fatalf("expected the reason:\n%s", out)
	}
}

func TestPlanCardSaysNotExecutableInsteadOfReady(t *testing.T) {
	p := samplePlan()
	p.Recommendation = analyzer.Recommendation{
		Engine: analyzer.EngineGhOst, Executable: false,
		ExecutableReason: "the gh-ost adapter is not implemented in this build",
	}

	var buf bytes.Buffer
	if err := PlanCard(&buf, p); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "RESULT     ready") {
		t.Fatalf("a non-executable plan must never read as ready:\n%s", out)
	}
	if !strings.Contains(out, "not executable in this build") {
		t.Fatalf("expected the not-executable result:\n%s", out)
	}
}
