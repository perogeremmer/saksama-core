package analyzer

import (
	"fmt"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// Check IDs are stable and documented in docs/CHECKS.md. Never renumber them.
const (
	// Charset & collation.
	chkFKCharsetMismatch  = "A1"
	chkConvertCharsetFK   = "A2"
	chkFKColumnCharset    = "A3"
	chkUtf8mb3            = "A4"
	chkKeyTooLong         = "A5"
	chkCompactRowFormat   = "A6"
	chkBlobIndexNoPrefix  = "A7"
	chkIndexColumnMissing = "A8"

	// Index.
	chkNoPrimaryKey    = "B1"
	chkNoUniqueKey     = "B2"
	chkCompositePK     = "B3"
	chkUUIDPrimaryKey  = "B4"
	chkUniqueNeedsScan = "B5"
	chkPrefixIndex     = "B6"
	chkFKNoIndex       = "B7"
	chkTooManyIndexes  = "B8"

	// Foreign key.
	chkFKColumnTouched     = "C1"
	chkFKReferencedTouched = "C2"
	chkFKActionPreserve    = "C3"
	chkCopyEngineWithFK    = "C4"
	chkManyForeignKeys     = "C5"
	chkAuroraNoFKChecks    = "C6"
	chkFKColumnNullable    = "C7"

	// Operational.
	chkLockWaitTimeout  = "D2"
	chkDiskHeadroom     = "D6"
	chkExistingTrigger  = "D7"
	chkCheckConstraint  = "D9"
	chkGeneratedColumn  = "D11"
	chkProductionTarget = "D12"
	chkOnlineAlterLog   = "D13"
	chkHugeTable        = "D17"

	// Capability.
	chkUnknownOp          = "E0"
	chkMariaDB            = "E1"
	chkEngineNotInnoDB    = "E2"
	chkVersionUnparseable = "E3"
	chkOldPtOSC           = "E4"
	chkOldServer          = "E5"

	// Engine routing.
	chkNoEngineFits           = "R1"
	chkChosenEngineUnsuitable = "R2"
	chkEngineNotInstalled     = "R3"
)

// checksCapability reports operations the analyzer could not classify.
func checksCapability(in Input, ops []model.Op, anyBlocked, anyRebuild bool) []Check {
	var out []Check

	for _, op := range ops {
		switch op.Kind {
		case model.OpUnknown:
			out = append(out, Check{
				ID: chkUnknownOp, Severity: SeverityBlocker,
				Title:       "operation is not understood",
				Detail:      fmt.Sprintf("Saksama could not classify %q", op.Raw),
				Remediation: "rewrite it as operations Saksama models (see `saksama plan --help` for the list), or run this statement yourself with `mysql` and apply the same pre-flight checks by hand - this tool will not guess at a change it cannot classify",
			})
		case model.OpAddForeignKey, model.OpDropForeignKey:
			out = append(out, Check{
				ID: "C8", Severity: SeverityBlocker,
				Title:       "foreign key mutation is out of scope in v1",
				Detail:      fmt.Sprintf("%q changes a foreign key constraint", op.Raw),
				Remediation: "foreign key changes are intentionally blocked; they open an integrity window that v1 cannot make deterministic",
			})
		}
	}

	if in.Server.IsMariaDB() {
		out = append(out, Check{
			ID: chkMariaDB, Severity: SeverityBlocker,
			Title:       "MariaDB is not a verified target",
			Detail:      "DDL capability, engine behaviour, and throttle semantics differ from MySQL",
			Remediation: "point Saksama at a MySQL server instead; MariaDB reports a version string MySQL-shaped but does not implement the same ONLINE DDL rules, so nothing here would be a verified claim. `--force` does not override E1",
		})
	}

	if _, err := model.ParseVersion(in.Server.Version); err != nil {
		out = append(out, Check{
			ID: chkVersionUnparseable, Severity: SeverityBlocker,
			Title:       "server version cannot be parsed",
			Detail:      fmt.Sprintf("version %q did not parse", in.Server.Version),
			Remediation: "provide the server version explicitly with --mysql-version",
		})
	}

	// E5: anything older than 5.6 is refused outright, before the engine matrix
	// is consulted. 5.6 is still verified - plenty of applications have been
	// running since 2015 and nobody is going to upgrade them this quarter - but
	// 5.5 and older predate gh-ost's own support and reached end of life years
	// ago.
	if v, err := model.ParseVersion(in.Server.Version); err == nil {
		if v.Major < 5 || (v.Major == 5 && v.Minor < 6) {
			out = append(out, Check{
				ID:          chkOldServer,
				Severity:    SeverityBlocker,
				Title:       "MySQL older than 5.6 is not a verified target",
				Detail:      fmt.Sprintf("server reports %d.%d.%d; 5.5 reached end of life in 2018 and gh-ost itself requires 5.6 or newer", v.Major, v.Minor, v.Patch),
				Remediation: "upgrade to MySQL 5.6 or newer, where this tool's rules were measured; on anything older run `pt-online-schema-change` or `gh-ost` yourself and treat its own output as the record",
			})
		}

		// E5b: 5.6 is supported, but it cannot answer the question that matters
		// most. performance_schema.metadata_locks arrived in 5.7, so the lock
		// queue is invisible here. Say that in every plan instead of letting the
		// safety check quietly pass.
		if v.Major == 5 && v.Minor == 6 {
			sev := SeverityWarn
			detail := "MySQL 5.6 has no performance_schema.metadata_locks, so a pending lock queue cannot be detected; DDL queues behind open metadata locks and blocks every other query"
			remediation := "check SHOW PROCESSLIST and INFORMATION_SCHEMA.INNODB_TRX by hand immediately before applying, or run this on 5.7 or newer"
			if in.Production && !in.AcceptUnknownMDL {
				sev = SeverityBlocker
				remediation += "; a production run needs --accept-unknown-mdl to say this risk is understood"
			}
			out = append(out, Check{
				ID:          "E5b",
				Severity:    sev,
				Title:       "the metadata lock queue cannot be observed on MySQL 5.6",
				Detail:      detail,
				Remediation: remediation,
			})
		}
	}

	if e := in.Table.Engine; e != "" && !strings.EqualFold(e, "InnoDB") {
		out = append(out, Check{
			ID: chkEngineNotInnoDB, Severity: SeverityBlocker,
			Title:       "table is not InnoDB",
			Detail:      fmt.Sprintf("engine is %q", e),
			Remediation: "convert the table first, in its own window: ALTER TABLE <table> ENGINE=InnoDB; both copy engines and every ONLINE DDL rule assume InnoDB, so neither native nor a copy engine means what this plan says on another storage engine",
		})
	}

	if in.Table.Partitioned {
		out = append(out, Check{
			ID: "D10", Severity: SeverityWarn,
			Title:       "table is partitioned",
			Detail:      "gh-ost does not support partitioned tables",
			Remediation: "prefer a change MySQL can do natively, or run pt-online-schema-change with `saksama engine set pt-osc` so the plan is built for a partitioned table; gh-ost refuses partitioned tables outright",
		})
	}

	return out
}

// checksCharset validates character set and collation hazards.
func checksCharset(in Input, projected model.Table) []Check {
	var out []Check

	// A1: FK column charset/collation compatibility.
	// Offline we can only verify columns that exist in the same table; the
	// parent side requires a live connection, so we say so instead of guessing.
	if len(in.Table.ForeignKeys) > 0 && in.SchemaSource != "live" {
		out = append(out, Check{
			ID: chkFKCharsetMismatch, Severity: SeverityInfo,
			Title:       "foreign key charset compatibility not verified",
			Detail:      "the analyzer only saw this table's schema, so parent-side charset/collation was not compared",
			Remediation: "run saksama with a live connection (or include referenced tables in the schema input) to verify check A1",
		})
	}

	// A2: charset conversion on a table with foreign keys.
	if hasKind(in.Ops, model.OpConvertCharset) && len(in.Table.AllForeignKeys()) > 0 {
		out = append(out, Check{
			ID: chkConvertCharsetFK, Severity: SeverityBlocker,
			Title:       "character set conversion on a table with foreign keys",
			Detail:      fmt.Sprintf("%s has %d foreign key(s) in or out", in.Table.Name, len(in.Table.AllForeignKeys())),
			Remediation: "convert the foreign key columns explicitly, one table at a time: ALTER TABLE <child> MODIFY <col> varchar(32) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci; and the same on the parent; then CONVERT TO CHARACTER SET. Saksama will not do it in one statement, because MySQL refuses mid-flight",
			MySQLError:  1832,
		})
	}

	// A3: FK column charset differs from the table default.
	for _, fk := range in.Table.ForeignKeys {
		for _, colName := range fk.Columns {
			col, ok := in.Table.Column(colName)
			if !ok || !col.IsTextual() || col.Charset == "" || in.Table.Charset == "" {
				continue
			}
			if !strings.EqualFold(col.Charset, in.Table.Charset) {
				out = append(out, Check{
					ID: chkFKColumnCharset, Severity: SeverityWarn,
					Title:       "foreign key column uses a different character set than the table",
					Detail:      fmt.Sprintf("%s.%s is %s but the table is %s", in.Table.Name, colName, col.Charset, in.Table.Charset),
					Remediation: "align the two columns before anything recreates the constraint, in this order: ALTER TABLE <child> MODIFY <col> <type> CHARACTER SET <parent_charset> COLLATE <parent_collation>; then plan again. Both sides are named in the detail above",
					MySQLError:  3780,
				})
			}
		}
	}

	// A4: deprecated utf8/utf8mb3 columns.
	var mb3 []string
	for _, c := range projected.Columns {
		if strings.EqualFold(c.Charset, "utf8") || strings.EqualFold(c.Charset, "utf8mb3") {
			mb3 = append(mb3, c.Name)
		}
	}
	if len(mb3) > 0 {
		out = append(out, Check{
			ID: chkUtf8mb3, Severity: SeverityWarn,
			Title:       "deprecated utf8mb3 columns",
			Detail:      fmt.Sprintf("%d column(s) use utf8mb3: %s", len(mb3), strings.Join(mb3, ", ")),
			Remediation: "plan the conversion as its own change: ALTER TABLE <table> CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci; and note it rebuilds the table, so it belongs with the copy engines; check every index stays under the key limit first",
		})
	}

	// A5/A7/A6 are evaluated per index in checksIndexes.
	return out
}

// checksIndexes validates index shape against the projected schema.
func checksIndexes(in Input, projected model.Table) []Check {
	var out []Check

	limit := keyLimitBytes(projected.RowFormat)

	for _, ix := range projected.Indexes {
		// A8: a column that is not there at all. MySQL answers this with 1072,
		// and saying "key too long" instead sends the operator to count bytes on
		// a column that does not exist.
		var missing []string
		for _, p := range ix.Parts {
			if _, ok := projected.Column(p.Column); !ok {
				missing = append(missing, p.Column)
			}
		}
		if len(missing) > 0 {
			out = append(out, Check{
				ID: chkIndexColumnMissing, Severity: SeverityBlocker,
				Title:       "index refers to a column the table does not have",
				Detail:      fmt.Sprintf("index %q uses %s; the table has no such column", ix.Name, strings.Join(missing, ", ")),
				Remediation: "use a column name that exists in this table (see `saksama advise -t <table>` for the list); MySQL would reject this ALTER with error 1072",
				MySQLError:  1072,
			})
			continue
		}

		// A7: TEXT/BLOB indexed without a prefix length.
		for _, p := range ix.Parts {
			col, ok := projected.Column(p.Column)
			if !ok || !col.IsBlobLike() {
				continue
			}
			if p.PrefixLen == 0 && !ix.Fulltext {
				out = append(out, Check{
					ID: chkBlobIndexNoPrefix, Severity: SeverityBlocker,
					Title:       "TEXT/BLOB column indexed without a prefix length",
					Detail:      fmt.Sprintf("index %q uses %s (%s) without a prefix length", ix.Name, col.Name, col.DataType),
					Remediation: fmt.Sprintf("add a prefix length, e.g. ADD INDEX %s (%s(191))", ix.Name, col.Name),
					MySQLError:  1170,
				})
			}
		}

		// A5: total key length over the InnoDB limit.
		if bytes := indexKeyBytes(ix, projected); bytes > limit {
			out = append(out, Check{
				ID: chkKeyTooLong, Severity: SeverityBlocker,
				Title:       "index key exceeds the InnoDB limit",
				Detail:      fmt.Sprintf("index %q is %d bytes; limit is %d bytes for row format %s", ix.Name, bytes, limit, rowFormatOr(projected.RowFormat)),
				Remediation: "add a prefix length or index fewer columns: `ALTER TABLE <table> ADD INDEX ix (col(191))`. The InnoDB limit is 3072 bytes with ROW_FORMAT=DYNAMIC, and a utf8mb4 varchar counts 4 bytes per character, so col(191) is the usual ceiling",
				MySQLError:  1071,
			})
		}

		// B6: prefix index information.
		for _, p := range ix.Parts {
			if p.PrefixLen > 0 {
				out = append(out, Check{
					ID: chkPrefixIndex, Severity: SeverityInfo,
					Title:       "prefix index",
					Detail:      fmt.Sprintf("index %q indexes only the first %d characters of %s", ix.Name, p.PrefixLen, p.Column),
					Remediation: "nothing to fix before applying: remember that a query benefits only when it filters on that same prefix, and the index cannot satisfy ORDER BY on the full column",
				})
			}
		}
	}

	// A6: narrow row format means a 767-byte limit.
	if strings.EqualFold(projected.RowFormat, "COMPACT") || strings.EqualFold(projected.RowFormat, "REDUNDANT") {
		out = append(out, Check{
			ID: chkCompactRowFormat, Severity: SeverityWarn,
			Title:       "legacy row format limits index keys to 767 bytes",
			Detail:      fmt.Sprintf("row format is %s", projected.RowFormat),
			Remediation: "either keep every key under 767 bytes, or rebuild the table with a modern row format in its own window: ALTER TABLE <table> ROW_FORMAT=DYNAMIC; that rebuild is itself a copy-engine job",
			MySQLError:  1071,
		})
	}

	// B1/B2: the table must be rebuildable by a copy engine.
	if _, ok := projected.PrimaryKey(); !ok {
		sev := SeverityWarn
		detail := "the table has no primary key after this migration"
		remediation := "gh-ost and pt-online-schema-change require a primary or unique key; add one in a separate migration"
		if len(projected.Indexes) == 0 {
			// Will be escalated by routing if a rebuild is required.
			sev = SeverityInfo
		}
		out = append(out, Check{
			ID: chkNoPrimaryKey, Severity: sev,
			Title: "table has no primary key", Detail: detail, Remediation: remediation,
		})
	}

	if !projected.HasUniqueKey() {
		out = append(out, Check{
			ID: chkNoUniqueKey, Severity: SeverityWarn,
			Title:       "table has no unique key",
			Detail:      "copy-based engines chunk by a unique key; without one they degrade to a single slow pass or refuse",
			Remediation: "add one before a copy engine can work, and expect that ALTER to be a rebuild: ALTER TABLE <table> ADD UNIQUE INDEX uq_id (<column>) - gh-ost needs a key to chunk on, and pt-osc needs one it can trust",
		})
	}

	// B3/B4: primary key shape drives engine choice.
	if pk, ok := projected.PrimaryKey(); ok {
		if len(pk.Parts) > 1 {
			out = append(out, Check{
				ID: chkCompositePK, Severity: SeverityInfo,
				Title:       "composite primary key",
				Detail:      fmt.Sprintf("primary key has %d columns", len(pk.Parts)),
				Remediation: "nothing to fix: this only rules out gh-ost, so the plan will choose pt-osc; set it explicitly with `saksama engine set pt-osc` if you would rather not leave it to the analyzer",
			})
		}
		if len(pk.Parts) == 1 {
			if col, ok := projected.Column(pk.Parts[0].Column); ok && col.IsUUIDLike() {
				out = append(out, Check{
					ID: chkUUIDPrimaryKey, Severity: SeverityWarn,
					Title:       "UUID primary key",
					Detail:      fmt.Sprintf("primary key %q is %s", col.Name, col.Type),
					Remediation: "nothing to fix before applying, but size the window for it: random UUIDv4 spreads inserts across the whole index, so the copy is slower and less predictable than on an auto-increment key",
				})
			}
		}
	}

	// B5: uniqueness cannot be proven offline.
	if hasKind(in.Ops, model.OpAddUniqueIndex) && in.SchemaSource != "live" {
		out = append(out, Check{
			ID: chkUniqueNeedsScan, Severity: SeverityInfo,
			Title:       "unique index not checked for duplicates",
			Detail:      "duplicate detection requires a live query over the indexed columns",
			Remediation: "connect with --live so the duplicate probe can run, or check it yourself before applying: SELECT COUNT(*) FROM (SELECT 1 FROM <table> GROUP BY <columns> HAVING COUNT(*) > 1) d",
			MySQLError:  1062,
		})
	}

	// B7: foreign key column without a supporting index.
	for _, fk := range projected.ForeignKeys {
		if len(fk.Columns) == 0 {
			continue
		}
		first := fk.Columns[0]
		found := false
		for _, ix := range projected.Indexes {
			if len(ix.Parts) > 0 && strings.EqualFold(ix.Parts[0].Column, first) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, Check{
				ID: chkFKNoIndex, Severity: SeverityWarn,
				Title:       "foreign key column has no index prefix",
				Detail:      fmt.Sprintf("FK %q column %q is not the first column of any index", fk.Name, first),
				Remediation: "add the missing index in its own change: ALTER TABLE <child> ADD INDEX ix_<col> (<col>); without it, deleting a parent row scans this whole table, and MySQL needs the index anyway to keep the constraint fast",
			})
		}
	}

	// B8: index bloat.
	if len(projected.Indexes) > 20 {
		out = append(out, Check{
			ID: chkTooManyIndexes, Severity: SeverityInfo,
			Title:       "many indexes",
			Detail:      fmt.Sprintf("table would have %d indexes", len(projected.Indexes)),
			Remediation: "nothing to fix now: each index slows inserts and makes every future rebuild longer, so drop any this query did not justify - SHOW INDEX FROM <table> lists them with their cardinality",
		})
	}

	return out
}

// checksForeignKeys validates FK hazards against the columns the ALTER touches.
func checksForeignKeys(in Input, projected model.Table) []Check {
	var out []Check

	// The touched-column map drives C1 and C2 only. Everything else in this
	// function is a fact about the table that a reader needs before any change,
	// and it used to sit behind this guard - so `ADD INDEX`, which touches no
	// column, silently reported no referential actions and no constraint count.
	touched := touchedColumns(in.Ops)

	// C1: outgoing FK columns being changed.
	for _, fk := range in.Table.ForeignKeys {
		for _, col := range fk.Columns {
			if touched[strings.ToLower(col)] {
				out = append(out, Check{
					ID: chkFKColumnTouched, Severity: SeverityBlocker,
					Title:       "column is used by a foreign key on this table",
					Detail:      fmt.Sprintf("FK %q constrains %s, which this ALTER changes", fk.Name, col),
					Remediation: "MySQL refuses this with error 1832, so change the constraint first and in a separate step: ALTER TABLE <table> DROP FOREIGN KEY <fk>; then the column change; then ADD CONSTRAINT <fk> ... back. Saksama does not edit foreign keys in v1, and the ALTER above must not touch a constrained column",
					MySQLError:  1832,
				})
			}
		}
	}

	// C2: columns referenced by incoming FKs from other tables.
	for _, fk := range in.Table.ReferencedBy {
		for _, col := range fk.RefColumns {
			if touched[strings.ToLower(col)] {
				out = append(out, Check{
					ID: chkFKReferencedTouched, Severity: SeverityBlocker,
					Title:       "column is referenced by a foreign key from another table",
					Detail:      fmt.Sprintf("FK %q on %s references %s.%s", fk.Name, fk.RefTable, in.Table.Name, col),
					Remediation: "the child table owns this constraint and MySQL refuses with 1832, so the child comes first: on <child>, DROP FOREIGN KEY <fk>, then change this column, then ADD CONSTRAINT <fk> ... back on the child",
					MySQLError:  1832,
				})
			}
		}
	}

	// C3: referential actions must survive any constraint recreation.
	for _, fk := range in.Table.ForeignKeys {
		if fk.HasAction() {
			out = append(out, Check{
				ID: chkFKActionPreserve, Severity: SeverityInfo,
				Title:       "foreign key carries a referential action",
				Detail:      fmt.Sprintf("FK %q is ON DELETE %s / ON UPDATE %s", fk.Name, fk.OnDelete, fk.OnUpdate),
				Remediation: "keep the actions in the new definition, or a parent delete silently starts doing something else: ALTER TABLE <child> ADD CONSTRAINT <fk> FOREIGN KEY (<col>) REFERENCES <parent> (<id>) ON DELETE <action> ON UPDATE <action>",
			})
		}
	}

	// C7: a nullable foreign key column is a legal value that no constraint
	// checks, and it decides what a parent delete actually does.
	for _, fk := range in.Table.ForeignKeys {
		var nullable []string
		for _, col := range fk.Columns {
			if c, ok := in.Table.Column(col); ok && c.Nullable {
				nullable = append(nullable, col)
			}
		}
		if len(nullable) == 0 {
			continue
		}
		detail := fmt.Sprintf("FK %q covers nullable column(s) %s", fk.Name, strings.Join(nullable, ", "))
		if fk.OnDelete == model.ActionSetNull {
			detail += ", and it is ON DELETE SET NULL: deleting a parent writes NULL here through the constraint"
		}
		out = append(out, Check{
			ID: chkFKColumnNullable, Severity: SeverityInfo,
			Title:       "foreign key column is nullable",
			Detail:      detail,
			Remediation: "find the rows that rely on it first: SELECT COUNT(*) FROM <child> WHERE <col> IS NULL; then decide. If a dangling child must be impossible, make the column NOT NULL and recreate the constraint as ON DELETE RESTRICT in the same window: ALTER TABLE <child> MODIFY <col> <type> NOT NULL, DROP FOREIGN KEY <fk>, ADD CONSTRAINT <fk> FOREIGN KEY (<col>) REFERENCES <parent> (<id>) ON DELETE RESTRICT. If SET NULL is intended, the application must already read NULL as 'no parent'",
		})
	}

	// C5: many constraints.
	if n := len(in.Table.AllForeignKeys()); n > 10 {
		out = append(out, Check{
			ID: chkManyForeignKeys, Severity: SeverityWarn,
			Title:       "many foreign key constraints",
			Detail:      fmt.Sprintf("%d constraints involve this table", n),
			Remediation: "nothing to fix before applying, but budget for it: every constraint listed above has to be dropped and re-added around a column change, and each gap is an integrity window - do them in one window, not several",
		})
	}

	return out
}

// checksOperational reports environment and blast-radius facts.
func checksOperational(in Input, anyRebuild bool) []Check {
	var out []Check

	out = append(out, Check{
		ID: chkLockWaitTimeout, Severity: SeverityInfo,
		Title:       "session lock_wait_timeout is bounded",
		Detail:      "Saksama sets a short lock_wait_timeout so DDL fails fast instead of queueing every query behind a metadata lock",
		Remediation: "none needed; this is the default safety behaviour",
		MySQLError:  1205,
	})

	if in.Table.Triggers != nil && len(in.Table.Triggers) > 0 {
		out = append(out, Check{
			ID: chkExistingTrigger, Severity: SeverityWarn,
			Title:       "table already has triggers",
			Detail:      fmt.Sprintf("%d existing trigger(s): %s", len(in.Table.Triggers), strings.Join(in.Table.Triggers, ", ")),
			Remediation: "gh-ost and pt-osc create their own triggers, and this table already has some. Read them before a copy-engine run: SHOW TRIGGERS FROM <database> WHERE `Table` = '<table>'; if they mutate the same columns, plan the trigger change first",
		})
	}

	if in.Production {
		out = append(out, Check{
			ID: chkProductionTarget, Severity: SeverityWarn,
			Title:       "target is marked as production",
			Detail:      "apply requires typing the hostname to confirm",
			Remediation: "apply this plan yourself, typing the hostname when asked: --production --confirm <host>. The friction is the point - it is the difference between reading a plan and running it on the wrong server",
		})
	}

	// D11: generated columns are computed by the server, so a table rebuilt by
	// anything else has to recreate them exactly.
	var generated []string
	for _, c := range in.Table.Columns {
		if c.Generated {
			generated = append(generated, c.Name)
		}
	}
	if len(generated) > 0 {
		out = append(out, Check{
			ID: chkGeneratedColumn, Severity: SeverityInfo,
			Title:       "table has generated columns",
			Detail:      fmt.Sprintf("%d generated column(s): %s", len(generated), strings.Join(generated, ", ")),
			Remediation: "read the expressions before choosing an engine: SHOW CREATE TABLE <table>. A copy engine recreates the table from its own definition, so an expression that does not survive unchanged means the copy silently holds different values. pt-osc has preserved generated columns the longest: if a rebuild is needed and both engines fit, pass --engine pt-osc, and check the version first with saksama engine doctor pt-osc",
		})
	}

	// E4: the installed toolkit is older than what this build verifies against.
	// 2.x has a narrower ALTER grammar and no --resume, so a stopped copy restarts
	// from nothing - a difference an operator finds out about at the worst moment.
	if bin, ok := in.Binaries["pt-online-schema-change"]; ok && bin.Available {
		if v, err := model.ParseVersion(bin.Version); err == nil && !v.AtLeast(3, 0, 0) {
			out = append(out, Check{
				ID: chkOldPtOSC, Severity: SeverityWarn,
				Title:       "Percona Toolkit is older than 3.0",
				Detail:      fmt.Sprintf("pt-online-schema-change is %s", bin.Version),
				Remediation: "3.x is what this build verifies against, and the difference shows up mid-migration: 2.x has no --resume, so an interrupted copy starts over. Install the verified build with: saksama engine install pt-osc",
			})
		}
	}

	// D9: CHECK constraints travel with the copy, and a row that violates one fails
	// the copy with 3819 - after minutes of work, at the swap, on a table that had
	// been fine until then. Since 8.0.16 the server enforces them, so a table carried
	// over from an older one can hold exactly those rows without anyone noticing.
	if len(in.Table.Checks) > 0 {
		var names, unenforced []string
		for _, chk := range in.Table.Checks {
			label := chk.Name
			if label == "" {
				label = chk.Expression
			}
			names = append(names, label)
			if !chk.Enforced {
				unenforced = append(unenforced, label)
			}
		}
		detail := fmt.Sprintf("%d constraint(s) copied into the shadow table: %s", len(names), strings.Join(names, ", "))
		if len(unenforced) > 0 {
			detail += fmt.Sprintf(" (%d NOT ENFORCED, so the current rows are allowed to violate them)", len(unenforced))
		}
		violated := violatedChecks(in)
		if len(violated) > 0 {
			detail += "; rows already violate " + strings.Join(violated, ", ")
		} else if in.Live != nil && in.Live.CheckProbeSkipped != "" {
			detail += "; not probed: " + in.Live.CheckProbeSkipped
		} else if in.Live != nil && in.Live.CheckViolations != nil {
			detail += "; no violating rows found"
		}
		out = append(out, Check{
			ID: chkCheckConstraint, Severity: severityFor(in, anyRebuild, len(violated) > 0),
			Title:  "table has CHECK constraints",
			Detail: detail,
			Remediation: "find the rows that would break the copy before it starts, one query per constraint: " +
				checkProbeQuery(in.Table.Name, in.Table.Checks) +
				". Rows the constraint cannot accept fail the copy with 3819 part-way through; fix the data, or drop the constraint deliberately first",
			MySQLError: 3819,
		})
	}

	if size := in.Table.SizeBytes(); size > 100<<30 {
		out = append(out, Check{
			ID: chkHugeTable, Severity: SeverityWarn,
			Title:       "large table",
			Detail:      fmt.Sprintf("table is %s", humanBytes(size)),
			Remediation: "give a rebuild a window rather than a pause: plan the copy engine (gh-ost or pt-osc) so it can be throttled and paused, and pass --free-disk and --disk-total so D6 can prove there is room",
		})
	}

	return out
}

// touchedColumns returns the lower-cased columns an ALTER modifies or drops.
func touchedColumns(ops []model.Op) map[string]bool {
	out := map[string]bool{}
	for _, op := range ops {
		switch op.Kind {
		case model.OpModifyColumn, model.OpDropColumn, model.OpChangeColumn, model.OpRenameColumn:
			for _, c := range op.RefColumns() {
				if c != "" {
					out[strings.ToLower(c)] = true
				}
			}
		}
	}
	return out
}

func hasKind(ops []model.Op, kind model.OpKind) bool {
	for _, op := range ops {
		if op.Kind == kind {
			return true
		}
	}
	return false
}

func rowFormatOr(rf string) string {
	if rf == "" {
		return "DEFAULT"
	}
	return strings.ToUpper(rf)
}

func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", n, units[i])
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// severityFor is D9's one judgement: a violation on a plan that copies the table is
// a warning about a migration that will die half-way; on a plan that does not touch
// the rows it is information.
func severityFor(in Input, anyRebuild, violated bool) Severity {
	if violated && (anyRebuild || in.ForExecution) {
		return SeverityWarn
	}
	if anyRebuild {
		return SeverityWarn
	}
	return SeverityInfo
}

// violatedChecks names the constraints the live probe found rows for.
func violatedChecks(in Input) []string {
	if in.Live == nil || in.Live.CheckViolations == nil {
		return nil
	}
	var out []string
	for _, chk := range in.Table.Checks {
		label := chk.Name
		if label == "" {
			label = chk.Expression
		}
		if n := in.Live.CheckViolations[label]; n > 0 {
			out = append(out, fmt.Sprintf("%s (%d row(s))", label, n))
		}
	}
	return out
}

// checkProbeQuery is the query the operator can run, written out rather than
// described, so the remediation is something to paste.
func checkProbeQuery(table string, checks []model.Check) string {
	var parts []string
	for i, chk := range checks {
		if i == 3 {
			parts = append(parts, "...")
			break
		}
		parts = append(parts, fmt.Sprintf("SELECT COUNT(*) FROM `%s` WHERE NOT (%s)", table, chk.Expression))
	}
	return strings.Join(parts, "; ")
}
