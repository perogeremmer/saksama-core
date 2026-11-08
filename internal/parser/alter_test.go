package parser

import (
	"testing"

	"github.com/perogeremmer/saksama-core/internal/model"
	"strings"
)

func TestParseAlterAddIndex(t *testing.T) {
	a, err := ParseAlter("ALTER TABLE `shop`.`orders` ADD INDEX `ix_created` (`created_at`, `status`(10))")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Schema != "shop" || a.Table != "orders" {
		t.Fatalf("got schema=%q table=%q", a.Schema, a.Table)
	}
	if len(a.Ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(a.Ops))
	}
	op := a.Ops[0]
	if op.Kind != model.OpAddIndex {
		t.Fatalf("got kind %q", op.Kind)
	}
	if op.IndexName != "ix_created" {
		t.Fatalf("got index name %q", op.IndexName)
	}
	if len(op.Parts) != 2 {
		t.Fatalf("got %d parts", len(op.Parts))
	}
	if op.Parts[0].Column != "created_at" || op.Parts[0].PrefixLen != 0 {
		t.Fatalf("bad part 0: %+v", op.Parts[0])
	}
	if op.Parts[1].Column != "status" || op.Parts[1].PrefixLen != 10 {
		t.Fatalf("bad part 1: %+v", op.Parts[1])
	}
}

func TestParseAlterMultipleOps(t *testing.T) {
	a, err := ParseAlter("ALTER TABLE t ADD COLUMN a INT NOT NULL DEFAULT 0, DROP COLUMN b, ADD UNIQUE KEY uk_a (a)")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(a.Ops) != 3 {
		t.Fatalf("got %d ops, want 3", len(a.Ops))
	}
	if a.Ops[0].Kind != model.OpAddColumn || a.Ops[0].Column.Name != "a" {
		t.Fatalf("bad op 0: %+v", a.Ops[0])
	}
	if a.Ops[0].Column.Nullable {
		t.Fatalf("expected NOT NULL, got nullable")
	}
	if a.Ops[1].Kind != model.OpDropColumn || a.Ops[1].Column.Name != "b" {
		t.Fatalf("bad op 1: %+v", a.Ops[1])
	}
	if a.Ops[2].Kind != model.OpAddUniqueIndex {
		t.Fatalf("bad op 2: %+v", a.Ops[2])
	}
}

func TestParseAlterColumnPosition(t *testing.T) {
	a, err := ParseAlter("ALTER TABLE t ADD COLUMN c INT AFTER `id`")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	op := a.Ops[0]
	if op.Position == nil || !op.Position.Set || op.Position.After != "id" {
		t.Fatalf("bad position: %+v", op.Position)
	}

	a, err = ParseAlter("ALTER TABLE t ADD COLUMN c INT FIRST")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Ops[0].Position == nil || !a.Ops[0].Position.First {
		t.Fatalf("bad position: %+v", a.Ops[0].Position)
	}
}

func TestParseAlterForeignKeyActionsPreserved(t *testing.T) {
	a, err := ParseAlter("ALTER TABLE t ADD CONSTRAINT `fk_x` FOREIGN KEY (`a`, `b`) REFERENCES `p` (`x`, `y`) ON DELETE CASCADE ON UPDATE SET NULL")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	op := a.Ops[0]
	if op.Kind != model.OpAddForeignKey {
		t.Fatalf("got kind %q", op.Kind)
	}
	fk := op.ForeignKey
	if fk.Name != "fk_x" {
		t.Fatalf("got name %q", fk.Name)
	}
	if fk.OnDelete != model.ActionCascade {
		t.Fatalf("lost ON DELETE: %q", fk.OnDelete)
	}
	if fk.OnUpdate != model.ActionSetNull {
		t.Fatalf("lost ON UPDATE: %q", fk.OnUpdate)
	}
	if len(fk.Columns) != 2 || len(fk.RefColumns) != 2 {
		t.Fatalf("bad columns: %+v", fk)
	}
}

func TestParseAlterOptions(t *testing.T) {
	a, err := ParseAlter("ALTER TABLE t ADD INDEX ix (a), ALGORITHM=INPLACE, LOCK=NONE")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Algorithm != "INPLACE" {
		t.Fatalf("got algorithm %q", a.Algorithm)
	}
	if a.Lock != "NONE" {
		t.Fatalf("got lock %q", a.Lock)
	}
	if len(a.Ops) != 1 {
		t.Fatalf("options leaked into operations: %d", len(a.Ops))
	}
}

func TestParseAlterRenameForms(t *testing.T) {
	a, err := ParseAlter("ALTER TABLE t RENAME COLUMN a TO b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Ops[0].Kind != model.OpRenameColumn || a.Ops[0].OldName != "a" || a.Ops[0].NewName != "b" {
		t.Fatalf("bad rename column: %+v", a.Ops[0])
	}

	a, err = ParseAlter("ALTER TABLE t RENAME TO t2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Ops[0].Kind != model.OpRenameTable || a.Ops[0].NewName != "t2" {
		t.Fatalf("bad rename table: %+v", a.Ops[0])
	}
}

func TestParseAlterUnknownBecomesUnknownOp(t *testing.T) {
	cases := []string{
		"ALTER TABLE t FORCE",
		"ALTER TABLE t ORDER BY a",
		"ALTER TABLE t DROP CHECK c1",
		"ALTER TABLE t PARTITION BY HASH(id) PARTITIONS 4",
	}
	for _, sql := range cases {
		a, err := ParseAlter(sql)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", sql, err)
		}
		if len(a.Ops) != 1 {
			t.Fatalf("%s: got %d ops", sql, len(a.Ops))
		}
		if a.Ops[0].Kind != model.OpUnknown {
			t.Fatalf("%s: expected unknown op, got %q", sql, a.Ops[0].Kind)
		}
	}
}

func TestParseAlterRejectsMultiStatement(t *testing.T) {
	if _, err := ParseAlter("ALTER TABLE t ADD COLUMN a INT; DROP TABLE t"); err == nil {
		t.Fatal("expected multi-statement rejection, got nil")
	}
}

func TestParseAlterAllowsEscapedBacktickInIdentifier(t *testing.T) {
	a, err := ParseAlter("ALTER TABLE `ord``ers` ADD COLUMN a INT")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Table != "ord`ers" {
		t.Fatalf("got table %q, want ord`ers", a.Table)
	}
}

func TestParseAlterRejectsUnterminatedIdentifier(t *testing.T) {
	// A stray backtick must not be silently absorbed into a second statement.
	if _, err := ParseAlter("ALTER TABLE `ord`ers` ADD COLUMN a INT"); err == nil {
		t.Fatal("expected rejection, got nil")
	}
}

func TestParseAlterCommentsIgnored(t *testing.T) {
	a, err := ParseAlter("ALTER TABLE t -- trailing comment\nADD COLUMN a INT /* block */ NOT NULL")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Ops[0].Column.Name != "a" {
		t.Fatalf("got %+v", a.Ops[0].Column)
	}
	if a.Ops[0].Column.Nullable {
		t.Fatal("comment ate the NOT NULL")
	}
}

func TestParseAlterCommaInsideParensNotSplit(t *testing.T) {
	a, err := ParseAlter("ALTER TABLE t ADD COLUMN d DECIMAL(18,2) NOT NULL, ADD COLUMN e ENUM('a','b') NOT NULL")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(a.Ops) != 2 {
		t.Fatalf("got %d ops, want 2", len(a.Ops))
	}
	if a.Ops[0].Column.Type != "decimal(18,2)" {
		t.Fatalf("got type %q", a.Ops[0].Column.Type)
	}
	if a.Ops[1].Column.Type != "enum('a','b')" {
		t.Fatalf("got type %q", a.Ops[1].Column.Type)
	}
}

func TestParseAlterGeneratedColumn(t *testing.T) {
	a, err := ParseAlter("ALTER TABLE t ADD COLUMN total DECIMAL(18,2) GENERATED ALWAYS AS (a+b) STORED")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !a.Ops[0].Column.Generated {
		t.Fatal("expected generated column")
	}
}

func TestParseAlterCharsetForms(t *testing.T) {
	a, err := ParseAlter("ALTER TABLE t CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Ops[0].Kind != model.OpConvertCharset || a.Ops[0].Charset != "utf8mb4" {
		t.Fatalf("bad op: %+v", a.Ops[0])
	}
	if a.Ops[0].Collation != "utf8mb4_0900_ai_ci" {
		t.Fatalf("bad collation: %q", a.Ops[0].Collation)
	}
}

// Whatever the tokenizer decodes must survive the round trip back to SQL: the
// regenerated op.Raw is the text the native engine embeds and the copy engines
// receive as --alter. Re-quoting the decoded value would break both.
func TestRenderedSQLSurvivesEscapes(t *testing.T) {
	cases := []struct{ name, alter, mustContain string }{
		{"doubled quote in default", "ALTER TABLE t MODIFY COLUMN name varchar(64) NOT NULL DEFAULT 'O''Brien'", `'O''Brien'`},
		{"backslash escape in default", "ALTER TABLE t MODIFY COLUMN name varchar(64) NOT NULL DEFAULT 'a\\nb'", `'a\nb'`},
		{"doubled backtick in identifier", "ALTER TABLE t ADD INDEX `a``b` (`id`)", "`a``b`"},
		{"plain default unchanged", "ALTER TABLE t ADD COLUMN note varchar(64) DEFAULT 'hello'", `'hello'`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := ParseAlter(c.alter)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(a.Ops) != 1 {
				t.Fatalf("got %d ops", len(a.Ops))
			}
			if !strings.Contains(a.Ops[0].Raw, c.mustContain) {
				t.Fatalf("regenerated SQL lost the source text:\nwant substring %q in:\n%s", c.mustContain, a.Ops[0].Raw)
			}
			// The broken re-renderings must not appear.
			for _, bad := range []string{"'O'Brien'", "`a`b`"} {
				if strings.Contains(a.Ops[0].Raw, bad) {
					t.Fatalf("regenerated SQL contains broken quoting %q:\n%s", bad, a.Ops[0].Raw)
				}
			}
		})
	}
}
