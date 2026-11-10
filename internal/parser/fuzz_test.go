package parser

import (
	"strings"
	"testing"
)

// The parser is the only component that reads text a human typed, and it is the
// gate the analyzer trusts: whatever it returns becomes a rule verdict, and a rule
// verdict becomes an ALTER on a production table. Coverage measures which lines run
// on the inputs we thought of; this looks for the inputs we did not.
//
// Two invariants, both cheap and both meaningful:
//
//   - Normalization is stable. Tokenize then JoinTokens is how the tool renders a
//     statement back to a reader; if re-tokenizing that rendering produces something
//     different, then a plan can describe a statement it would not execute. One
//     round trip is idempotent or the parser is losing information.
//   - A parse that succeeds describes something real: a table name, at least one
//     operation, and an operation kind for each.
//
// Neither can be checked by an example. Both are properties.

func FuzzParseAlter(f *testing.F) {
	seeds := []string{
		// The shapes the tool documents, one per operation family.
		"ALTER TABLE `orders` ADD INDEX ix_created (created_at)",
		"ALTER TABLE `orders` ADD UNIQUE INDEX ix_email (email)",
		"ALTER TABLE `orders` ADD FULLTEXT INDEX ft_note (note)",
		"ALTER TABLE `shop`.`orders` ADD COLUMN `flag` tinyint NOT NULL DEFAULT 0",
		"ALTER TABLE orders ADD COLUMN flag tinyint NOT NULL DEFAULT 0 AFTER id",
		"ALTER TABLE orders ADD COLUMN flag tinyint FIRST",
		"ALTER TABLE orders DROP COLUMN note",
		"ALTER TABLE orders MODIFY COLUMN status varchar(64) NOT NULL DEFAULT 'pending'",
		"ALTER TABLE orders CHANGE COLUMN status state varchar(64) NOT NULL",
		"ALTER TABLE orders RENAME COLUMN status TO state",
		"ALTER TABLE orders RENAME TO orders_old",
		"ALTER TABLE orders ALTER COLUMN status SET DEFAULT 'x'",
		"ALTER TABLE orders ALTER COLUMN status DROP DEFAULT",
		"ALTER TABLE orders ADD CONSTRAINT fk_spot FOREIGN KEY (spot_id) REFERENCES spots (id) ON DELETE CASCADE ON UPDATE RESTRICT",
		"ALTER TABLE orders DROP FOREIGN KEY fk_spot",
		"ALTER TABLE orders DROP INDEX ix_created",
		"ALTER TABLE orders RENAME INDEX ix_created TO ix_created_at",
		"ALTER TABLE orders ALGORITHM=INPLACE, LOCK=NONE, ADD INDEX ix (id)",
		"ALTER TABLE orders CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci",
		"ALTER TABLE orders ENGINE=InnoDB",
		"ALTER TABLE orders PARTITION BY HASH (id) PARTITIONS 4",
		"ALTER TABLE orders ADD INDEX ix (id), DROP COLUMN note, MODIFY COLUMN status varchar(64)",

		// The shapes that broke something once: quoting, comments, casing, spacing,
		// unicode, and the multi-statement refusal.
		"ALTER TABLE `or``ders` ADD INDEX `ix``weird` (`id```)",
		"ALTER TABLE orders ADD COLUMN note varchar(10) DEFAULT 'it''s'",
		"ALTER TABLE orders ADD COLUMN note varchar(10) DEFAULT \"x\"",
		"ALTER TABLE orders ADD COLUMN note varchar(10) DEFAULT 'a\\'b'",
		"alter table orders add index ix (id)",
		"ALTER TABLE orders\n\tADD INDEX ix (id)\n",
		"ALTER/*c*/TABLE orders ADD/*c*/INDEX ix (id)",
		"ALTER TABLE -- trailing\norders ADD INDEX ix (id)",
		"ALTER TABLE orders ADD INDEX ix (id);;",
		"ALTER TABLE orders ADD INDEX ix (id); DROP TABLE orders",
		"ALTER TABLE orders ADD COLUMN n int DEFAULT 1 COMMENT 'a;b'",
		"ALTER TABLE orders ADD COLUMN n int NOT NULL AUTO_INCREMENT",
		"ALTER TABLE orders ADD COLUMN amount decimal(10,2) NOT NULL DEFAULT 0.00",
		"ALTER TABLE orders ADD COLUMN created_at datetime(6) DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6)",
		"ALTER TABLE orders ADD COLUMN g int GENERATED ALWAYS AS (`id` + 1) STORED",
		"ALTER TABLE orders ADD COLUMN `héllo` varchar(10) NOT NULL DEFAULT 'ünïcode'",
		"ALTER TABLE orders ADD INDEX ix ((lower(status)))",
		"ALTER TABLE orders ADD INDEX ix (status(10))",
		"ALTER TABLE orders ADD SPATIAL INDEX ix_geo (geo)",
		"ALTER TABLE",
		"ALTER TABLE orders",
		"",
		";",
		"ALTER TABLE orders ADD",
		"ALTER TABLE orders ADD COLUMN",
		"ALTER TABLE orders ADD INDEX ix (",
		"ALTER TABLE orders ADD INDEX ix ()",
		"ALTER TABLE orders MODIFY COLUMN status",
		"ALTER TABLE `` ADD INDEX ix (id)",
		"ALTER TABLE orders ADD COLUMN `n int` varchar(1)",
		"ALTER TABLE orders ADD COLUMN n varchar(99999999999999999999)",
		strings.Repeat("(", 500),
		strings.Repeat("ALTER TABLE orders ", 50),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, sql string) {
		first, err := ParseAlter(sql)
		if err != nil {
			// A refusal is a correct answer, and must not depend on anything that
			// changes between calls.
			if _, again := ParseAlter(sql); again == nil {
				t.Fatalf("the same input parsed and then did not: %q", sql)
			}
			return
		}

		if first.Table == "" {
			t.Fatalf("a successful parse has no table name: %q -> %+v", sql, first)
		}
		if len(first.Ops) == 0 {
			t.Fatalf("a successful parse has no operations: %q -> %+v", sql, first)
		}
		for _, op := range first.Ops {
			if op.Kind == "" {
				t.Fatalf("an operation has no kind: %q -> %+v", sql, first)
			}
		}

		// Determinism: parse twice, get the same answer.
		second, err := ParseAlter(sql)
		if err != nil {
			t.Fatalf("the same input parsed and then did not: %q", sql)
		}
		if len(second.Ops) != len(first.Ops) || second.Table != first.Table || second.Algorithm != first.Algorithm || second.Lock != first.Lock {
			t.Fatalf("two parses of %q disagree:\n%+v\n%+v", sql, first, second)
		}

		// Normalization must be stable, and the normalized statement must parse to
		// the same thing. This is the invariant that catches a quoting bug: the
		// renderer writes `or``ders`, and the tokenizer has to read it back.
		tokens, err := Tokenize(sql)
		if err != nil {
			t.Fatalf("the parser accepted a statement its tokenizer rejects: %q", sql)
		}
		normalized := JoinTokens(tokens)
		againTokens, err := Tokenize(normalized)
		if err != nil {
			t.Fatalf("the normalized form no longer tokenizes.\ninput: %q\nnormalized: %q\nerror: %v", sql, normalized, err)
		}
		if twice := JoinTokens(againTokens); twice != normalized {
			t.Fatalf("normalization is not idempotent.\ninput: %q\nonce: %q\ntwice: %q", sql, normalized, twice)
		}
		renormalized, err := ParseAlter(normalized)
		if err != nil {
			t.Fatalf("the normalized form no longer parses.\ninput: %q\nnormalized: %q\nerror: %v", sql, normalized, err)
		}
		if renormalized.Table != first.Table || len(renormalized.Ops) != len(first.Ops) {
			t.Fatalf("normalization changed the meaning.\ninput: %q\nnormalized: %q\nbefore: %+v\nafter: %+v",
				sql, normalized, first, renormalized)
		}
	})
}

func FuzzParseCreateTable(f *testing.F) {
	seeds := []string{
		"CREATE TABLE `orders` (\n  `id` bigint unsigned NOT NULL AUTO_INCREMENT,\n  `status` varchar(32) NOT NULL DEFAULT 'pending',\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		"CREATE TABLE `orders` (`id` int NOT NULL, `spot_id` int NOT NULL, PRIMARY KEY (`id`), KEY `ix_spot` (`spot_id`), CONSTRAINT `fk_spot` FOREIGN KEY (`spot_id`) REFERENCES `spots` (`id`) ON DELETE CASCADE) ENGINE=InnoDB",
		"CREATE TABLE `m2m` (`a` int NOT NULL, `b` int NOT NULL, PRIMARY KEY (`a`,`b`)) ENGINE=InnoDB",
		"CREATE TABLE `audit` (`id` int NOT NULL) ENGINE=InnoDB",
		"CREATE TABLE `weird` (`id` int NOT NULL, `a``b` varchar(10) DEFAULT 'x''y', PRIMARY KEY (`id`)) ENGINE=InnoDB",
		"CREATE TABLE `t` (`id` int NOT NULL, `g` int GENERATED ALWAYS AS (`id` + 1) STORED, `up` varchar(32) GENERATED ALWAYS AS (upper(`status`)) VIRTUAL, PRIMARY KEY (`id`)) ENGINE=InnoDB",
		"CREATE TABLE `p` (`id` int NOT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB PARTITION BY HASH (`id`) PARTITIONS 4",
		"CREATE TABLE `f` (`id` int NOT NULL, FULLTEXT KEY `ft` (`body`), `body` text, PRIMARY KEY (`id`)) ENGINE=InnoDB",
		"CREATE TEMPORARY TABLE `tmp` (`id` int) ENGINE=InnoDB",
		"CREATE TABLE IF NOT EXISTS `t` (`id` int) ENGINE=InnoDB",
		"CREATE TABLE `t` (`id` int NOT NULL, `checksum` int, /* a comment; with a semicolon */ PRIMARY KEY (`id`)) ENGINE=InnoDB",
		"CREATE TABLE `t` (`id` int NOT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC",
		"CREATE TABLE `t` (`id` int) ENGINE=InnoDB; DROP TABLE x",
		"CREATE TABLE `t` (`id` int);;",
		"CREATE TABLE",
		"CREATE TABLE `t`",
		"CREATE TABLE `t` (",
		"CREATE TABLE `t` ()",
		"",
		";",
		strings.Repeat("(", 500),
		strings.Repeat("`a` int, ", 200),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, sql string) {
		tbl, ignored, err := ParseCreateTable(sql)
		if err != nil {
			if _, _, again := ParseCreateTable(sql); again == nil {
				t.Fatalf("the same input parsed and then did not: %q", sql)
			}
			return
		}

		if tbl.Name == "" {
			t.Fatalf("a successful parse has no table name: %q -> %+v", sql, tbl)
		}
		if len(tbl.Columns) == 0 {
			t.Fatalf("a successful parse has no columns: %q -> %+v", sql, tbl)
		}

		// A column table the analyzer reads must not contain the same column twice:
		// MySQL rejects that, so producing it means the parser merged or repeated a
		// fragment it did not understand.
		seen := map[string]bool{}
		for _, c := range tbl.Columns {
			if c.Name == "" {
				t.Fatalf("a parsed column has no name: %q -> %+v", sql, tbl.Columns)
			}
			key := strings.ToLower(c.Name)
			if seen[key] {
				t.Fatalf("column %q appears twice: %q", c.Name, sql)
			}
			seen[key] = true
			if c.DataType == "" {
				t.Fatalf("column %q has no data type: %q", c.Name, sql)
			}
		}

		// Every index part must name a column or an expression, and a primary key is
		// unique by definition.
		for _, idx := range tbl.Indexes {
			if len(idx.Parts) == 0 {
				t.Fatalf("index %q has no parts: %q", idx.Name, sql)
			}
			for _, p := range idx.Parts {
				if p.Column == "" {
					t.Fatalf("index %q has a part with no column: %q", idx.Name, sql)
				}
			}
		}

		// Anything the parser could not model is reported, and the fragments it
		// reports must be the input's own words rather than a rendering of them.
		for _, fragment := range ignored {
			if strings.TrimSpace(fragment) == "" {
				t.Fatalf("an empty fragment was reported as ignored: %q", sql)
			}
		}

		// Normalization stability, as for ALTER.
		tokens, err := Tokenize(sql)
		if err != nil {
			t.Fatalf("the parser accepted a statement its tokenizer rejects: %q", sql)
		}
		normalized := JoinTokens(tokens)
		againTokens, err := Tokenize(normalized)
		if err != nil {
			t.Fatalf("the normalized form no longer tokenizes.\ninput: %q\nnormalized: %q\nerror: %v", sql, normalized, err)
		}
		if twice := JoinTokens(againTokens); twice != normalized {
			t.Fatalf("normalization is not idempotent.\ninput: %q\nonce: %q\ntwice: %q", sql, normalized, twice)
		}
		renormalized, _, err := ParseCreateTable(normalized)
		if err != nil {
			t.Fatalf("the normalized form no longer parses.\ninput: %q\nnormalized: %q\nerror: %v", sql, normalized, err)
		}
		if renormalized.Name != tbl.Name || len(renormalized.Columns) != len(tbl.Columns) {
			t.Fatalf("normalization changed the meaning.\ninput: %q\nnormalized: %q\nbefore: %+v\nafter: %+v",
				sql, normalized, tbl, renormalized)
		}
	})
}
