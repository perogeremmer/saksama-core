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
		})

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
	got := AssessSchema(tables, model.Server{Version: "8.0.36"}, nil)
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
