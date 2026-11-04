package parser

import (
	"testing"

	"github.com/perogeremmer/saksama-core/internal/model"
)

const ordersDDL = "CREATE TABLE `orders` (\n" +
	"  `id` bigint unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `spot_id` bigint unsigned NOT NULL,\n" +
	"  `status` varchar(32) COLLATE utf8mb4_0900_ai_ci NOT NULL DEFAULT 'pending',\n" +
	"  `amount` decimal(18,2) NOT NULL DEFAULT '0.00',\n" +
	"  `note` text COLLATE utf8mb4_0900_ai_ci,\n" +
	"  PRIMARY KEY (`id`),\n" +
	"  UNIQUE KEY `uk_status` (`status`),\n" +
	"  KEY `ix_spot` (`spot_id`),\n" +
	"  CONSTRAINT `fk_orders_spot` FOREIGN KEY (`spot_id`) REFERENCES `spots` (`id`) ON DELETE CASCADE ON UPDATE SET NULL\n" +
	") ENGINE=InnoDB AUTO_INCREMENT=59000001 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC"

func TestParseCreateTable(t *testing.T) {
	tbl, ignored, err := ParseCreateTable(ordersDDL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ignored) != 0 {
		t.Fatalf("unexpected ignored fragments: %v", ignored)
	}
	if tbl.Name != "orders" {
		t.Fatalf("got name %q", tbl.Name)
	}
	if tbl.Engine != "InnoDB" {
		t.Fatalf("got engine %q", tbl.Engine)
	}
	if tbl.RowFormat != "DYNAMIC" {
		t.Fatalf("got row format %q", tbl.RowFormat)
	}
	if tbl.Charset != "utf8mb4" || tbl.Collation != "utf8mb4_0900_ai_ci" {
		t.Fatalf("got charset %q collation %q", tbl.Charset, tbl.Collation)
	}
	if len(tbl.Columns) != 5 {
		t.Fatalf("got %d columns: %+v", len(tbl.Columns), tbl.Columns)
	}
	if len(tbl.Indexes) != 3 {
		t.Fatalf("got %d indexes: %+v", len(tbl.Indexes), tbl.Indexes)
	}

	pk, ok := tbl.PrimaryKey()
	if !ok {
		t.Fatal("no primary key parsed")
	}
	if len(pk.Parts) != 1 || pk.Parts[0].Column != "id" {
		t.Fatalf("bad pk: %+v", pk)
	}

	if len(tbl.ForeignKeys) != 1 {
		t.Fatalf("got %d FKs", len(tbl.ForeignKeys))
	}
	fk := tbl.ForeignKeys[0]
	if fk.Name != "fk_orders_spot" || fk.RefTable != "spots" {
		t.Fatalf("bad fk: %+v", fk)
	}
	// The whole point: referential actions must survive parsing.
	if fk.OnDelete != model.ActionCascade {
		t.Fatalf("ON DELETE lost: %q", fk.OnDelete)
	}
	if fk.OnUpdate != model.ActionSetNull {
		t.Fatalf("ON UPDATE lost: %q", fk.OnUpdate)
	}

	// Column charset inheritance.
	col, ok := tbl.Column("status")
	if !ok {
		t.Fatal("status column missing")
	}
	if col.Charset != "utf8mb4" {
		t.Fatalf("got column charset %q", col.Charset)
	}
	if col.Nullable {
		t.Fatal("status should be NOT NULL")
	}
	if col.Default == nil || *col.Default != "pending" {
		t.Fatalf("bad default: %v", col.Default)
	}
}

func TestParseCreateTableNoPK(t *testing.T) {
	ddl := "CREATE TABLE `logs` (`msg` text, `at` datetime) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"
	tbl, _, err := ParseCreateTable(ddl)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := tbl.PrimaryKey(); ok {
		t.Fatal("expected no primary key")
	}
	if tbl.HasUniqueKey() {
		t.Fatal("expected no unique key")
	}
}

func TestParseCreateTableCompositePK(t *testing.T) {
	ddl := "CREATE TABLE `m` (`a` int NOT NULL, `b` int NOT NULL, PRIMARY KEY (`a`,`b`)) ENGINE=InnoDB"
	tbl, _, err := ParseCreateTable(ddl)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pk, ok := tbl.PrimaryKey()
	if !ok || len(pk.Parts) != 2 {
		t.Fatalf("bad pk: %+v", pk)
	}
}

func TestParseCreateTableQuotedDefaultsAndComments(t *testing.T) {
	ddl := "CREATE TABLE `t` (\n" +
		"  `a` varchar(10) NOT NULL DEFAULT 'x,y' COMMENT 'has, comma',\n" +
		"  `b` int NOT NULL DEFAULT 0\n" +
		") ENGINE=InnoDB"
	tbl, _, err := ParseCreateTable(ddl)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tbl.Columns) != 2 {
		t.Fatalf("got %d columns", len(tbl.Columns))
	}
	if tbl.Columns[0].Default == nil || *tbl.Columns[0].Default != "x,y" {
		t.Fatalf("bad default: %v", tbl.Columns[0].Default)
	}
}

func TestParseCreateTableRejectsGarbage(t *testing.T) {
	if _, _, err := ParseCreateTable("SELECT 1"); err == nil {
		t.Fatal("expected error for non-CREATE statement")
	}
}
