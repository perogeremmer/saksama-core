package analyzer

import (
	"strings"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/model"
	"github.com/perogeremmer/saksama-core/internal/parser"
)

// --- fixtures ---------------------------------------------------------------

func baseTable() model.Table {
	return model.Table{
		Schema:    "shop",
		Name:      "orders",
		Engine:    "InnoDB",
		RowFormat: "DYNAMIC",
		Charset:   "utf8mb4",
		Collation: "utf8mb4_0900_ai_ci",
		// Realistic production shape: ~59M rows, ~18 GiB data, ~4 GiB indexes.
		Rows:        59_000_000,
		DataLength:  18 << 30,
		IndexLength: 4 << 30,
		Columns: []model.Column{
			{Name: "id", DataType: "bigint", Type: "bigint unsigned", Nullable: false, Extra: "auto_increment"},
			{Name: "spot_id", DataType: "bigint", Type: "bigint unsigned", Nullable: false},
			{Name: "status", DataType: "varchar", Type: "varchar(32)", Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci", Nullable: false},
			{Name: "note", DataType: "text", Type: "text", Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci", Nullable: true},
			{Name: "created_at", DataType: "datetime", Type: "datetime", Nullable: false},
		},
		Indexes: []model.Index{
			{Name: "PRIMARY", Primary: true, Unique: true, Parts: []model.IndexPart{{Column: "id"}}},
			{Name: "ix_spot", Parts: []model.IndexPart{{Column: "spot_id"}}},
		},
	}
}

func withFK(t model.Table) model.Table {
	t.ForeignKeys = append(t.ForeignKeys, model.ForeignKey{
		Name: "fk_orders_spot", Columns: []string{"spot_id"},
		RefTable: "spots", RefColumns: []string{"id"},
		OnDelete: model.ActionCascade, OnUpdate: model.ActionSetNull,
	})
	return t
}

func withIncomingFK(t model.Table) model.Table {
	t.ReferencedBy = append(t.ReferencedBy, model.ForeignKey{
		Name: "fk_items_order", RefTable: "order_items",
		Columns: []string{"order_id"}, RefColumns: []string{"id"},
		OnDelete: model.ActionCascade,
	})
	return t
}

func mustOps(t *testing.T, body string) []model.Op {
	t.Helper()
	a, err := parser.ParseAlter("ALTER TABLE `orders` " + body)
	if err != nil {
		t.Fatalf("parse %q: %v", body, err)
	}
	return a.Ops
}

func plan(t *testing.T, tbl model.Table, version, body string, mods ...func(*Input)) Plan {
	t.Helper()
	in := Input{
		Server:       model.Server{Version: version, Vendor: "mysql"},
		Table:        tbl,
		Ops:          mustOps(t, body),
		RawAlter:     body,
		Host:         "db.prod",
		SchemaSource: "live",
	}
	for _, m := range mods {
		m(&in)
	}
	return Analyze(in)
}

func hasCheck(p Plan, id string, sev Severity) bool {
	for _, c := range p.Checks {
		if c.ID == id && c.Severity == sev {
			return true
		}
	}
	return false
}

func hasRule(p Plan, rule string) bool {
	for _, op := range p.Operations {
		if op.RuleID == rule {
			return true
		}
	}
	return false
}

// --- routing ----------------------------------------------------------------

func TestAddIndexIsNativeInplace(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "ADD INDEX ix_created (created_at)")
	if p.Recommendation.Engine != EngineNative {
		t.Fatalf("got engine %q, want native", p.Recommendation.Engine)
	}
	if p.Recommendation.Blockers != 0 {
		t.Fatalf("unexpected blockers: %d", p.Recommendation.Blockers)
	}
	if !hasRule(p, "R-ADD-INDEX-INPLACE") {
		t.Fatalf("missing rule, got %+v", p.Operations)
	}
	if p.Operations[0].Method != MethodInplace || p.Operations[0].Lock != "NONE" {
		t.Fatalf("bad method: %+v", p.Operations[0])
	}
	if p.Duration.Confidence != "medium" {
		t.Fatalf("got confidence %q", p.Duration.Confidence)
	}
}

func TestAddIndexStillNativeWithForeignKeys(t *testing.T) {
	// Adding a secondary index does not touch FK columns, so the FK ban on copy
	// engines must not block it. This is the "native beats the guardrail" case.
	p := plan(t, withFK(baseTable()), "8.0.36", "ADD INDEX ix_created (created_at)")
	if p.Recommendation.Engine != EngineNative {
		t.Fatalf("got %q, want native", p.Recommendation.Engine)
	}
	if p.Recommendation.Blockers != 0 {
		t.Fatalf("unexpected blockers: %+v", p.Checks)
	}
}

func TestModifyColumnRoutesToGhOst(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL")
	if p.Recommendation.Engine != EngineGhOst {
		t.Fatalf("got %q, want gh-ost", p.Recommendation.Engine)
	}
	if !hasRule(p, "R-MODIFY-COLUMN-COPY") {
		t.Fatalf("missing rule: %+v", p.Operations)
	}
	if p.Recovery.HoldCutover != true {
		t.Fatal("gh-ost plans must expose hold-cutover")
	}
}

// A table with foreign keys still has a viable engine: pt-osc copies constraints
// natively. Routing must reach for it instead of refusing the change.
func TestModifyColumnWithForeignKeysSelectsPtOSC(t *testing.T) {
	p := plan(t, withFK(baseTable()), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL",
		withBinaries(availableBinaries()))

	if p.Recommendation.Engine != EnginePtOSC {
		t.Fatalf("got %q, want pt-osc", p.Recommendation.Engine)
	}
	if p.Recommendation.Blockers != 0 {
		t.Fatalf("expected no blockers, got %+v", p.Checks)
	}
	if !p.Recommendation.Executable {
		t.Fatalf("pt-osc should be runnable here, reason=%q", p.Recommendation.ExecutableReason)
	}
}

func TestIncomingForeignKeyWarnsAboutTheRewrittenChildRows(t *testing.T) {
	p := plan(t, withIncomingFK(baseTable()), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL",
		withBinaries(availableBinaries()))

	if p.Recommendation.Engine != EnginePtOSC {
		t.Fatalf("got %q, want pt-osc", p.Recommendation.Engine)
	}
	if !hasCheck(p, "R4", SeverityWarn) {
		t.Fatalf("expected the incoming-FK strategy warning: %+v", p.Checks)
	}
}

func TestRebuildWithoutPrimaryKeyIsBlocked(t *testing.T) {
	tbl := baseTable()
	tbl.Indexes = []model.Index{{Name: "ix_spot", Parts: []model.IndexPart{{Column: "spot_id"}}}}
	p := plan(t, tbl, "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL")
	if !hasCheck(p, chkNoEngineFits, SeverityBlocker) && !hasCheck(p, chkChosenEngineUnsuitable, SeverityBlocker) {
		t.Fatalf("expected a no-key blocker: %+v", p.Checks)
	}
	if p.Recommendation.Engine != EngineNone {
		t.Fatalf("got %q, want none", p.Recommendation.Engine)
	}
}

func TestCompositePrimaryKeyRoutesToPtOSC(t *testing.T) {
	tbl := baseTable()
	tbl.Indexes[0] = model.Index{Name: "PRIMARY", Primary: true, Unique: true,
		Parts: []model.IndexPart{{Column: "id"}, {Column: "spot_id"}}}
	p := plan(t, tbl, "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL",
		withBinaries(availableBinaries()))

	if p.Recommendation.Engine != EnginePtOSC {
		t.Fatalf("got %q, want pt-osc", p.Recommendation.Engine)
	}
	if !hasCheck(p, chkCompositePK, SeverityInfo) {
		t.Fatalf("expected composite-PK info: %+v", p.Checks)
	}
}

// --- version-gated capability ----------------------------------------------

func TestAddColumnVersionMatrix(t *testing.T) {
	cases := []struct {
		version  string
		body     string
		wantEng  Engine
		wantRule string
	}{
		{"8.0.11", "ADD COLUMN a INT NOT NULL DEFAULT 0", EngineGhOst, "R-ADD-COLUMN-COPY"},
		{"8.0.12", "ADD COLUMN a INT NOT NULL DEFAULT 0", EngineNative, "R-ADD-COLUMN-INSTANT-8012"},
		{"8.0.12", "ADD COLUMN a INT NOT NULL DEFAULT 0 AFTER status", EngineGhOst, "R-ADD-COLUMN-MIDDLE-COPY"},
		{"8.0.29", "ADD COLUMN a INT NOT NULL DEFAULT 0 AFTER status", EngineNative, "R-ADD-COLUMN-INSTANT-8029"},
		{"5.7.44", "ADD COLUMN a INT NOT NULL DEFAULT 0", EngineGhOst, "R-ADD-COLUMN-COPY"},
	}
	for _, tc := range cases {
		p := plan(t, baseTable(), tc.version, tc.body)
		if p.Recommendation.Engine != tc.wantEng {
			t.Errorf("%s %q: got engine %q, want %q", tc.version, tc.body, p.Recommendation.Engine, tc.wantEng)
		}
		if !hasRule(p, tc.wantRule) {
			t.Errorf("%s %q: missing rule %q, got %+v", tc.version, tc.body, tc.wantRule, p.Operations)
		}
	}
}

func TestDropColumnVersionMatrix(t *testing.T) {
	if p := plan(t, baseTable(), "8.0.28", "DROP COLUMN note"); p.Recommendation.Engine != EngineGhOst {
		t.Fatalf("8.0.28: got %q, want gh-ost", p.Recommendation.Engine)
	}
	if p := plan(t, baseTable(), "8.0.29", "DROP COLUMN note"); p.Recommendation.Engine != EngineNative {
		t.Fatalf("8.0.29: got %q, want native", p.Recommendation.Engine)
	}
}

func TestDropIndexIsNative(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "DROP INDEX ix_spot")
	if p.Recommendation.Engine != EngineNative {
		t.Fatalf("got %q", p.Recommendation.Engine)
	}
	if !hasRule(p, "R-DROP-INDEX-INPLACE") {
		t.Fatalf("missing rule: %+v", p.Operations)
	}
}

func TestRenameTableIsNative(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "RENAME TO orders_old")
	if p.Recommendation.Engine != EngineNative {
		t.Fatalf("got %q", p.Recommendation.Engine)
	}
}

// --- safety blockers --------------------------------------------------------

func TestUnknownOperationIsBlocked(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "FORCE")
	if !hasCheck(p, chkUnknownOp, SeverityBlocker) {
		t.Fatalf("expected unknown-op blocker: %+v", p.Checks)
	}
	if p.Recommendation.Engine != EngineNone {
		t.Fatalf("got %q, want none", p.Recommendation.Engine)
	}
}

func TestForeignKeyDDLIsBlocked(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "ADD CONSTRAINT fk_x FOREIGN KEY (spot_id) REFERENCES spots (id) ON DELETE CASCADE")
	if !hasCheck(p, "C8", SeverityBlocker) {
		t.Fatalf("expected FK-DDL blocker: %+v", p.Checks)
	}
}

func TestMariaDBIsBlocked(t *testing.T) {
	in := Input{
		Server:       model.Server{Version: "10.6.16-MariaDB", Vendor: "mariadb"},
		Table:        baseTable(),
		Ops:          mustOps(t, "ADD INDEX ix_created (created_at)"),
		RawAlter:     "ADD INDEX ix_created (created_at)",
		SchemaSource: "live",
	}
	p := Analyze(in)
	if !hasCheck(p, chkMariaDB, SeverityBlocker) {
		t.Fatalf("expected MariaDB blocker: %+v", p.Checks)
	}
}

func TestNonInnoDBIsBlocked(t *testing.T) {
	tbl := baseTable()
	tbl.Engine = "MyISAM"
	p := plan(t, tbl, "8.0.36", "ADD INDEX ix_created (created_at)")
	if !hasCheck(p, chkEngineNotInnoDB, SeverityBlocker) {
		t.Fatalf("expected engine blocker: %+v", p.Checks)
	}
}

func TestUnparseableVersionIsBlocked(t *testing.T) {
	p := plan(t, baseTable(), "unknown", "ADD INDEX ix_created (created_at)")
	if !hasCheck(p, chkVersionUnparseable, SeverityBlocker) {
		t.Fatalf("expected version blocker: %+v", p.Checks)
	}
}

func TestChangedFKColumnIsBlocked1832(t *testing.T) {
	p := plan(t, withFK(baseTable()), "8.0.36", "MODIFY COLUMN spot_id bigint unsigned NOT NULL")
	if !hasCheck(p, chkFKColumnTouched, SeverityBlocker) {
		t.Fatalf("expected C1 blocker: %+v", p.Checks)
	}
	for _, c := range p.Checks {
		if c.ID == chkFKColumnTouched && c.MySQLError != 1832 {
			t.Fatalf("expected mysql error 1832, got %d", c.MySQLError)
		}
	}
}

func TestDroppedColumnReferencedByIncomingFKIsBlocked(t *testing.T) {
	p := plan(t, withIncomingFK(baseTable()), "8.0.36", "DROP COLUMN id")
	if !hasCheck(p, chkFKReferencedTouched, SeverityBlocker) {
		t.Fatalf("expected C2 blocker: %+v", p.Checks)
	}
}

func TestCharsetConversionWithForeignKeyIsBlocked(t *testing.T) {
	p := plan(t, withFK(baseTable()), "8.0.36", "CONVERT TO CHARACTER SET latin1")
	if !hasCheck(p, chkConvertCharsetFK, SeverityBlocker) {
		t.Fatalf("expected A2 blocker: %+v", p.Checks)
	}
}

// --- index checks -----------------------------------------------------------

func TestTextIndexWithoutPrefixIsBlocked(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "ADD INDEX ix_note (note)")
	if !hasCheck(p, chkBlobIndexNoPrefix, SeverityBlocker) {
		t.Fatalf("expected A7 blocker: %+v", p.Checks)
	}
	if p.Recommendation.Engine != EngineNone {
		t.Fatalf("got %q, want none", p.Recommendation.Engine)
	}
}

func TestTextIndexWithPrefixIsAccepted(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "ADD INDEX ix_note (note(191))")
	if hasCheck(p, chkBlobIndexNoPrefix, SeverityBlocker) {
		t.Fatalf("prefix index should not trip A7: %+v", p.Checks)
	}
	if !hasCheck(p, chkPrefixIndex, SeverityInfo) {
		t.Fatalf("expected B6 prefix info: %+v", p.Checks)
	}
	if p.Recommendation.Engine != EngineNative {
		t.Fatalf("got %q", p.Recommendation.Engine)
	}
}

func TestKeyTooLongIsBlocked(t *testing.T) {
	tbl := baseTable()
	// utf8mb4 varchar(1000) = 4000 bytes > 3072
	tbl.Columns = append(tbl.Columns, model.Column{
		Name: "wide", DataType: "varchar", Type: "varchar(1000)",
		Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci",
	})
	p := plan(t, tbl, "8.0.36", "ADD INDEX ix_wide (wide)")
	if !hasCheck(p, chkKeyTooLong, SeverityBlocker) {
		t.Fatalf("expected A5 blocker: %+v", p.Checks)
	}
}

func TestKeyFitsInUtf8mb4IsAccepted(t *testing.T) {
	tbl := baseTable()
	tbl.Columns = append(tbl.Columns, model.Column{
		Name: "code", DataType: "varchar", Type: "varchar(100)",
		Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci",
	})
	p := plan(t, tbl, "8.0.36", "ADD INDEX ix_code (code)")
	if hasCheck(p, chkKeyTooLong, SeverityBlocker) {
		t.Fatalf("400 byte key should fit: %+v", p.Checks)
	}
}

func TestUtf8mb3Warning(t *testing.T) {
	tbl := baseTable()
	tbl.Columns = append(tbl.Columns, model.Column{
		Name: "legacy", DataType: "varchar", Type: "varchar(10)", Charset: "utf8mb3",
	})
	p := plan(t, tbl, "8.0.36", "ADD INDEX ix_created (created_at)")
	if !hasCheck(p, chkUtf8mb3, SeverityWarn) {
		t.Fatalf("expected A4 warning: %+v", p.Checks)
	}
}

func TestUniqueIndexOfflineNotesDuplicateScan(t *testing.T) {
	in := Input{
		Server:       model.Server{Version: "8.0.36", Vendor: "mysql"},
		Table:        baseTable(),
		Ops:          mustOps(t, "ADD UNIQUE INDEX uk_status (status)"),
		RawAlter:     "ADD UNIQUE INDEX uk_status (status)",
		SchemaSource: "ddl",
	}
	p := Analyze(in)
	if !hasCheck(p, chkUniqueNeedsScan, SeverityInfo) {
		t.Fatalf("expected B5 info: %+v", p.Checks)
	}
}

// --- operational ------------------------------------------------------------

func TestInsufficientDiskIsBlocked(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL", func(in *Input) {
		in.Table.DataLength = 100 << 30
		in.FreeDisk = 10 << 30
	})
	if !hasCheck(p, chkDiskHeadroom, SeverityBlocker) {
		t.Fatalf("expected D6 blocker: %+v", p.Checks)
	}
}

func TestProductionWarning(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "ADD INDEX ix_created (created_at)", func(in *Input) {
		in.Production = true
	})
	if !hasCheck(p, chkProductionTarget, SeverityWarn) {
		t.Fatalf("expected D12 warning: %+v", p.Checks)
	}
}

func TestManagedServerMentionedInReasons(t *testing.T) {
	in := Input{
		Server:       model.Server{Version: "8.0.36", Vendor: "aurora", VersionCmt: "Aurora MySQL"},
		Table:        baseTable(),
		Ops:          mustOps(t, "MODIFY COLUMN status varchar(64) NOT NULL"),
		RawAlter:     "MODIFY COLUMN status varchar(64) NOT NULL",
		SchemaSource: "live",
	}
	p := Analyze(in)
	if !p.Server.Managed {
		t.Fatal("expected managed server detection")
	}
	found := false
	for _, c := range p.Checks {
		if c.ID == "R5" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the managed-server warning, got %+v", p.Checks)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// --- determinism ------------------------------------------------------------

func TestFingerprintIgnoresVolatileFields(t *testing.T) {
	a := baseTable()
	b := baseTable()
	b.Rows = 59_000_000
	b.DataLength = 18 << 30
	if Fingerprint(a) != Fingerprint(b) {
		t.Fatal("fingerprint must ignore row counts and sizes")
	}
}

func TestFingerprintTracksSchemaChanges(t *testing.T) {
	a := baseTable()
	b := baseTable()
	b.Columns = append(b.Columns, model.Column{Name: "extra", DataType: "int"})
	if Fingerprint(a) == Fingerprint(b) {
		t.Fatal("fingerprint must change when columns change")
	}

	c := withFK(baseTable())
	if Fingerprint(a) == Fingerprint(c) {
		t.Fatal("fingerprint must change when constraints change")
	}
}

func TestAnalyzeIsDeterministic(t *testing.T) {
	tbl := baseTable()
	first := plan(t, tbl, "8.0.36", "ADD INDEX ix_created (created_at)")
	second := plan(t, tbl, "8.0.36", "ADD INDEX ix_created (created_at)")
	if first.SchemaFingerprint != second.SchemaFingerprint {
		t.Fatal("fingerprint not deterministic")
	}
	if len(first.Checks) != len(second.Checks) {
		t.Fatal("check list not deterministic")
	}
	for i := range first.Checks {
		if first.Checks[i].ID != second.Checks[i].ID {
			t.Fatalf("check order not deterministic at %d", i)
		}
	}
}

func TestBlockersSortBeforeWarnings(t *testing.T) {
	p := plan(t, withFK(baseTable()), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL", func(in *Input) {
		in.Production = true
	})
	prev := 99
	for _, c := range p.Checks {
		rank := severityRank(c.Severity)
		if rank > prev {
			t.Fatalf("check %s (%s) sorts after a lower severity: %+v", c.ID, c.Severity, p.Checks)
		}
		prev = rank
	}
}

// --- fail-closed guarantees -------------------------------------------------

func TestEmptyOperationListIsNeverReady(t *testing.T) {
	// A plan with no operations must not be reported as safe: that would be the
	// worst possible failure mode for a CI gate.
	in := Input{
		Server:       model.Server{Version: "8.0.36", Vendor: "mysql"},
		Table:        baseTable(),
		Ops:          nil,
		RawAlter:     "",
		SchemaSource: "ddl",
	}
	p := Analyze(in)
	if p.Recommendation.Blockers == 0 {
		t.Fatalf("empty operation list produced no blocker: %+v", p)
	}
	if p.Recommendation.Engine != EngineNone {
		t.Fatalf("got engine %q, want none", p.Recommendation.Engine)
	}
	if !hasCheck(p, chkUnknownOp, SeverityBlocker) {
		t.Fatalf("expected unknown-op blocker: %+v", p.Checks)
	}
}

func TestUUIDPrimaryKeyWarns(t *testing.T) {
	tbl := model.Table{
		Schema: "shop", Name: "sessions", Engine: "InnoDB", RowFormat: "DYNAMIC",
		Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci",
		Columns: []model.Column{
			{Name: "uuid", DataType: "char", Type: "char(36)", Charset: "utf8mb4"},
			{Name: "created_at", DataType: "datetime", Type: "datetime"},
		},
		Indexes: []model.Index{{Name: "PRIMARY", Primary: true, Unique: true,
			Parts: []model.IndexPart{{Column: "uuid"}}}},
	}
	p := plan(t, tbl, "8.0.36", "MODIFY COLUMN created_at datetime(6) NOT NULL")
	if !hasCheck(p, chkUUIDPrimaryKey, SeverityWarn) {
		t.Fatalf("expected B4 warning: %+v", p.Checks)
	}
	// A UUID primary key is a warning, not a blocker: gh-ost can chunk it,
	// just less evenly.
	if p.Recommendation.Engine != EngineGhOst {
		t.Fatalf("got %q, want gh-ost", p.Recommendation.Engine)
	}
}

// --- engine matrix ("is this a gh-ost table or a pt-osc table?") -------------

func assessmentFor(p Plan, e Engine) (EngineAssessment, bool) {
	for _, a := range p.Engines {
		if a.Engine == e {
			return a, true
		}
	}
	return EngineAssessment{}, false
}

// unmet returns the requirement names that failed for an engine.
func unmetList(a EngineAssessment) []string { return unmet(a) }

func unmetContains(a EngineAssessment, name string) bool {
	for _, n := range unmet(a) {
		if n == name {
			return true
		}
	}
	return false
}

func unmet(a EngineAssessment) []string {
	var out []string
	for _, r := range a.Requirements {
		if !r.Met {
			out = append(out, r.Name)
		}
	}
	return out
}

func withBinaries(b map[string]EngineBinary) func(*Input) {
	return func(in *Input) { in.Binaries = b }
}

func availableBinaries() map[string]EngineBinary {
	return map[string]EngineBinary{
		"gh-ost":                  {Name: "gh-ost", Available: true, Path: "/usr/local/bin/gh-ost", Version: "1.1.7"},
		"pt-online-schema-change": {Name: "pt-online-schema-change", Available: true, Path: "/usr/bin/pt-online-schema-change", Version: "3.5.1"},
	}
}

func TestAdviseNativeSuitableForAddIndex(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "ADD INDEX ix_created (created_at)")

	native, ok := assessmentFor(p, EngineNative)
	if !ok {
		t.Fatal("native assessment missing")
	}
	if native.Verdict != VerdictSuitable {
		t.Fatalf("native verdict = %q (%v)", native.Verdict, unmet(native))
	}
	if !native.Runnable() {
		t.Fatal("native must be runnable")
	}
	if !p.Recommendation.Executable {
		t.Fatal("recommendation must be executable")
	}
	if p.Recommendation.Analyzer != EngineNative {
		t.Fatalf("analyzer pick = %q", p.Recommendation.Analyzer)
	}
}

func TestAdviseGhOstUnsuitableWithForeignKeys(t *testing.T) {
	p := plan(t, withFK(baseTable()), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL",
		withBinaries(availableBinaries()))

	gh, _ := assessmentFor(p, EngineGhOst)
	if gh.Verdict != VerdictUnsuitable {
		t.Fatalf("gh-ost verdict = %q, want unsuitable", gh.Verdict)
	}
	if !unmetContains(gh, "no foreign keys in or out") {
		t.Fatalf("expected the FK requirement to fail, got %v", unmet(gh))
	}
	// It is installed, so "unsuitable" must be about the engine, not about us.
	if !gh.Installed {
		t.Fatal("gh-ost should be reported as installed")
	}
	if gh.Runnable() {
		t.Fatal("an unsuitable engine must never be runnable")
	}

	// pt-osc handles foreign keys, so it must not fail that requirement.
	pt, _ := assessmentFor(p, EnginePtOSC)
	if unmetContains(pt, "no foreign keys in or out") {
		t.Fatal("pt-osc should not be blocked by foreign keys")
	}
	if pt.Verdict != VerdictSuitable {
		t.Fatalf("pt-osc verdict = %q (%v)", pt.Verdict, unmet(pt))
	}
}

func TestAdviseGhOstSuitableWithoutForeignKeys(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL",
		withBinaries(availableBinaries()))

	gh, _ := assessmentFor(p, EngineGhOst)
	if gh.Verdict != VerdictSuitable {
		t.Fatalf("gh-ost verdict = %q (%v)", gh.Verdict, unmet(gh))
	}
	if !gh.Runnable() {
		t.Fatal("gh-ost should be runnable: suitable, installed, wrapped")
	}
	if p.Recommendation.Engine != EngineGhOst {
		t.Fatalf("recommended = %q", p.Recommendation.Engine)
	}
	if !p.Recommendation.Executable {
		t.Fatalf("recommendation must be executable, reason=%q", p.Recommendation.ExecutableReason)
	}
	if gh.Command == "" {
		t.Fatal("a suitable engine must show the command Saksama will run")
	}
}

func TestAdviseGhOstNotInstalledIsNotUnsuitable(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL",
		withBinaries(map[string]EngineBinary{
			"gh-ost": {Name: "gh-ost", Available: false, Error: "not found on PATH"},
		}))

	gh, _ := assessmentFor(p, EngineGhOst)
	if gh.Verdict != VerdictSuitable {
		t.Fatalf("verdict = %q; a missing binary must not change the verdict", gh.Verdict)
	}
	if gh.Installed {
		t.Fatal("binary should be reported as missing")
	}
	if gh.Runnable() {
		t.Fatal("a missing binary must make the engine not runnable")
	}
	if !strings.Contains(p.Recommendation.ExecutableReason, "not installed") {
		t.Fatalf("reason = %q", p.Recommendation.ExecutableReason)
	}
}

func TestAdviseCompositePrimaryKeyFavoursPtOSC(t *testing.T) {
	tbl := baseTable()
	tbl.Indexes[0] = model.Index{Name: "PRIMARY", Primary: true, Unique: true,
		Parts: []model.IndexPart{{Column: "id"}, {Column: "spot_id"}}}

	p := plan(t, tbl, "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL",
		withBinaries(availableBinaries()))

	gh, _ := assessmentFor(p, EngineGhOst)
	if gh.Verdict != VerdictUnsuitable {
		t.Fatalf("gh-ost verdict = %q", gh.Verdict)
	}
	if !unmetContains(gh, "single-column key to chunk by") {
		t.Fatalf("gh-ost should fail the single-column key requirement: %v", unmet(gh))
	}

	pt, _ := assessmentFor(p, EnginePtOSC)
	if pt.Verdict != VerdictSuitable {
		t.Fatalf("pt-osc should fit a composite key: %v", unmet(pt))
	}
}

func TestAdvisePtOSCUnsuitableWithoutKey(t *testing.T) {
	tbl := baseTable()
	tbl.Indexes = nil

	p := plan(t, tbl, "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL",
		withBinaries(availableBinaries()))
	pt, _ := assessmentFor(p, EnginePtOSC)
	if pt.Verdict != VerdictUnsuitable {
		t.Fatalf("pt-osc verdict = %q", pt.Verdict)
	}
}

func TestAdviseNativeUnsuitableWhenFKColumnTouched(t *testing.T) {
	p := plan(t, withFK(baseTable()), "8.0.36", "MODIFY COLUMN spot_id bigint unsigned NOT NULL")
	native, _ := assessmentFor(p, EngineNative)
	// Two requirements fail here: MODIFY COLUMN needs a rebuild, and the column is
	// constrained by a foreign key.
	if native.Verdict != VerdictUnsuitable {
		t.Fatalf("native verdict = %q, want unsuitable", native.Verdict)
	}
	if !unmetContains(native, "no table rebuild required") {
		t.Fatalf("rebuild requirement should fail: %v", unmet(native))
	}
	if !unmetContains(native, "no foreign-key column is modified") {
		t.Fatalf("expected the FK-column requirement to fail: %v", unmet(native))
	}
}

func TestAdvisePartitionedTableUnsuitableForGhOst(t *testing.T) {
	tbl := baseTable()
	tbl.Partitioned = true
	p := plan(t, tbl, "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL",
		withBinaries(availableBinaries()))
	gh, _ := assessmentFor(p, EngineGhOst)
	if gh.Verdict != VerdictUnsuitable {
		t.Fatalf("partitioned table must make gh-ost unsuitable: %q", gh.Verdict)
	}
	if !unmetContains(gh, "not partitioned") {
		t.Fatalf("blockers: %v", unmet(gh))
	}
}

func TestAdviseExplainsWithEvidenceAndTradeoffs(t *testing.T) {
	p := plan(t, withFK(baseTable()), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL",
		withBinaries(availableBinaries()))

	if len(p.TableFacts) == 0 {
		t.Fatal("no table facts: the advice must be traceable to observed schema")
	}
	var sawFKFact, sawPKFact bool
	for _, f := range p.TableFacts {
		if strings.Contains(f.Fact, "foreign key") {
			sawFKFact = true
		}
		if strings.Contains(f.Fact, "primary key") {
			sawPKFact = true
		}
		if f.Fact == "" || f.Effect == "" {
			t.Fatalf("incomplete evidence: %+v", f)
		}
	}
	if !sawFKFact || !sawPKFact {
		t.Fatalf("expected PK and FK facts, got %+v", p.TableFacts)
	}

	for _, a := range p.Engines {
		if len(a.Pros) == 0 || len(a.Cons) == 0 {
			t.Errorf("%s must list pros and cons", a.Engine)
		}
		if a.Summary == "" {
			t.Errorf("%s has no summary", a.Engine)
		}
	}
}

func TestAdviseUUIDPrimaryKeyWarnsInCons(t *testing.T) {
	tbl := model.Table{
		Schema: "shop", Name: "sessions", Engine: "InnoDB", RowFormat: "DYNAMIC",
		Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci",
		Columns: []model.Column{
			{Name: "uuid", DataType: "char", Type: "char(36)", Charset: "utf8mb4"},
			{Name: "at", DataType: "datetime", Type: "datetime"},
		},
		Indexes: []model.Index{{Name: "PRIMARY", Primary: true, Unique: true,
			Parts: []model.IndexPart{{Column: "uuid"}}}},
	}
	p := plan(t, tbl, "8.0.36", "MODIFY COLUMN at datetime(6) NOT NULL", withBinaries(availableBinaries()))

	gh, _ := assessmentFor(p, EngineGhOst)
	var mentioned bool
	for _, c := range gh.Cons {
		if strings.Contains(strings.ToLower(c), "uuid") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Fatalf("expected a UUID chunking caveat in gh-ost cons: %v", gh.Cons)
	}
}

// --- schema-wide assessment (advise without a table name) --------------------

func fleetTable(name string, mut func(*model.Table)) model.Table {
	t := model.Table{
		Schema: "shop", Name: name, Engine: "InnoDB", RowFormat: "DYNAMIC",
		Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci",
		Rows: 1000, DataLength: 1 << 20,
		Columns: []model.Column{
			{Name: "id", DataType: "bigint", Type: "bigint unsigned", Extra: "auto_increment"},
			{Name: "v", DataType: "int", Type: "int"},
		},
		Indexes: []model.Index{{Name: "PRIMARY", Primary: true, Unique: true,
			Parts: []model.IndexPart{{Column: "id"}}}},
	}
	if mut != nil {
		mut(&t)
	}
	return t
}

func TestAssessSchemaPicksTheEnginePerTableShape(t *testing.T) {
	tables := []model.Table{
		fleetTable("plain", nil),
		fleetTable("with_fk", func(t *model.Table) {
			t.ForeignKeys = []model.ForeignKey{{
				Name: "fk_a", Columns: []string{"v"}, RefTable: "other", RefColumns: []string{"id"},
				OnDelete: model.ActionCascade,
			}}
		}),
		fleetTable("composite", func(t *model.Table) {
			t.Indexes[0].Parts = append(t.Indexes[0].Parts, model.IndexPart{Column: "v"})
		}),
		fleetTable("no_key", func(t *model.Table) {
			t.Indexes = nil
		}),
		fleetTable("myisam", func(t *model.Table) {
			t.Engine = "MyISAM"
		}),
	}

	got := AssessSchema(tables, model.Server{Version: "8.0.36", Vendor: "mysql"},
		map[string]EngineBinary{
			"gh-ost":                  {Name: "gh-ost", Available: true, Version: "1.1.7"},
			"pt-online-schema-change": {Name: "pt-online-schema-change", Available: true, Version: "3.5.1"},
		}, binlogOn())

	byName := map[string]TableAssessment{}
	for _, r := range got {
		byName[r.Table] = r
	}

	if e := byName["plain"].RebuildEngine; e != EngineGhOst {
		t.Errorf("plain: got %q, want gh-ost", e)
	}
	if e := byName["with_fk"].RebuildEngine; e != EnginePtOSC {
		t.Errorf("with_fk: got %q, want pt-osc", e)
	}
	if e := byName["composite"].RebuildEngine; e != EnginePtOSC {
		t.Errorf("composite: got %q, want pt-osc", e)
	}
	if e := byName["no_key"].RebuildEngine; e != EngineNone {
		t.Errorf("no_key: got %q, want none", e)
	}
	if len(byName["no_key"].Blockers) == 0 {
		t.Error("no_key must explain why no engine fits")
	}
	if e := byName["myisam"].RebuildEngine; e != EngineNone {
		t.Errorf("myisam: got %q, want none", e)
	}

	// Biggest table first, so the expensive decisions are on top.
	for i := 1; i < len(got); i++ {
		if got[i-1].SizeBytes < got[i].SizeBytes {
			t.Fatalf("not sorted by size: %+v", got)
		}
	}
}

func TestAssessSchemaDescribesPrimaryKey(t *testing.T) {
	tables := []model.Table{
		fleetTable("simple", nil),
		fleetTable("composite", func(t *model.Table) {
			t.Indexes[0].Parts = append(t.Indexes[0].Parts, model.IndexPart{Column: "v"})
		}),
		fleetTable("none", func(t *model.Table) { t.Indexes = nil }),
		fleetTable("uuid", func(t *model.Table) {
			t.Columns = []model.Column{{Name: "uuid", DataType: "char", Type: "char(36)", Charset: "utf8mb4"}}
			t.Indexes[0].Parts = []model.IndexPart{{Column: "uuid"}}
		}),
	}
	got := AssessSchema(tables, model.Server{Version: "8.0.36"}, nil, nil)
	byName := map[string]TableAssessment{}
	for _, r := range got {
		byName[r.Table] = r
	}

	if !strings.Contains(byName["simple"].PrimaryKey, "id bigint") {
		t.Errorf("simple PK = %q", byName["simple"].PrimaryKey)
	}
	if !strings.HasPrefix(byName["composite"].PrimaryKey, "composite(") {
		t.Errorf("composite PK = %q", byName["composite"].PrimaryKey)
	}
	if byName["none"].PrimaryKey != "(none)" {
		t.Errorf("none PK = %q", byName["none"].PrimaryKey)
	}
	if !strings.Contains(byName["uuid"].PrimaryKey, "char(36)") {
		t.Errorf("uuid PK = %q", byName["uuid"].PrimaryKey)
	}
}

// --- server environment checks ----------------------------------------------

// binlogOn is a server configured the way gh-ost needs it.
func binlogOn() map[string]string {
	return map[string]string{
		"log_bin":                 "ON",
		"binlog_format":           "ROW",
		"binlog_row_image":        "FULL",
		"server_id":               "1",
		"read_only":               "OFF",
		"super_read_only":         "OFF",
		"performance_schema":      "ON",
		"innodb_file_per_table":   "ON",
		"sql_require_primary_key": "OFF",
		"transaction_isolation":   "REPEATABLE-READ",
	}
}

func binlogOff() map[string]string {
	m := binlogOn()
	m["log_bin"] = "OFF"
	return m
}

func settingsPlan(t *testing.T, settings map[string]string, body string) Plan {
	t.Helper()
	return plan(t, baseTable(), "8.0.36", body, func(in *Input) {
		in.Binaries = availableBinaries()
		in.Settings = SettingsFromMap(settings)
	})
}

func TestGhOstRequiresBinaryLogging(t *testing.T) {
	p := settingsPlan(t, binlogOff(), "MODIFY COLUMN status varchar(64) NOT NULL")

	gh, _ := assessmentFor(p, EngineGhOst)
	if gh.Verdict != VerdictSuitable {
		t.Fatalf("the engine still suits the table, got %q", gh.Verdict)
	}
	if gh.EnvironmentReady() {
		t.Fatal("log_bin=OFF must leave the environment unmet")
	}
	if gh.Runnable() {
		t.Fatal("an engine that cannot start must not be reported runnable")
	}
	// The tool picks the engine this server can actually run, and says why it did
	// not pick the cheaper one.
	if p.Recommendation.Engine != EnginePtOSC {
		t.Fatalf("got %q, want pt-osc as the fallback", p.Recommendation.Engine)
	}
	if !hasCheck(p, "R9", SeverityWarn) {
		t.Fatalf("expected R9 explaining the skipped engine: %+v", p.Checks)
	}
}

// An explicit choice is honoured even when the server cannot run it: the plan
// refuses with a fixable blocker instead of silently switching engines.
func TestChosenEngineBlockedByServerConfig(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL", func(in *Input) {
		in.Binaries = availableBinaries()
		in.Settings = SettingsFromMap(binlogOff())
		in.EngineHint = EngineGhOst
		in.EngineHintSource = "recorded"
	})

	if p.Recommendation.Engine != EngineGhOst {
		t.Fatalf("got %q, want the chosen gh-ost", p.Recommendation.Engine)
	}
	if !hasCheck(p, "R8", SeverityBlocker) {
		t.Fatalf("expected R8 for the chosen engine: %+v", p.Checks)
	}
	if p.Recommendation.Executable {
		t.Fatal("the plan must not claim it can run")
	}
	if !strings.Contains(p.Recommendation.ExecutableReason, "not configured") {
		t.Fatalf("reason = %q", p.Recommendation.ExecutableReason)
	}
}

func TestGhOstRunnableWhenServerIsConfigured(t *testing.T) {
	p := settingsPlan(t, binlogOn(), "MODIFY COLUMN status varchar(64) NOT NULL")

	gh, _ := assessmentFor(p, EngineGhOst)
	if !gh.EnvironmentReady() {
		t.Fatalf("unmet prerequisites: %v", gh.EnvironmentProblems())
	}
	if !gh.Runnable() {
		t.Fatal("gh-ost should be runnable on a properly configured server")
	}
	if p.Recommendation.Engine != EngineGhOst {
		t.Fatalf("got %q, want gh-ost", p.Recommendation.Engine)
	}
}

func TestBinlogRowImageMustBeFull(t *testing.T) {
	m := binlogOn()
	m["binlog_row_image"] = "MINIMAL"
	p := settingsPlan(t, m, "MODIFY COLUMN status varchar(64) NOT NULL")

	gh, _ := assessmentFor(p, EngineGhOst)
	if gh.EnvironmentReady() {
		t.Fatal("MINIMAL row image must block gh-ost")
	}
	var named bool
	for _, problem := range gh.EnvironmentProblems() {
		if problem == "binlog_row_image is FULL" {
			named = true
		}
	}
	if !named {
		t.Fatalf("problems = %v", gh.EnvironmentProblems())
	}
}

func TestReadOnlyServerBlocksEveryEngine(t *testing.T) {
	m := binlogOn()
	m["read_only"] = "ON"
	p := settingsPlan(t, m, "MODIFY COLUMN status varchar(64) NOT NULL")

	for _, e := range []Engine{EngineNative, EngineGhOst, EnginePtOSC} {
		a, ok := assessmentFor(p, e)
		if !ok {
			t.Fatalf("%s missing", e)
		}
		if a.EnvironmentReady() {
			t.Errorf("%s should be blocked by read_only", e)
		}
	}
}

func TestSettingChecksGradeEachRow(t *testing.T) {
	checks := SettingChecks(SettingsFromMap(binlogOff()))
	bySetting := map[string]SettingCheck{}
	for _, c := range checks {
		bySetting[c.Setting] = c
	}

	if bySetting["log_bin"].Status != EnvFail {
		t.Errorf("log_bin = %q, want fail", bySetting["log_bin"].Status)
	}
	if bySetting["log_bin"].Required != "ON" {
		t.Errorf("required = %q", bySetting["log_bin"].Required)
	}
	if bySetting["binlog_format"].Status != EnvOK {
		t.Errorf("binlog_format = %q", bySetting["binlog_format"].Status)
	}
	if len(bySetting["log_bin"].NeededBy) == 0 {
		t.Error("log_bin must name the engines that need it")
	}

	fails, _, _ := EnvironmentSummary(checks)
	if fails != 1 {
		t.Errorf("fails = %d, want 1", fails)
	}
	if hints := FixHints(checks); len(hints) != 1 {
		t.Errorf("hints = %v", hints)
	}
}

func TestSettingChecksReportUnknownWithoutLiveState(t *testing.T) {
	checks := SettingChecks(SettingsFromMap(nil))
	for _, c := range checks {
		if c.Status != EnvUnknown {
			t.Errorf("%s = %q, want unknown when the server was not inspected", c.Setting, c.Status)
		}
		if c.Current != "(not inspected)" {
			t.Errorf("%s current = %q", c.Setting, c.Current)
		}
	}
	if hints := FixHints(checks); len(hints) != 0 {
		t.Errorf("no fix hints should be offered for unknown settings: %v", hints)
	}
}

func TestUnknownSettingsDoNotBlockRunnable(t *testing.T) {
	// Offline plans must not pretend the environment is broken.
	p := plan(t, baseTable(), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL", func(in *Input) {
		in.Binaries = availableBinaries()
		in.Settings = ServerSettings{}
	})
	gh, _ := assessmentFor(p, EngineGhOst)
	if !gh.EnvironmentReady() {
		t.Fatalf("unknown settings should not fail: %v", gh.EnvironmentProblems())
	}
}

// --- disk space (D6) --------------------------------------------------------
//
// MySQL cannot report free filesystem space over SQL, so this is either measured
// on the database host, supplied by the operator, or unknown. Unknown must never
// read as safe.

const gib = int64(1) << 30

func diskInput(free, total int64, measured bool) func(*Input) {
	return func(in *Input) {
		in.Disk = DiskFacts{Free: free, Total: total, Measured: measured, Source: "filesystem"}
	}
}

func TestDiskBlocksWhenVolumeIsBelowTenPercent(t *testing.T) {
	// 40 GiB of 500 GiB is 8 percent: enough for a small change, still refused.
	p := plan(t, baseTable(), "8.0.36", "ADD INDEX ix (created_at)",
		diskInput(40*gib, 500*gib, true), func(in *Input) { in.ForExecution = true })

	found := false
	for _, c := range p.Checks {
		if c.ID == chkDiskHeadroom && c.Severity == SeverityBlocker {
			found = true
			if !strings.Contains(c.Title, "10%") {
				t.Errorf("title should name the floor: %q", c.Title)
			}
		}
	}
	if !found {
		t.Fatalf("expected a blocker for a volume below 10 percent: %+v", p.Checks)
	}
	if !p.Recommendation.Blocked {
		t.Fatal("the plan must be blocked")
	}
}

func TestDiskAcceptsHealthyVolume(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "ADD INDEX ix (created_at)",
		diskInput(400*gib, 500*gib, true), func(in *Input) { in.ForExecution = true })

	for _, c := range p.Checks {
		if c.ID == chkDiskHeadroom && c.Severity != SeverityInfo {
			t.Fatalf("expected an informational disk check, got %q: %s", c.Severity, c.Title)
		}
	}
}

func TestDiskWarnsBetweenTenAndTwentyFivePercent(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "ADD INDEX ix (created_at)",
		diskInput(60*gib, 500*gib, true), func(in *Input) { in.ForExecution = true })

	var warn bool
	for _, c := range p.Checks {
		if c.ID == chkDiskHeadroom && c.Severity == SeverityWarn {
			warn = true
		}
	}
	if !warn {
		t.Fatalf("12 percent free should warn: %+v", p.Checks)
	}
}

func TestDiskBlocksWhenCopyDoesNotFit(t *testing.T) {
	// 20 GiB free of 100 GiB is above the floor, but this 22 GiB table needs 44 GiB
	// for a copy, so the fit check must fire on its own.
	p := plan(t, baseTable(), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL",
		diskInput(20*gib, 100*gib, true),
		withBinaries(availableBinaries()),
		func(in *Input) { in.ForExecution = true })

	var blocker *Check
	for i, c := range p.Checks {
		if c.ID == chkDiskHeadroom && c.Severity == SeverityBlocker {
			blocker = &p.Checks[i]
		}
	}
	if blocker == nil {
		t.Fatalf("expected a blocker: a 22 GiB table needs 44 GiB, only 20 free: %+v", p.Checks)
	}
	if !strings.Contains(blocker.Title, "copy") {
		t.Errorf("title should name the copy: %q", blocker.Title)
	}
}

func TestDiskUnmeasuredBlocksExecutionOnly(t *testing.T) {
	// A plan is useful without a measured disk. An execution is not.
	planned := plan(t, baseTable(), "8.0.36", "ADD INDEX ix (created_at)", diskInput(0, 0, false))
	for _, c := range planned.Checks {
		if c.ID == chkDiskHeadroom && c.Severity == SeverityBlocker {
			t.Fatalf("planning must not be blocked by an unmeasured disk: %s", c.Title)
		}
	}

	executing := plan(t, baseTable(), "8.0.36", "ADD INDEX ix (created_at)", diskInput(0, 0, false),
		func(in *Input) { in.ForExecution = true })
	var blocked bool
	for _, c := range executing.Checks {
		if c.ID == chkDiskHeadroom && c.Severity == SeverityBlocker {
			blocked = true
			if !strings.Contains(c.Remediation, "--free-disk") {
				t.Errorf("remediation must say how to satisfy it: %q", c.Remediation)
			}
		}
	}
	if !blocked {
		t.Fatalf("execution with an unmeasured disk must be refused: %+v", executing.Checks)
	}
}

func TestDiskUnmeasuredAllowedByForce(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "ADD INDEX ix (created_at)", diskInput(0, 0, false),
		func(in *Input) { in.ForExecution = true; in.Force = true })

	for _, c := range p.Checks {
		if c.ID == chkDiskHeadroom && c.Severity == SeverityBlocker {
			t.Fatalf("--force should downgrade an unmeasured disk: %s", c.Title)
		}
	}
}

func TestDiskOperatorSuppliedNumbersAreUsed(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "ADD INDEX ix (created_at)", func(in *Input) {
		in.ForExecution = true
		in.FreeDisk = 30 * gib // 6 percent of the volume below
		in.Disk = DiskFacts{Free: 30 * gib, Total: 500 * gib, Measured: true, Source: "operator"}
	})

	var blocked bool
	for _, c := range p.Checks {
		if c.ID == chkDiskHeadroom && c.Severity == SeverityBlocker {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("operator-supplied numbers must be judged too: %+v", p.Checks)
	}
}

// The floor is 5.6. Applications that started in 2015 are still running, and
// every fact the matrix needs exists on 5.6, so it is planned rather than
// refused - with one caveat that must never disappear from the output.
func TestPlanAcceptsMySQL56WithTheMDLCaveat(t *testing.T) {
	p := plan(t, baseTable(), "5.6.51", "ADD INDEX ix_c (status)")
	if p.Recommendation.Blocked {
		t.Fatalf("5.6 is the supported floor and must plan: %+v", p.Checks)
	}
	if !hasCheck(p, "E5b", SeverityWarn) {
		t.Fatal("a 5.6 plan must carry E5b: the metadata lock queue cannot be observed")
	}
	if hasCheck(p, "E5", SeverityBlocker) {
		t.Fatal("5.6 must no longer hit the too-old blocker")
	}

	// In production the missing lock queue is a blocker unless the operator says
	// the risk is understood: the queue is the outage this tool exists to avoid.
	prod := plan(t, baseTable(), "5.6.51", "ADD INDEX ix_c (status)", func(in *Input) { in.Production = true })
	if !hasCheck(prod, "E5b", SeverityBlocker) {
		t.Fatal("a production 5.6 plan must block on the unobservable lock queue")
	}
	accepted := plan(t, baseTable(), "5.6.51", "ADD INDEX ix_c (status)", func(in *Input) {
		in.Production = true
		in.AcceptUnknownMDL = true
	})
	if hasCheck(accepted, "E5b", SeverityBlocker) {
		t.Fatal("--accept-unknown-mdl must clear the 5.6 production blocker")
	}
}

// 5.5 and older stay refused: gh-ost needs 5.6, and 5.5 reached end of life in
// 2018.
func TestPlanRefusesMySQL55(t *testing.T) {
	p := plan(t, baseTable(), "5.5.62", "ADD INDEX ix_c (status)")
	if !p.Recommendation.Blocked {
		t.Fatal("a plan against MySQL 5.5 must be blocked, not guessed")
	}
	if !hasCheck(p, "E5", SeverityBlocker) {
		t.Fatal("check E5 (server too old) is missing from the plan")
	}
	// The boundary matters: 5.6 stays plannable.
	if p := plan(t, baseTable(), "5.6.51", "ADD INDEX ix_c (status)"); p.Recommendation.Blocked {
		t.Fatalf("5.6 must not be caught by the 5.5 refusal: %+v", p.Recommendation)
	}
}

// A rebuild engine that fits the table but cannot run on this machine is a
// recommendation nobody can follow, so the two facts are reported apart.
func TestAssessSchemaSaysWhenTheEngineIsNotInstalled(t *testing.T) {
	tables := []model.Table{fleetTable("plain", nil)}

	got := AssessSchema(tables, model.Server{Version: "8.0.36", Vendor: "mysql"},
		map[string]EngineBinary{
			"gh-ost":                  {Name: "gh-ost", Available: false},
			"pt-online-schema-change": {Name: "pt-online-schema-change", Available: true, Version: "3.5.1"},
		}, binlogOn())

	if len(got) != 1 {
		t.Fatalf("got %d assessments", len(got))
	}
	a := got[0]
	if a.RebuildEngine != EngineGhOst {
		t.Fatalf("the engine that fits must stay gh-ost, got %q", a.RebuildEngine)
	}
	if a.RebuildReady {
		t.Fatal("gh-ost is not installed, so it cannot be ready")
	}
	if a.RebuildBlocked != "not installed" {
		t.Errorf("got %q, want \"not installed\"", a.RebuildBlocked)
	}
	if !strings.Contains(a.RebuildFix, "gh-ost") {
		t.Errorf("the fix must name the binary, got %q", a.RebuildFix)
	}
}

// --no-probe means nobody looked. Unknown is not a pass.
func TestAssessSchemaReportsUnprobedBinariesAsUnproven(t *testing.T) {
	got := AssessSchema([]model.Table{fleetTable("plain", nil)},
		model.Server{Version: "8.0.36", Vendor: "mysql"}, nil, binlogOn())

	if got[0].RebuildReady {
		t.Fatal("an unprobed binary cannot be reported as ready")
	}
	if got[0].RebuildBlocked != "not probed" {
		t.Errorf("got %q, want \"not probed\"", got[0].RebuildBlocked)
	}
}

// The native question is answered by the version gated matrix, so a server that
// the matrix refuses cannot be promised native DDL.
func TestNativeIndexPathFollowsTheCapabilityMatrix(t *testing.T) {
	ok := []model.Server{
		{Version: "8.0.46", Vendor: "mysql"},
		{Version: "8.4.13", Vendor: "mysql"},
		{Version: "5.7.44", Vendor: "mysql"},
		// Online DDL for index creation arrived in 5.6, so the native claim holds.
		{Version: "5.6.51", Vendor: "mysql"},
	}
	for _, srv := range ok {
		yes, detail := NativeIndexPath(srv)
		if !yes {
			t.Errorf("%s: expected native index DDL, got %q", srv.Version, detail)
		}
		if !strings.Contains(detail, "INPLACE") || !strings.Contains(detail, "LOCK=NONE") {
			t.Errorf("%s: detail must cite the algorithm and the lock: %q", srv.Version, detail)
		}
	}

	no := []model.Server{
		{Version: "5.4.4", Vendor: "mysql"},
		{Version: "5.5.62", Vendor: "mysql"},
		{Version: "10.11.6-MariaDB-log", Vendor: "mariadb"},
		{Version: "", Vendor: "mysql"},
		{Version: "not-a-version", Vendor: "mysql"},
	}
	for _, srv := range no {
		yes, detail := NativeIndexPath(srv)
		if yes {
			t.Errorf("%s: claimed native DDL the matrix does not cover", srv.Version)
		}
		if detail == "" {
			t.Errorf("%s: a refusal needs a reason", srv.Version)
		}
	}
}

// An index on a column that is not there is error 1072, not a key-length problem.
// The width estimator has to assume *something* for an unknown column, and
// without this check that assumption reached the operator as "key too long".
func TestPlanNamesAMissingIndexColumn(t *testing.T) {
	p := plan(t, baseTable(), "5.6.51", "ADD INDEX ix_nope (does_not_exist)")
	if !hasCheck(p, "A8", SeverityBlocker) {
		t.Fatalf("expected A8 for an unknown index column: %+v", p.Checks)
	}
	for _, c := range p.Checks {
		if c.ID == "A5" {
			t.Fatalf("A5 must not fire for a column that does not exist: %+v", c)
		}
		if c.ID == "A8" && c.MySQLError != 1072 {
			t.Errorf("A8 must cite MySQL 1072, got %d", c.MySQLError)
		}
	}
}

// gh-ost refuses to start on a server without performance_schema.metadata_locks
// ("bailing out because metadata lock instrument not enabled"), so the wrapper has
// to pass --skip-metadata-lock-check there. The plan must say the same thing: an
// engine running with its lock check switched off is a blind spot, not a detail.
func TestGhOstOnMySQL56AdmitsItCannotCheckTheLockQueue(t *testing.T) {
	fk := func(t *model.Table) {
		t.ForeignKeys = nil
		t.Indexes[0].Parts = []model.IndexPart{{Column: "id"}}
		t.Columns = append(t.Columns, model.Column{Name: "status", DataType: "varchar", Type: "varchar(32)"})
	}
	binaries := map[string]EngineBinary{
		"gh-ost": {Name: "gh-ost", Available: true, Version: "1.1.11"},
	}
	in := Input{
		Server:   model.Server{Version: "5.6.51", Vendor: "mysql"},
		Table:    fleetTable("plain", fk),
		Ops:      []model.Op{{Kind: model.OpModifyColumn}},
		Binaries: binaries,
		Settings: SettingsFromMap(binlogOn()),
	}
	p := Analyze(in)
	if p.Recommendation.Engine != EngineGhOst {
		t.Fatalf("expected gh-ost on 5.6 with binlog on, got %q", p.Recommendation.Engine)
	}
	if !hasCheck(p, "R11", SeverityWarn) {
		t.Fatalf("a gh-ost plan on 5.6 must carry R11: %+v", p.Checks)
	}
	// The same plan on 5.7 must not carry it: the instrument exists there.
	in.Server = model.Server{Version: "5.7.44", Vendor: "mysql"}
	p57 := Analyze(in)
	if p57.Recommendation.Engine == EngineGhOst && hasCheck(p57, "R11", SeverityWarn) {
		t.Errorf("gh-ost on 5.7 can check the lock queue and must not warn about it")
	}
}
