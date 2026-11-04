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

func TestModifyColumnWithForeignKeysIsBlocked(t *testing.T) {
	p := plan(t, withFK(baseTable()), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL")
	if p.Recommendation.Engine != EngineNone {
		t.Fatalf("got %q, want none", p.Recommendation.Engine)
	}
	if !hasCheck(p, chkFKCopyBlocked, SeverityBlocker) {
		t.Fatalf("expected copy-with-FK blocker: %+v", p.Checks)
	}
}

func TestIncomingForeignKeyAlsoBlocksCopyEngine(t *testing.T) {
	p := plan(t, withIncomingFK(baseTable()), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL")
	if !hasCheck(p, chkFKCopyBlocked, SeverityBlocker) {
		t.Fatalf("expected blocker for incoming FK: %+v", p.Checks)
	}
}

func TestRebuildWithoutPrimaryKeyIsBlocked(t *testing.T) {
	tbl := baseTable()
	tbl.Indexes = []model.Index{{Name: "ix_spot", Parts: []model.IndexPart{{Column: "spot_id"}}}}
	p := plan(t, tbl, "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL")
	if !hasCheck(p, chkCopyEngineNoPK, SeverityBlocker) {
		t.Fatalf("expected no-PK blocker: %+v", p.Checks)
	}
	if p.Recommendation.Engine != EngineNone {
		t.Fatalf("got %q, want none", p.Recommendation.Engine)
	}
}

func TestCompositePrimaryKeyBlocksGhOst(t *testing.T) {
	tbl := baseTable()
	tbl.Indexes[0] = model.Index{Name: "PRIMARY", Primary: true, Unique: true,
		Parts: []model.IndexPart{{Column: "id"}, {Column: "spot_id"}}}
	p := plan(t, tbl, "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL")
	if !hasCheck(p, chkCompositePKNoGhOst, SeverityBlocker) {
		t.Fatalf("expected composite-PK blocker: %+v", p.Checks)
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
	for _, r := range p.Recommendation.Reasons {
		if contains(r, "rds_superuser") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected privilege note, got %v", p.Recommendation.Reasons)
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

func TestAdviseNativeSuitableForAddIndex(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "ADD INDEX ix_created (created_at)")

	native, ok := assessmentFor(p, EngineNative)
	if !ok {
		t.Fatal("native assessment missing")
	}
	if native.Verdict != VerdictSuitable {
		t.Fatalf("native verdict = %q (%v)", native.Verdict, native.BlockedBy)
	}
	if !native.Executable {
		t.Fatal("native must be executable in this build")
	}
	if !p.Recommendation.Executable {
		t.Fatal("recommendation must be executable")
	}
}

func TestAdviseGhOstBlockedByForeignKeys(t *testing.T) {
	p := plan(t, withFK(baseTable()), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL")

	gh, ok := assessmentFor(p, EngineGhOst)
	if !ok {
		t.Fatal("gh-ost assessment missing")
	}
	if gh.Verdict != VerdictBlocked {
		t.Fatalf("gh-ost verdict = %q", gh.Verdict)
	}
	found := false
	for _, b := range gh.BlockedBy {
		if b == "no foreign keys in or out" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the foreign key requirement to be the blocker, got %v", gh.BlockedBy)
	}

	// pt-osc handles foreign keys, so it must NOT be blocked by them.
	pt, ok := assessmentFor(p, EnginePtOSC)
	if !ok {
		t.Fatal("pt-osc assessment missing")
	}
	for _, b := range pt.BlockedBy {
		if b == "no foreign keys in or out" {
			t.Fatal("pt-osc should not be blocked by foreign keys")
		}
	}
}

func TestAdviseGhOstFitsWithoutForeignKeys(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL", func(in *Input) {
		in.Binaries = map[string]EngineBinary{
			"gh-ost": {Name: "gh-ost", Available: true, Path: "/usr/local/bin/gh-ost", Version: "1.1.6"},
		}
	})

	gh, _ := assessmentFor(p, EngineGhOst)
	if gh.Verdict != VerdictNotExecutable {
		t.Fatalf("gh-ost verdict = %q, want not_executable (fits, adapter missing): %v", gh.Verdict, gh.BlockedBy)
	}
	if len(gh.BlockedBy) != 0 {
		t.Fatalf("gh-ost should have no blockers here: %v", gh.BlockedBy)
	}

	// The recommendation must not claim to be executable.
	if p.Recommendation.Engine != EngineGhOst {
		t.Fatalf("recommended = %q", p.Recommendation.Engine)
	}
	if p.Recommendation.Executable {
		t.Fatal("this build must not claim it can run gh-ost")
	}
	if !strings.Contains(p.Recommendation.ExecutableReason, "not implemented") {
		t.Fatalf("reason = %q", p.Recommendation.ExecutableReason)
	}
}

func TestAdviseGhOstBlockedWhenBinaryMissing(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL", func(in *Input) {
		in.Binaries = map[string]EngineBinary{
			"gh-ost": {Name: "gh-ost", Available: false, Error: "not found on PATH"},
		}
	})
	gh, _ := assessmentFor(p, EngineGhOst)
	if gh.Verdict != VerdictBlocked {
		t.Fatalf("verdict = %q, want blocked", gh.Verdict)
	}
	var hasBinaryBlock bool
	for _, b := range gh.BlockedBy {
		if b == "gh-ost binary installed" {
			hasBinaryBlock = true
		}
	}
	if !hasBinaryBlock {
		t.Fatalf("expected the binary requirement: %v", gh.BlockedBy)
	}
}

func TestAdviseCompositePrimaryKeyFavoursPtOSC(t *testing.T) {
	tbl := baseTable()
	tbl.Indexes[0] = model.Index{Name: "PRIMARY", Primary: true, Unique: true,
		Parts: []model.IndexPart{{Column: "id"}, {Column: "spot_id"}}}

	p := plan(t, tbl, "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL")

	gh, _ := assessmentFor(p, EngineGhOst)
	if gh.Verdict != VerdictBlocked {
		t.Fatalf("gh-ost verdict = %q, want blocked", gh.Verdict)
	}
	pt, _ := assessmentFor(p, EnginePtOSC)
	if len(pt.BlockedBy) != 0 {
		t.Fatalf("pt-osc should fit a composite key, blocked by %v", pt.BlockedBy)
	}
	// And it must say why pt-osc is the better fit.
	var mentionsComposite bool
	for _, n := range pt.Notes {
		if strings.Contains(n, "composite primary key") {
			mentionsComposite = true
		}
	}
	if !mentionsComposite {
		t.Fatalf("expected a note about composite keys: %v", pt.Notes)
	}
}

func TestAdvisePtOSCBlockedWithoutKey(t *testing.T) {
	tbl := baseTable()
	tbl.Indexes = nil

	p := plan(t, tbl, "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL")
	pt, _ := assessmentFor(p, EnginePtOSC)
	if pt.Verdict != VerdictBlocked {
		t.Fatalf("pt-osc verdict = %q, want blocked", pt.Verdict)
	}
}

func TestAdviseNativeBlockedWhenFKColumnTouched(t *testing.T) {
	p := plan(t, withFK(baseTable()), "8.0.36", "MODIFY COLUMN spot_id bigint unsigned NOT NULL")
	native, _ := assessmentFor(p, EngineNative)
	if native.Verdict != VerdictBlocked {
		t.Fatalf("native verdict = %q", native.Verdict)
	}
	var hasFkBlock bool
	for _, b := range native.BlockedBy {
		if b == "no foreign-key column touched" {
			hasFkBlock = true
		}
	}
	if !hasFkBlock {
		t.Fatalf("expected the FK-column requirement: %v", native.BlockedBy)
	}
}

func TestAdvisePartitionedTableBlocksGhOst(t *testing.T) {
	tbl := baseTable()
	tbl.Partitioned = true
	p := plan(t, tbl, "8.0.36", "MODIFY COLUMN status varchar(64) NOT NULL")
	gh, _ := assessmentFor(p, EngineGhOst)
	var blocked bool
	for _, b := range gh.BlockedBy {
		if b == "table is not partitioned" {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("partitioned table must block gh-ost: %v", gh.BlockedBy)
	}
}

func TestAdviseEveryEngineIsAssessed(t *testing.T) {
	p := plan(t, baseTable(), "8.0.36", "ADD INDEX ix_created (created_at)")
	if len(p.Engines) != 3 {
		t.Fatalf("got %d assessments, want 3", len(p.Engines))
	}
	for _, a := range p.Engines {
		if a.Summary == "" {
			t.Errorf("%s has no summary", a.Engine)
		}
		if len(a.Requirements) == 0 {
			t.Errorf("%s has no requirements", a.Engine)
		}
	}
}
