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
		{"8.0.11", "ADD COLUMN a INT NOT NULL DEFAULT 0", EngineGhOst, "R-ADD-COLUMN-REBUILD-INPLACE"},
		{"8.0.12", "ADD COLUMN a INT NOT NULL DEFAULT 0", EngineNative, "R-ADD-COLUMN-INSTANT-8012"},
		{"8.0.12", "ADD COLUMN a INT NOT NULL DEFAULT 0 AFTER status", EngineGhOst, "R-ADD-COLUMN-MIDDLE-COPY"},
		{"8.0.29", "ADD COLUMN a INT NOT NULL DEFAULT 0 AFTER status", EngineNative, "R-ADD-COLUMN-INSTANT-8029"},
		// 59M rows: the rule is INPLACE-with-rebuild, and the size is what keeps
		// the answer a copy engine.
		{"5.7.44", "ADD COLUMN a INT NOT NULL DEFAULT 0", EngineGhOst, "R-ADD-COLUMN-REBUILD-INPLACE"},
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
	// Two requirements fail here: the change needs the COPY algorithm, which this
	// engine does not run, and the column is constrained by a foreign key.
	if native.Verdict != VerdictUnsuitable {
		t.Fatalf("native verdict = %q, want unsuitable", native.Verdict)
	}
	if !unmetContains(native, "no operation needs the COPY algorithm") {
		t.Fatalf("the COPY-algorithm requirement should fail: %v", unmet(native))
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
	// A plan is useful without a measured disk. An execution is not - but only when
	// the execution would actually write the table. This used to use ADD INDEX,
	// which copies nothing: the check now follows the change, so the case that must
	// still be refused is a rebuild.
	planned := plan(t, baseTable(), "5.6.51", "ADD COLUMN note varchar(64) NULL", diskInput(0, 0, false))
	for _, c := range planned.Checks {
		if c.ID == chkDiskHeadroom && c.Severity == SeverityBlocker {
			t.Fatalf("planning must not be blocked by an unmeasured disk: %s", c.Title)
		}
	}

	executing := plan(t, baseTable(), "5.6.51", "ADD COLUMN note varchar(64) NULL", diskInput(0, 0, false),
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
	p := plan(t, baseTable(), "5.6.51", "ADD COLUMN note varchar(64) NULL", diskInput(0, 0, false),
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
		// Forced on purpose: on a small table the analyzer would now prefer
		// native rebuild, and this test is about what gh-ost reports on 5.6.
		EngineHint:       EngineGhOst,
		EngineHintSource: "flag",
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

// The rebuild gate, as a table: the size decides, and production needs the
// operator to say the trade out loud. On 5.6 and 5.7 the server performs ADD
// COLUMN in place with LOCK=NONE, so a small table needs no install and no
// restart - while thirty million rows is exactly what a copy engine is for.
func TestNativeRebuildIsGatedBySizeAndProduction(t *testing.T) {
	small := func() model.Table {
		tbl := baseTable()
		tbl.Rows = 4_000
		tbl.DataLength = 512 << 10
		tbl.IndexLength = 0
		return tbl
	}
	const addColumn = "ADD COLUMN note varchar(64) NULL"

	t.Run("thirty million rows is a copy engine", func(t *testing.T) {
		p := plan(t, baseTable(), "5.6.51", addColumn)
		if p.Recommendation.Engine == EngineNative {
			t.Fatalf("a 59M-row table must not be rebuilt by native: %+v", p.Recommendation)
		}
		if !hasRule(p, "R-ADD-COLUMN-REBUILD-INPLACE") {
			t.Fatalf("the operation should still be described as an in-place rebuild: %+v", p.Operations)
		}
		if !strings.Contains(nativeRequirement(p), "throttle") {
			t.Fatalf("the refusal must say what a copy engine gives that native does not: %q", nativeRequirement(p))
		}
	})

	t.Run("a few thousand rows runs natively", func(t *testing.T) {
		p := plan(t, small(), "5.6.51", addColumn)
		if p.Recommendation.Engine != EngineNative {
			t.Fatalf("a 4k-row table should need no install: got %q", p.Recommendation.Engine)
		}
		if !hasCheck(p, "R12", SeverityWarn) {
			t.Fatalf("a native rebuild must carry R12: %+v", p.Checks)
		}
	})

	t.Run("production needs the flag", func(t *testing.T) {
		// AcceptUnknownMDL isolates the rebuild gate from E5b, which is a
		// separate production blocker on 5.6 and has its own tests.
		p := plan(t, small(), "5.6.51", addColumn, func(in *Input) {
			in.Production = true
			in.AcceptUnknownMDL = true
		})
		if p.Recommendation.Engine == EngineNative {
			t.Fatal("production must not rebuild in place without --allow-native-rebuild")
		}
		if !hasCheck(p, "R13", SeverityWarn) {
			t.Fatalf("the operator must be told the flag exists: %+v", p.Checks)
		}
		if !strings.Contains(checkDetail(p, "R13"), "--allow-native-rebuild") {
			t.Fatalf("R13 must name the flag: %q", checkDetail(p, "R13"))
		}
	})

	t.Run("production with the flag runs natively", func(t *testing.T) {
		p := plan(t, small(), "5.6.51", addColumn, func(in *Input) {
			in.Production = true
			in.AcceptUnknownMDL = true
			in.AllowNativeRebuild = true
		})
		if p.Recommendation.Engine != EngineNative {
			t.Fatalf("with the flag, native should run: got %q", p.Recommendation.Engine)
		}
		if !hasCheck(p, "R12", SeverityWarn) {
			t.Fatalf("the trade must still be named: %+v", p.Checks)
		}
	})

	t.Run("unknown size is not a pass", func(t *testing.T) {
		tbl := baseTable()
		tbl.Rows = 0
		tbl.DataLength = 0
		p := plan(t, tbl, "5.6.51", addColumn)
		if p.Recommendation.Engine == EngineNative {
			t.Fatal("an unmeasured table must not be rebuilt in place")
		}
		if !strings.Contains(nativeRequirement(p), "not measured") {
			t.Fatalf("the refusal must say the size is unknown: %q", nativeRequirement(p))
		}
	})
}

// nativeRequirement returns the unmet "no table rebuild required" detail from
// native's own assessment, which is where the reasoning lives.
func nativeRequirement(p Plan) string {
	for _, a := range p.Engines {
		if a.Engine != EngineNative {
			continue
		}
		for _, r := range a.Requirements {
			if r.Name == "no table rebuild required" {
				return r.Detail
			}
		}
	}
	return ""
}

func checkDetail(p Plan, id string) string {
	for _, c := range p.Checks {
		if c.ID == id {
			return c.Title + " " + c.Detail + " " + c.Remediation
		}
	}
	return ""
}

// The byte-prefix line, as a table. Measured on real servers: on a utf8mb4 column
// MySQL fills in place up to varchar(63) (252 bytes, one length byte) and refuses
// varchar(64) (256 bytes, two), and on 5.6 it refuses the widening outright. The
// rule has to reproduce that or it sends someone to gh-ost for nothing - or worse,
// promises INPLACE for a change the server answers with error 1846.
func TestWidenVarcharInPlaceFollowsTheBytePrefix(t *testing.T) {
	latin1 := baseTable()
	for i := range latin1.Columns {
		if latin1.Columns[i].Name == "status" {
			latin1.Columns[i].Charset = "latin1"
			latin1.Columns[i].Collation = "latin1_swedish_ci"
		}
	}

	cases := []struct {
		name    string
		table   model.Table
		version string
		alter   string
		rule    string
		engine  Engine
	}{
		{
			name: "utf8mb4 32 to 40 stays in one length byte", table: baseTable(), version: "8.0.36",
			alter: "MODIFY COLUMN status varchar(40) NOT NULL", rule: "R-MODIFY-COLUMN-WIDEN-INPLACE", engine: EngineNative,
		},
		{
			name: "utf8mb4 32 to 64 crosses into two", table: baseTable(), version: "8.0.36",
			alter: "MODIFY COLUMN status varchar(64) NOT NULL", rule: "R-MODIFY-COLUMN-COPY", engine: EngineGhOst,
		},
		{
			name: "latin1 32 to 200 still fits", table: latin1, version: "5.7.44",
			alter: "MODIFY COLUMN status varchar(200) NOT NULL", rule: "R-MODIFY-COLUMN-WIDEN-INPLACE", engine: EngineNative,
		},
		{
			name: "latin1 32 to 300 crosses into two", table: latin1, version: "5.7.44",
			alter: "MODIFY COLUMN status varchar(300) NOT NULL", rule: "R-MODIFY-COLUMN-COPY", engine: EngineGhOst,
		},
		{
			name: "5.6 refuses the widening in place", table: latin1, version: "5.6.51",
			alter: "MODIFY COLUMN status varchar(200) NOT NULL", rule: "R-MODIFY-COLUMN-COPY", engine: EngineGhOst,
		},
		{
			name: "narrowing is a rebuild", table: latin1, version: "5.7.44",
			alter: "MODIFY COLUMN status varchar(16) NOT NULL", rule: "R-MODIFY-COLUMN-COPY", engine: EngineGhOst,
		},
		{
			name: "widening that also relaxes nullability is a rebuild", table: latin1, version: "5.7.44",
			alter: "MODIFY COLUMN status varchar(200) NULL", rule: "R-MODIFY-COLUMN-COPY", engine: EngineGhOst,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := plan(t, c.table, c.version, c.alter)
			if !hasRule(p, c.rule) {
				t.Fatalf("%s on %s: want rule %s, got %+v", c.alter, c.version, c.rule, p.Operations)
			}
			if p.Recommendation.Engine != c.engine {
				t.Fatalf("%s on %s: engine %q, want %q", c.alter, c.version, p.Recommendation.Engine, c.engine)
			}
		})
	}
}

// A default change is metadata on every supported version: INSTANT on 8.0.12+,
// in place with no rebuild before that. Calling it a rebuild on 5.6 and 5.7 sent
// operators to gh-ost - and on 5.6 that means accepting the unobservable lock
// queue - to change a default.
func TestDefaultChangeIsMetadataEverywhere(t *testing.T) {
	cases := []struct{ version, rule string }{
		{"5.6.51", "R-DEFAULT-INPLACE"},
		{"5.7.44", "R-DEFAULT-INPLACE"},
		{"8.0.11", "R-DEFAULT-INPLACE"},
		{"8.0.12", "R-DEFAULT-INSTANT"},
		{"8.4.0", "R-DEFAULT-INSTANT"},
	}
	for _, c := range cases {
		t.Run(c.version, func(t *testing.T) {
			p := plan(t, baseTable(), c.version, "ALTER COLUMN status SET DEFAULT 'queued'")
			if !hasRule(p, c.rule) {
				t.Fatalf("want rule %s, got %+v", c.rule, p.Operations)
			}
			if p.Recommendation.Engine != EngineNative {
				t.Fatalf("engine %q, want native", p.Recommendation.Engine)
			}
			for _, op := range p.Operations {
				if op.Method == MethodCopy || op.Method == MethodUnsupported {
					t.Fatalf("a default change must not be planned as a rebuild on %s: %+v", c.version, op)
				}
			}
		})
	}
}

// The disk gate has to follow the change, not the calendar. An unmeasured volume
// must not block a change that does not copy anything - that is the same mistake as
// demanding a package install for something MySQL does itself - and it must still
// block one that does, including when a copy engine was chosen for an operation that
// would not have needed a copy.
func TestUnmeasuredDiskBlocksOnlyChangesThatCopy(t *testing.T) {
	executing := func(in *Input) { in.ForExecution = true }

	t.Run("ADD INDEX is not blocked", func(t *testing.T) {
		p := plan(t, baseTable(), "8.0.36", "ADD INDEX ix_status (status)", executing)
		if hasCheck(p, "D6", SeverityBlocker) {
			t.Fatalf("an index build does not copy the table and must not be blocked for a missing disk figure: %+v", checksOf(p, "D6"))
		}
		if !hasCheck(p, "D6", SeverityWarn) {
			t.Fatalf("the missing figure should still be mentioned: %+v", checksOf(p, "D6"))
		}
	})

	t.Run("a rebuild in place is blocked", func(t *testing.T) {
		p := plan(t, baseTable(), "5.6.51", "ADD COLUMN note varchar(64) NULL", executing)
		if !hasCheck(p, "D6", SeverityBlocker) {
			t.Fatalf("an in-place rebuild writes the whole table and needs the figure: %+v", checksOf(p, "D6"))
		}
	})

	t.Run("a copy engine is blocked even for ADD INDEX", func(t *testing.T) {
		p := plan(t, baseTable(), "8.0.36", "ADD INDEX ix_status (status)", executing, func(in *Input) {
			in.EngineHint = EngineGhOst
			in.EngineHintSource = "flag"
		})
		if !hasCheck(p, "D6", SeverityBlocker) {
			t.Fatalf("gh-ost copies the table whatever the operations need: %+v", checksOf(p, "D6"))
		}
	})
}

func checksOf(p Plan, id string) []Check {
	var out []Check
	for _, c := range p.Checks {
		if c.ID == id {
			out = append(out, c)
		}
	}
	return out
}

// A MySQL in a container is a normal setup, and the disk question has one command as
// an answer. The hint must name the container the server says it is, and must not
// pretend to know when the server says nothing useful.
func TestContainerDiskHint(t *testing.T) {
	cases := []struct {
		name  string
		facts DiskFacts
		want  string
	}{
		{"docker id", DiskFacts{ServerHost: "f261c7614f06", Path: "/var/lib/mysql/"}, "docker exec f261c7614f06 df -B1 /var/lib/mysql/"},
		{"compose service name", DiskFacts{ServerHost: "mysql-prod", Path: "/var/lib/mysql"}, "docker exec mysql-prod df -B1 /var/lib/mysql"},
		{"no path falls back to the default", DiskFacts{ServerHost: "mysql-prod"}, "df -B1 /var/lib/mysql"},
		{"nothing known", DiskFacts{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := containerDiskHint(c.facts)
			if c.want == "" {
				if got != "" {
					t.Fatalf("hint = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, c.want) {
				t.Fatalf("hint = %q, want it to contain %q", got, c.want)
			}
		})
	}
}

// The blocker offers all three answers: the numbers, the container command, and
// --force with its audit trail.
func TestDiskBlockerNamesEveryWayOut(t *testing.T) {
	p := plan(t, baseTable(), "5.6.51", "ADD COLUMN note varchar(64) NULL",
		func(in *Input) {
			in.ForExecution = true
			in.Disk = DiskFacts{Reason: "not on this host", ServerHost: "mysql-prod", Path: "/var/lib/mysql"}
		})
	var found bool
	for _, c := range checksOf(p, "D6") {
		if c.Severity != SeverityBlocker {
			continue
		}
		found = true
		for _, want := range []string{"--free-disk", "docker exec mysql-prod", "--force"} {
			if !strings.Contains(c.Remediation, want) {
				t.Errorf("the blocker must offer %q: %q", want, c.Remediation)
			}
		}
	}
	if !found {
		t.Fatalf("expected a D6 blocker: %+v", checksOf(p, "D6"))
	}
}

// A duration derived from real runs has to say where it came from, and it must
// never claim a measurement it does not have. Both directions are asserted here,
// because the failure that matters is not a wrong number - it is a confident one.
func TestMeasuredDurationReplacesTheAssumption(t *testing.T) {
	tbl := model.Table{Name: "orders", Rows: 10_000_000, DataLength: 40 << 30}
	in := Input{
		Table: tbl,
		Ops:   []model.Op{{Kind: model.OpAddIndex, Parts: []model.IndexPart{{Column: "spot_id"}}}},
		// Two runs of a much smaller table: 200 MB in 20s (10 MB/s) and 200 MB in
		// 5s (40 MB/s). Throughput is what transfers, not the size.
		History: []Observation{
			{Engine: EnginePtOSC, DataBytes: 200 << 20, Seconds: 20},
			{Engine: EnginePtOSC, DataBytes: 200 << 20, Seconds: 5},
		},
	}
	est := estimateDuration(in, tbl, EnginePtOSC)

	if est.Observations != 2 {
		t.Fatalf("observations = %d, want 2", est.Observations)
	}
	if est.Confidence != "medium" {
		t.Fatalf("confidence = %q, want medium for two measured runs", est.Confidence)
	}
	// 40 GB at 40 MB/s is ~1024s; at 10 MB/s it is ~4096s.
	if est.FromSeconds != 1024 || est.ToSeconds != 4096 {
		t.Fatalf("range = %ds-%ds, want 1024s-4096s", est.FromSeconds, est.ToSeconds)
	}
	if !strings.Contains(est.Basis, "measured on this machine") {
		t.Fatalf("the basis must say it was measured: %q", est.Basis)
	}
	if !strings.Contains(est.Basis, "2 earlier pt-osc run") {
		t.Fatalf("the basis must name the sample size and engine: %q", est.Basis)
	}
	if strings.Contains(est.Basis, "assumed") {
		t.Fatalf("a measured estimate must not also claim an assumption: %q", est.Basis)
	}
}

// One run is a point, not a range: the estimate stays an assumption and the single
// measurement is mentioned as exactly that.
func TestOneObservationIsNotARange(t *testing.T) {
	tbl := model.Table{Name: "orders", Rows: 1_000_000, DataLength: 20 << 30}
	in := Input{
		Table:   tbl,
		Ops:     []model.Op{{Kind: model.OpAddIndex, Parts: []model.IndexPart{{Column: "id"}}}},
		History: []Observation{{Engine: EngineGhOst, DataBytes: 100 << 20, Seconds: 10}},
	}
	est := estimateDuration(in, tbl, EngineGhOst)

	// The count reports what exists; the confidence reports how far it is trusted.
	// One run exists, and it is not enough for a range.
	if est.Observations != 1 {
		t.Fatalf("observations = %d, want 1: one run exists even though it is not a range", est.Observations)
	}
	if est.Confidence == "high" {
		t.Fatalf("confidence = %q; a single run cannot be trusted that far", est.Confidence)
	}
	if !strings.Contains(est.Basis, "assumed") {
		t.Fatalf("the range must still be described as an assumption: %q", est.Basis)
	}
	if !strings.Contains(est.Basis, "measured once") {
		t.Fatalf("the single measurement must be mentioned: %q", est.Basis)
	}
}

// History collected for one engine must not be applied to another: gh-ost and
// pt-osc move data at very different speeds, and mixing them would be worse than
// having no history at all.
func TestHistoryIsPerEngine(t *testing.T) {
	tbl := model.Table{Name: "orders", Rows: 1_000_000, DataLength: 20 << 30}
	in := Input{
		Table: tbl,
		Ops:   []model.Op{{Kind: model.OpAddIndex, Parts: []model.IndexPart{{Column: "id"}}}},
		History: []Observation{
			{Engine: EngineGhOst, DataBytes: 200 << 20, Seconds: 20},
			{Engine: EngineGhOst, DataBytes: 200 << 20, Seconds: 5},
		},
	}
	est := estimateDuration(in, tbl, EnginePtOSC)
	if est.Observations != 0 {
		t.Fatalf("gh-ost history leaked into a pt-osc estimate (observations = %d)", est.Observations)
	}
	if !strings.Contains(est.Basis, "assumed") {
		t.Fatalf("without its own history the estimate is an assumption: %q", est.Basis)
	}
}

// INSTANT stays INSTANT: a metadata-only change takes the same moment whether or
// not this machine has copied tables before.
func TestMeasuredHistoryDoesNotOverrideInstant(t *testing.T) {
	tbl := model.Table{Name: "t", Rows: 100, DataLength: 1 << 20}
	in := Input{
		Table:   tbl,
		Ops:     []model.Op{{Kind: model.OpSetDefault, Column: &model.Column{Name: "status", Type: "varchar(32)", DataType: "varchar"}}},
		History: []Observation{{Engine: EngineNative, DataBytes: 200 << 20, Seconds: 40}},
	}
	est := estimateDuration(in, tbl, EngineNative)
	if est.Confidence != "high" || est.ToSeconds > 2 {
		t.Fatalf("INSTANT must keep its own answer: %+v", est)
	}
}

// A wide spread is said out loud: two runs that differ by 8x do not justify a
// precise-looking range.
func TestWideSpreadIsCalledOut(t *testing.T) {
	tbl := model.Table{Name: "orders", Rows: 1_000_000, DataLength: 8 << 30}
	in := Input{
		Table: tbl,
		Ops:   []model.Op{{Kind: model.OpAddIndex, Parts: []model.IndexPart{{Column: "id"}}}},
		History: []Observation{
			{Engine: EngineGhOst, DataBytes: 200 << 20, Seconds: 2},   // 100 MB/s
			{Engine: EngineGhOst, DataBytes: 200 << 20, Seconds: 200}, // 1 MB/s
		},
	}
	est := estimateDuration(in, tbl, EngineGhOst)
	if !strings.Contains(est.Basis, "spread is wide") {
		t.Fatalf("an 8x spread must be named: %q", est.Basis)
	}
}

// Zero and negative observations are not measurements. A state directory can hold
// a session written by an older format, and the estimator must skip it rather than
// divide by zero and print an infinity.
func TestUnusableObservationsAreSkipped(t *testing.T) {
	tbl := model.Table{Name: "orders", Rows: 1_000_000, DataLength: 4 << 30}
	in := Input{
		Table: tbl,
		Ops:   []model.Op{{Kind: model.OpAddIndex, Parts: []model.IndexPart{{Column: "id"}}}},
		History: []Observation{
			{Engine: EnginePtOSC, DataBytes: 0, Seconds: 10},
			{Engine: EnginePtOSC, DataBytes: 100 << 20, Seconds: 0},
			{Engine: EnginePtOSC, DataBytes: 100 << 20, Seconds: 10},
		},
	}
	est := estimateDuration(in, tbl, EnginePtOSC)
	if est.FromSeconds <= 0 || est.ToSeconds <= 0 {
		t.Fatalf("unusable observations produced a broken range: %+v", est)
	}
	if est.Observations != 1 {
		t.Fatalf("exactly one observation was usable, and the plan must say so: %+v", est)
	}
	if est.Confidence != "low" {
		t.Fatalf("one usable run must not raise the confidence: %+v", est)
	}
}

// --- the four checks that landed in v0.7.2 ---------------------------------

// C7: a nullable foreign key column is where ON DELETE SET NULL does its work,
// and no constraint stops a NULL from meaning nothing.
func TestNullableForeignKeyColumnIsReported(t *testing.T) {
	tbl := withFK(baseTable())
	for i, c := range tbl.Columns {
		if c.Name == "spot_id" {
			tbl.Columns[i].Nullable = true
		}
	}
	tbl.ForeignKeys[0].OnDelete = model.ActionSetNull

	p := plan(t, tbl, "8.0.36", "ADD INDEX ix_created (created_at)")
	if !hasCheck(p, "C7", SeverityInfo) {
		t.Fatalf("a nullable FK column must be reported: %+v", checksOf(p, "C7"))
	}
	var detail string
	for _, c := range checksOf(p, "C7") {
		detail += c.Detail + " "
	}
	if !strings.Contains(detail, "ON DELETE SET NULL") {
		t.Fatalf("the detail must connect NULL to the action: %q", detail)
	}

	// A NOT NULL column is the ordinary case and must stay quiet.
	if got := checksOf(plan(t, withFK(baseTable()), "8.0.36", "ADD INDEX ix_created (created_at)"), "C7"); len(got) != 0 {
		t.Fatalf("a NOT NULL FK column is not a finding: %+v", got)
	}
}

// D11: generated columns survive a copy only if the expression survives it.
func TestGeneratedColumnIsReported(t *testing.T) {
	tbl := baseTable()
	tbl.Columns = append(tbl.Columns, model.Column{
		Name: "status_upper", Type: "varchar(32)", DataType: "varchar",
		Generated: true, Extra: "GENERATED ALWAYS AS (upper(`status`)) STORED",
	})
	p := plan(t, tbl, "8.0.36", "ADD INDEX ix_created (created_at)")
	if !hasCheck(p, "D11", SeverityInfo) {
		t.Fatalf("a generated column must be reported: %+v", checksOf(p, "D11"))
	}
	if !strings.Contains(checksOf(p, "D11")[0].Detail, "status_upper") {
		t.Fatalf("the check must name the column: %+v", checksOf(p, "D11"))
	}
}

// E4: Percona Toolkit 2.x has no --resume, so an interrupted copy starts over.
func TestOldPerconaToolkitIsReported(t *testing.T) {
	withPtOSC := func(version string) func(*Input) {
		return func(in *Input) {
			in.Binaries = map[string]EngineBinary{
				"pt-online-schema-change": {Name: "pt-online-schema-change", Path: "/usr/bin/pt-online-schema-change", Version: version, Available: true},
			}
		}
	}
	p := plan(t, baseTable(), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL", withPtOSC("2.2.20"))
	if !hasCheck(p, "E4", SeverityWarn) {
		t.Fatalf("2.x must be reported: %+v", checksOf(p, "E4"))
	}
	// 3.x is what this build verifies against; reporting it would be noise.
	if got := checksOf(plan(t, baseTable(), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL", withPtOSC("3.7.2")), "E4"); len(got) != 0 {
		t.Fatalf("3.x is not a finding: %+v", got)
	}
}

// D13: an in-place rebuild collects concurrent DML in the online alter log, and a
// full log means 1799 and a rollback of everything done so far.
func TestSmallOnlineAlterLogIsReportedForALargeRebuild(t *testing.T) {
	withLog := func(value string) func(*Input) {
		return func(in *Input) {
			in.Settings = SettingsFromMap(map[string]string{
				"performance_schema":               "ON",
				"innodb_online_alter_log_max_size": value,
			})
		}
	}
	// An index build on the 22 GiB fixture is an in-place rebuild.
	p := plan(t, baseTable(), "8.0.36", "ADD INDEX ix_status (status)", withLog("134217728"))
	if !hasCheck(p, "D13", SeverityWarn) {
		t.Fatalf("a 128 MB log on a 22 GiB rebuild must be reported: %+v", checksOf(p, "D13"))
	}
	if got := checksOf(p, "D13")[0]; got.MySQLError != 1799 {
		t.Fatalf("the check must name the error it predicts: %+v", got)
	}

	// A raised log is the fix, so the finding must disappear.
	if got := checksOf(plan(t, baseTable(), "8.0.36", "ADD INDEX ix_status (status)", withLog("1073741824")), "D13"); len(got) != 0 {
		t.Fatalf("a 1 GiB log is enough for this rebuild: %+v", got)
	}

	// INSTANT touches no rows, so there is nothing to accumulate.
	instant := plan(t, baseTable(), "8.0.36", "ADD COLUMN flag tinyint NOT NULL DEFAULT 0", withLog("134217728"))
	if got := checksOf(instant, "D13"); len(got) != 0 {
		t.Fatalf("INSTANT must not be warned about the online alter log: %+v", got)
	}

	// A copy engine keeps its own table and never uses this log.
	copyEngine := plan(t, baseTable(), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL", withLog("134217728"))
	if copyEngine.Recommendation.Engine == EngineNative {
		t.Fatalf("fixture was expected to route to a copy engine, got %q", copyEngine.Recommendation.Engine)
	}
	if got := checksOf(copyEngine, "D13"); len(got) != 0 {
		t.Fatalf("a copy engine does not need the online alter log: %+v", got)
	}

	// An unreadable setting is reported as unknown, never as a pass - but only on
	// a run where it would have mattered.
	unknown := plan(t, baseTable(), "8.0.36", "ADD INDEX ix_status (status)",
		func(in *Input) {
			in.Settings = SettingsFromMap(map[string]string{"performance_schema": "ON"})
		})
	if !hasCheck(unknown, "D13", SeverityInfo) {
		t.Fatalf("an unreadable setting must be named on a large rebuild: %+v", checksOf(unknown, "D13"))
	}

	// A small table cannot overflow a 128 MB log in the time it takes to rebuild.
	small := baseTable()
	small.DataLength, small.IndexLength, small.Rows = 2<<30, 0, 500_000
	if got := checksOf(plan(t, small, "8.0.36", "ADD INDEX ix_status (status)", withLog("134217728")), "D13"); len(got) != 0 {
		t.Fatalf("a 2 GiB table is not worth this warning: %+v", got)
	}
}
