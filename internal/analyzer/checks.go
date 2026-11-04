package analyzer

import (
	"fmt"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// Check IDs are stable and documented in docs/CHECKS.md. Never renumber them.
const (
	// Charset & collation.
	chkFKCharsetMismatch = "A1"
	chkConvertCharsetFK  = "A2"
	chkFKColumnCharset   = "A3"
	chkUtf8mb3           = "A4"
	chkKeyTooLong        = "A5"
	chkCompactRowFormat  = "A6"
	chkBlobIndexNoPrefix = "A7"

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

	// Operational.
	chkLockWaitTimeout  = "D2"
	chkDiskHeadroom     = "D6"
	chkExistingTrigger  = "D7"
	chkProductionTarget = "D12"
	chkHugeTable        = "D17"

	// Capability.
	chkUnknownOp          = "E0"
	chkMariaDB            = "E1"
	chkEngineNotInnoDB    = "E2"
	chkVersionUnparseable = "E3"

	// Engine routing.
	chkCopyEngineNoPK     = "R1"
	chkCompositePKNoGhOst = "R2"
	chkFKCopyBlocked      = "R3"
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
				Remediation: "split the change into supported operations, or run it manually under a reviewed procedure",
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
			Remediation: "use a MySQL 8.0 server, or wait for the MariaDB capability profile",
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

	if e := in.Table.Engine; e != "" && !strings.EqualFold(e, "InnoDB") {
		out = append(out, Check{
			ID: chkEngineNotInnoDB, Severity: SeverityBlocker,
			Title:       "table is not InnoDB",
			Detail:      fmt.Sprintf("engine is %q", e),
			Remediation: "online DDL rules and both copy engines assume InnoDB",
		})
	}

	if in.Table.Partitioned {
		out = append(out, Check{
			ID: "D10", Severity: SeverityWarn,
			Title:       "table is partitioned",
			Detail:      "gh-ost does not support partitioned tables",
			Remediation: "plan a native operation, or use pt-online-schema-change with a verified capability profile",
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
			Remediation: "convert the FK columns explicitly, or drop and rebuild the constraints under a reviewed procedure",
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
					Remediation: "align the charset/collation before any operation that recreates the constraint",
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
			Remediation: "migrate to utf8mb4 to avoid deprecation issues and 3-byte truncation surprises",
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
				Remediation: "reduce the number or width of indexed columns, or use a prefix length",
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
					Remediation: "queries must filter on the same prefix column to benefit; the index cannot serve full-column ordering",
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
			Remediation: "consider ROW_FORMAT=DYNAMIC in a separate rebuild, or keep keys under 767 bytes",
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
			Remediation: "add a primary key or a unique index before using a copy engine",
		})
	}

	// B3/B4: primary key shape drives engine choice.
	if pk, ok := projected.PrimaryKey(); ok {
		if len(pk.Parts) > 1 {
			out = append(out, Check{
				ID: chkCompositePK, Severity: SeverityInfo,
				Title:       "composite primary key",
				Detail:      fmt.Sprintf("primary key has %d columns", len(pk.Parts)),
				Remediation: "gh-ost supports single-column keys only; a composite key forces pt-online-schema-change",
			})
		}
		if len(pk.Parts) == 1 {
			if col, ok := projected.Column(pk.Parts[0].Column); ok && col.IsUUIDLike() {
				out = append(out, Check{
					ID: chkUUIDPrimaryKey, Severity: SeverityWarn,
					Title:       "UUID primary key",
					Detail:      fmt.Sprintf("primary key %q is %s", col.Name, col.Type),
					Remediation: "random UUIDv4 makes range chunking uneven; expect slower, less predictable copy throughput",
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
			Remediation: "run with a live connection, or verify duplicates manually before applying",
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
				Remediation: "add an index on the FK column; without one, parent row deletes scan the child table",
			})
		}
	}

	// B8: index bloat.
	if len(projected.Indexes) > 20 {
		out = append(out, Check{
			ID: chkTooManyIndexes, Severity: SeverityInfo,
			Title:       "many indexes",
			Detail:      fmt.Sprintf("table would have %d indexes", len(projected.Indexes)),
			Remediation: "each index slows writes and lengthens any future rebuild",
		})
	}

	return out
}

// checksForeignKeys validates FK hazards against the columns the ALTER touches.
func checksForeignKeys(in Input, projected model.Table) []Check {
	var out []Check

	touched := touchedColumns(in.Ops)
	if len(touched) == 0 {
		return nil
	}

	// C1: outgoing FK columns being changed.
	for _, fk := range in.Table.ForeignKeys {
		for _, col := range fk.Columns {
			if touched[strings.ToLower(col)] {
				out = append(out, Check{
					ID: chkFKColumnTouched, Severity: SeverityBlocker,
					Title:       "column is used by a foreign key on this table",
					Detail:      fmt.Sprintf("FK %q constrains %s, which this ALTER changes", fk.Name, col),
					Remediation: "MySQL refuses this (1832). Change the constraint in a reviewed procedure, or use an operation that does not touch the FK column",
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
					Remediation: "the child constraint blocks the change (1832); coordinate the child table or use a compatible native operation",
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
				Remediation: "any reproduction of this constraint must keep these actions; dropping them silently changes data semantics",
			})
		}
	}

	// C5: many constraints.
	if n := len(in.Table.AllForeignKeys()); n > 10 {
		out = append(out, Check{
			ID: chkManyForeignKeys, Severity: SeverityWarn,
			Title:       "many foreign key constraints",
			Detail:      fmt.Sprintf("%d constraints involve this table", n),
			Remediation: "each one is a potential integrity window during any constraint change",
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
			Remediation: "gh-ost and pt-osc create their own triggers; review interaction before using a copy engine",
		})
	}

	if in.Production {
		out = append(out, Check{
			ID: chkProductionTarget, Severity: SeverityWarn,
			Title:       "target is marked as production",
			Detail:      "apply requires typing the hostname to confirm",
			Remediation: "confirm the hostname deliberately; this friction is intentional",
		})
	}

	if anyRebuild {
		size := in.Table.SizeBytes()
		need := size * 2
		detail := fmt.Sprintf("a copy-based migration needs roughly %s of additional space (shadow table plus the old table)", humanBytes(need))
		freeDisk := in.FreeDisk
		if freeDisk == 0 && in.Live != nil {
			freeDisk = in.Live.FreeDiskBytes
		}
		if in.Live != nil && in.Live.FreeDiskBytes > 0 {
			freeDisk = in.Live.FreeDiskBytes
		}
		if freeDisk > 0 {
			if freeDisk < need {
				out = append(out, Check{
					ID: chkDiskHeadroom, Severity: SeverityBlocker,
					Title:       "insufficient free disk space",
					Detail:      fmt.Sprintf("%s free but %s required", humanBytes(freeDisk), humanBytes(need)),
					Remediation: "free space or run the migration on a larger volume before applying",
				})
			} else {
				out = append(out, Check{
					ID: chkDiskHeadroom, Severity: SeverityInfo,
					Title:  "disk headroom looks sufficient",
					Detail: fmt.Sprintf("%s free, %s required", humanBytes(in.FreeDisk), humanBytes(need)),
				})
			}
		} else {
			out = append(out, Check{
				ID: chkDiskHeadroom, Severity: SeverityWarn,
				Title:       "free disk space was not measured",
				Detail:      detail,
				Remediation: "verify free space on the data volume before applying",
			})
		}
	}

	if size := in.Table.SizeBytes(); size > 100<<30 {
		out = append(out, Check{
			ID: chkHugeTable, Severity: SeverityWarn,
			Title:       "large table",
			Detail:      fmt.Sprintf("table is %s", humanBytes(size)),
			Remediation: "any rebuild will take a long time; plan a window and expect throttling",
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
