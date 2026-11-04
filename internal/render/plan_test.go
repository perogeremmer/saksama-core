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
	p.TableFacts = []analyzer.Evidence{
		{Fact: "primary key is a single column: id bigint unsigned", Effect: "both copy engines can chunk by range"},
		{Fact: "2 outgoing foreign key(s)", Effect: "gh-ost has no foreign key support; pt-osc copies them"},
	}
	p.Engines = []analyzer.EngineAssessment{
		{
			Engine: analyzer.EngineNative, Verdict: analyzer.VerdictSuitable,
			Installed: true, Wrapped: true, Summary: "plain ALTER TABLE",
			Requirements: []analyzer.Requirement{{Name: "no table rebuild required", Met: true, Detail: "in place"}},
			Pros:         []string{"no copy, no shadow table"},
			Cons:         []string{"still needs the metadata lock"},
		},
		{
			Engine: analyzer.EngineGhOst, Verdict: analyzer.VerdictUnsuitable,
			Installed: true, Wrapped: true, Summary: "ghost table copy",
			Requirements: []analyzer.Requirement{{Name: "no foreign keys in or out", Met: false, Detail: "2 constraints"}},
			Pros:         []string{"MIT, single binary"},
			Cons:         []string{"cannot do foreign keys at all"},
		},
		{
			Engine: analyzer.EnginePtOSC, Verdict: analyzer.VerdictSuitable,
			Installed: false, Wrapped: true, Summary: "Percona Toolkit",
			Requirements: []analyzer.Requirement{{Name: "primary or unique key", Met: true, Detail: "chunkable"}},
			Pros:         []string{"handles composite keys"},
			Cons:         []string{"needs Perl"},
			Command:      "pt-online-schema-change --alter=\"ADD INDEX ix (a)\"",
			Steps:        []string{"review with --print first"},
		},
	}
	p.Recommendation = analyzer.Recommendation{
		Engine: analyzer.EnginePtOSC, Analyzer: analyzer.EnginePtOSC,
		EngineSource: "recommendation", Executable: false,
		ExecutableReason: "the engine binary is not installed on this machine",
	}

	var buf bytes.Buffer
	if err := EngineMatrix(&buf, p, ""); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{
		"ENGINE SUITABILITY", "WHAT THIS TABLE LOOKS LIKE",
		"NATIVE", "GH-OST", "PT-OSC", "VERDICT",
		"SUITABLE", "UNSUITABLE",
		"primary key is a single column",
		"pros:", "cons:", "+ no copy, no shadow table", "- cannot do foreign keys at all",
		"not installed",
		"command (what Saksama runs):",
		"analyzer recommends  pt-osc",
		"decide with          saksama engine set",
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

func TestEngineMatrixShowsRecordedChoiceMismatch(t *testing.T) {
	p := samplePlan()
	p.Recommendation = analyzer.Recommendation{
		Engine: analyzer.EngineGhOst, Analyzer: analyzer.EngineGhOst,
		EngineSource: "recorded", Executable: true,
	}

	var buf bytes.Buffer
	if err := EngineMatrix(&buf, p, "pt-osc"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "recorded choice      pt-osc (differs from the recommendation)") {
		t.Fatalf("expected the mismatch to be visible:\n%s", out)
	}
}

func TestPlanCardSaysNotExecutableInsteadOfReady(t *testing.T) {
	p := samplePlan()
	p.Recommendation = analyzer.Recommendation{
		Engine: analyzer.EngineGhOst, Analyzer: analyzer.EngineGhOst,
		Executable:       false,
		ExecutableReason: "the engine binary is not installed on this machine",
	}

	var buf bytes.Buffer
	if err := PlanCard(&buf, p); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "RESULT     ready") {
		t.Fatalf("a non-executable plan must never read as ready:\n%s", out)
	}
	if !strings.Contains(out, "cannot run here yet") {
		t.Fatalf("expected the not-executable result:\n%s", out)
	}
	if !strings.Contains(out, "not installed") {
		t.Fatalf("expected the reason:\n%s", out)
	}
}

func TestSchemaReportExplainsBlockedTables(t *testing.T) {
	rows := []analyzer.TableAssessment{
		{Table: "orders", Rows: 59_000_000, SizeBytes: 20 << 30, PrimaryKey: "id bigint",
			FKOut: 2, GhOst: analyzer.VerdictUnsuitable, PtOSC: analyzer.VerdictSuitable,
			RebuildEngine: analyzer.EnginePtOSC, RebuildReady: true,
			Notes: []string{"pt-osc only: gh-ost cannot handle this table"}},
		{Table: "sessions", Rows: 2_100_000, SizeBytes: 640 << 20, PrimaryKey: "uuid char(36)",
			GhOst: analyzer.VerdictSuitable, PtOSC: analyzer.VerdictSuitable,
			RebuildEngine: analyzer.EngineGhOst, RebuildReady: false,
			RebuildBlocked: "not installed", RebuildFix: "install: https://github.com/github/gh-ost/releases"},
		{Table: "audit_log", PrimaryKey: "(none)",
			GhOst: analyzer.VerdictUnsuitable, PtOSC: analyzer.VerdictUnsuitable,
			RebuildEngine: analyzer.EngineNone,
			Blockers:      []string{"no primary key or unique key to chunk by"}},
	}

	var buf bytes.Buffer
	srv := model.Server{Version: "8.0.46", Vendor: "mysql"}
	if err := SchemaReport(&buf, "shop", rows, nil, srv); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{
		"ENGINE SUITABILITY for schema shop",
		"TABLE", "PRIMARY KEY", "GH-OST", "PT-OSC", "REBUILD WITH",
		"pt-osc", "none",
		"TABLES NO ENGINE CAN COPY",
		"no primary key or unique key to chunk by",
		"NOTES",
		// The verdict, and the claim inside it read from the matrix.
		"VERDICT for schema shop",
		"no engine, no copy: ADD INDEX = INPLACE, LOCK=NONE on MySQL 8.0.46",
		// An engine that fits but is not installed must not read as usable.
		"gh-ost (unavailable)",
		"not installed",
		"install: https://github.com/github/gh-ost/releases",
		// Commands derived from this schema, not a hardcoded example.
		"run this next",
		"saksama advise --live -d shop -t orders -a \"<ALTER>\"",
		"saksama engine set pt-osc -d shop -t orders",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// The old NEXT block hardcoded gh-ost regardless of what fit the tables: on
	// this schema that command cannot work, and copying it would record an engine
	// the operator's table cannot use.
	if strings.Contains(out, "engine set gh-ost -d <db>") || strings.Contains(out, "engine set gh-ost -d shop -t orders") {
		t.Errorf("verdict suggests an engine that does not fit the table:\n%s", out)
	}

	for _, r := range out {
		if r > 127 {
			t.Fatalf("non-ASCII rune %q in schema report", r)
		}
	}
}

// The native claim is version gated, so an old or unverified server must not get
// one. This is the case that used to be prose in the renderer: "an index-only
// change needs no engine at all on MySQL 8.0" was printed for every server.
func TestSchemaReportDoesNotPromiseNativeOnAnOldServer(t *testing.T) {
	rows := []analyzer.TableAssessment{
		{Table: "orders", Rows: 10, SizeBytes: 1024, PrimaryKey: "id bigint",
			GhOst: analyzer.VerdictSuitable, PtOSC: analyzer.VerdictSuitable,
			RebuildEngine: analyzer.EnginePtOSC, RebuildReady: true},
	}

	servers := []model.Server{
		{Version: "5.4.4", Vendor: "mysql"},
		{Version: "5.5.62", Vendor: "mysql"},
		{Version: "10.11.6-MariaDB-log", Vendor: "mariadb"},
	}
	for _, srv := range servers {
		var buf bytes.Buffer
		if err := SchemaReport(&buf, "shop", rows, nil, srv); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		if !strings.Contains(out, "not native on this server") {
			t.Errorf("server %q: expected the refusal:\n%s", srv.Version, out)
		}
		if strings.Contains(out, "no engine, no copy") {
			t.Errorf("server %q: claimed native DDL without matrix support:\n%s", srv.Version, out)
		}
	}

	// No server at all is not a server that supports native DDL.
	var buf bytes.Buffer
	if err := SchemaReport(&buf, "shop", rows, nil, model.Server{}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "unknown: no server version") {
		t.Errorf("expected the unknown-version verdict:\n%s", out)
	}
	if strings.Contains(out, "no engine, no copy") || strings.Contains(out, "cannot parse server version") {
		t.Errorf("an absent version must be reported plainly, not parsed:\n%s", out)
	}
}

func TestSchemaReportHandlesEmptySchema(t *testing.T) {
	var buf bytes.Buffer
	if err := SchemaReport(&buf, "empty", nil, nil, model.Server{Version: "8.0.46"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no tables found") {
		t.Fatalf("unexpected output: %s", buf.String())
	}
}

func TestTableRendererAlignsAndBorders(t *testing.T) {
	tbl := &Table{
		Border: true,
		Columns: []Column{
			{Header: "NAME"},
			{Header: "ROWS", Align: Right},
		},
	}
	tbl.Add("orders", "59000000")
	tbl.Add("m2m", "7")

	var buf bytes.Buffer
	if err := tbl.Render(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if !strings.HasPrefix(out, "+") {
		t.Fatalf("expected a top border:\n%s", out)
	}
	// top rule + header + rule + 2 rows + bottom rule
	if strings.Count(out, "\n") != 6 {
		t.Fatalf("expected 6 lines, got %d:\n%s", strings.Count(out, "\n"), out)
	}
	// Numbers must be right-aligned so magnitudes line up.
	if !strings.Contains(out, "| 59000000 |") {
		t.Fatalf("rows column not right aligned:\n%s", out)
	}
	if !strings.Contains(out, "|        7 |") {
		t.Fatalf("small numbers are not padded:\n%s", out)
	}
}

func TestTableRendererPlainHasNoBorders(t *testing.T) {
	tbl := plainTable(Column{Header: "A"}, Column{Header: "B"})
	tbl.Add("1", "2")

	var buf bytes.Buffer
	if err := tbl.Render(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.ContainsAny(out, "+|") {
		t.Fatalf("plain output must not contain border characters:\n%s", out)
	}
	if !strings.Contains(out, "A") || !strings.Contains(out, "2") {
		t.Fatalf("content missing:\n%s", out)
	}
}

func TestTableRendererTruncatesWideCells(t *testing.T) {
	tbl := &Table{
		Border:   true,
		MaxWidth: 20,
		Columns:  []Column{{Header: "ALTER"}},
	}
	tbl.Add(strings.Repeat("x", 100))

	var buf bytes.Buffer
	if err := tbl.Render(&buf); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if len(line) > 40 {
			t.Fatalf("line not truncated to a sane width: %q", line)
		}
	}
	if !strings.Contains(buf.String(), "...") {
		t.Fatal("expected a truncation marker")
	}
}

func TestTableRendererIsASCIIOnly(t *testing.T) {
	tbl := boxTable(Column{Header: "A"})
	tbl.Add("value")

	var buf bytes.Buffer
	if err := tbl.Render(&buf); err != nil {
		t.Fatal(err)
	}
	for _, r := range buf.String() {
		if r > 127 {
			t.Fatalf("non-ASCII rune %q in table output", r)
		}
	}
}

func TestCompatibilityDocMentionsEveryRow(t *testing.T) {
	var buf bytes.Buffer
	if err := Compatibility(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{
		"## MySQL", "## Engines", "## Findings", "## PostgreSQL",
		"verified", "claimed", "not yet",
		"gh-ost", "pt-osc", "8.0.x", "8.4.x",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in the compatibility document", want)
		}
	}
	if !strings.Contains(out, "PostgreSQL has no online schema change") {
		t.Errorf("expected an honest PostgreSQL note")
	}
}
