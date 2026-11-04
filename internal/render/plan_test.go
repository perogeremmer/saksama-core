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
			Engine:    analyzer.EngineNative,
			Reasons:   []string{"every operation is supported by a verified native DDL rule"},
			Fallbacks: []analyzer.Engine{analyzer.EngineGhOst},
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
