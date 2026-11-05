// Package analyzer is the pure decision layer of Saksama.
//
// It takes a table model, a parsed ALTER, and a server description, and returns
// a Plan. It never touches a database, never mutates anything, and never guesses:
// unknown capabilities become blockers, not assumptions.
package analyzer

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// Method is the physical execution method MySQL would use.
type Method string

const (
	MethodInstant     Method = "INSTANT"     // metadata-only change
	MethodInplace     Method = "INPLACE"     // no table copy, concurrent DML allowed
	MethodCopy        Method = "COPY"        // table rebuild: needs a copy engine
	MethodUnsupported Method = "UNSUPPORTED" // not understood or not allowed
)

// Capability is the analyzer's verdict for a single ALTER operation.
type Capability struct {
	RuleID  string `json:"rule_id"`
	Method  Method `json:"method"`
	Lock    string `json:"lock,omitempty"` // NONE | SHARED | EXCLUSIVE
	Reason  string `json:"reason"`
	Native  bool   `json:"native"`  // executable as plain ALTER TABLE
	Rebuild bool   `json:"rebuild"` // requires a copy engine
	Blocked bool   `json:"blocked"` // not allowed at all
}

// capabilityFor is the version-gated native DDL matrix, documented in
// docs/CHECKS.md section F.
//
// Every rule is fail-closed: if a version is not explicitly covered, the result
// is COPY/blocked rather than "probably fine".
func capabilityFor(op model.Op, srv model.Server, tbl model.Table) Capability {
	v, err := model.ParseVersion(srv.Version)
	if err != nil {
		return Capability{
			RuleID: "R-VERSION-UNPARSEABLE", Method: MethodUnsupported, Blocked: true,
			Reason: fmt.Sprintf("cannot parse server version %q; refusing to guess DDL capability", srv.Version),
		}
	}
	if srv.IsMariaDB() {
		return Capability{
			RuleID: "R-MARIADB-UNSUPPORTED", Method: MethodUnsupported, Blocked: true,
			Reason: "MariaDB capability matrix is not verified by Saksama; refusing to plan against it",
		}
	}

	if v.Major < 5 || (v.Major == 5 && v.Minor < 6) {
		// 5.6 is the floor: it carries every fact this matrix needs, and it is
		// still found running applications that started in 2015. 5.5 and older
		// are not: gh-ost requires 5.6 or newer, and 5.5 reached end of life in
		// 2018.
		return Capability{
			RuleID: "R-MYSQL-55-UNSUPPORTED", Method: MethodUnsupported, Blocked: true,
			Reason: fmt.Sprintf("MySQL %d.%d is older than 5.6, the oldest server Saksama verifies; gh-ost requires 5.6 or newer and 5.5 reached end of life in 2018", v.Major, v.Minor),
		}
	}

	engine := strings.ToLower(tbl.Engine)
	if engine != "" && engine != "innodb" {
		return Capability{
			RuleID: "R-ENGINE-NOT-INNODB", Method: MethodUnsupported, Blocked: true,
			Reason: fmt.Sprintf("storage engine %q is not InnoDB; online DDL rules do not apply", tbl.Engine),
		}
	}

	switch op.Kind {
	case model.OpAddIndex, model.OpAddUniqueIndex:
		if v.AtLeast(5, 6, 0) {
			return Capability{
				RuleID: "R-ADD-INDEX-INPLACE", Method: MethodInplace, Lock: "NONE", Native: true,
				Reason: "secondary index creation is an in-place operation that permits concurrent DML; no table copy is required",
			}
		}
		return rebuild("R-ADD-INDEX-COPY", "secondary index creation requires a table rebuild before MySQL 5.6")

	case model.OpDropIndex:
		if v.AtLeast(5, 6, 0) {
			return Capability{
				RuleID: "R-DROP-INDEX-INPLACE", Method: MethodInplace, Lock: "NONE", Native: true,
				Reason: "dropping a secondary index is metadata-only and permits concurrent DML",
			}
		}
		return rebuild("R-DROP-INDEX-COPY", "dropping a secondary index requires a table rebuild before MySQL 5.6")

	case model.OpAddFulltext, model.OpAddSpatial:
		return rebuild("R-FULLTEXT-SPATIAL-COPY",
			"full-text and spatial index creation rebuilds the index and does not permit concurrent DML; treat it as a copy operation")

	case model.OpAddColumn:
		if op.Column != nil && op.Column.Generated {
			return rebuild("R-ADD-GENERATED-COLUMN-COPY", "adding a generated column is not an INSTANT operation")
		}
		if op.Column != nil && strings.EqualFold(op.Column.Extra, "auto_increment") {
			return rebuild("R-ADD-AUTOINC-COLUMN-COPY", "adding an AUTO_INCREMENT column is not an INSTANT operation")
		}
		notLast := op.Position != nil && op.Position.Set
		if v.AtLeast(8, 0, 29) {
			return Capability{
				RuleID: "R-ADD-COLUMN-INSTANT-8029", Method: MethodInstant, Native: true,
				Reason: "MySQL 8.0.29+ supports INSTANT ADD COLUMN in any position. INSTANT cannot be combined with an explicit LOCK clause (MySQL error 1221), so none is emitted",
			}
		}
		if v.AtLeast(8, 0, 12) {
			if notLast {
				return rebuild("R-ADD-COLUMN-MIDDLE-COPY",
					"INSTANT ADD COLUMN before MySQL 8.0.29 only supports appending at the end of the row; FIRST/AFTER requires a rebuild")
			}
			return Capability{
				RuleID: "R-ADD-COLUMN-INSTANT-8012", Method: MethodInstant, Native: true,
				Reason: "MySQL 8.0.12+ supports INSTANT ADD COLUMN when the column is appended. INSTANT cannot be combined with an explicit LOCK clause, so none is emitted",
			}
		}
		// Measured on mysql:5.6.51 and mysql:5.7.44: the server accepts
		// ALGORITHM=INPLACE, LOCK=NONE and then rebuilds the table. So the label
		// is INPLACE (that is what the server runs) while the cost is a rebuild
		// (every row is rewritten), and those two facts decide different things.
		return inplaceRebuild("R-ADD-COLUMN-REBUILD-INPLACE",
			"before MySQL 8.0.12 an added column is performed in place with LOCK=NONE, but the table is rebuilt: every row is rewritten")

	case model.OpDropColumn:
		if v.AtLeast(8, 0, 29) {
			return Capability{
				RuleID: "R-DROP-COLUMN-INSTANT-8029", Method: MethodInstant, Native: true,
				Reason: "MySQL 8.0.29+ supports INSTANT DROP COLUMN. INSTANT cannot be combined with an explicit LOCK clause, so none is emitted",
			}
		}
		if v.AtLeast(8, 0, 0) {
			// 8.0.0 - 8.0.28 is left as a rebuild without the stronger claim: no
			// test here has asked that server whether it accepts INPLACE.
			return rebuild("R-DROP-COLUMN-COPY", "INSTANT DROP COLUMN requires MySQL 8.0.29+; earlier versions rebuild the table")
		}
		return inplaceRebuild("R-DROP-COLUMN-REBUILD-INPLACE",
			"before MySQL 8.0.12 the server drops a column in place with LOCK=NONE, but the table is rebuilt: every row is rewritten")

	case model.OpSetDefault, model.OpDropDefault:
		if v.AtLeast(8, 0, 12) {
			return Capability{
				RuleID: "R-DEFAULT-INSTANT", Method: MethodInstant, Native: true,
				Reason: "changing a column default is an INSTANT metadata operation on MySQL 8.0.12+. INSTANT cannot be combined with an explicit LOCK clause, so none is emitted",
			}
		}
		// Measured on mysql:5.6.51 and mysql:5.7.44, not read off a matrix: both
		// accept ALGORITHM=INPLACE, LOCK=NONE for a default change and write zero
		// bytes to the tablespace. Calling it a rebuild sent 5.6 and 5.7 users to
		// gh-ost - and on 5.6 that also means accepting the unobservable metadata
		// lock queue - to change a default.
		return Capability{
			RuleID: "R-DEFAULT-INPLACE", Method: MethodInplace, Lock: "NONE", Native: true,
			Reason: "changing a column default is a metadata operation on MySQL 5.6 and 5.7: the server performs it in place and rewrites no rows",
		}

	case model.OpRenameColumn:
		if v.AtLeast(8, 0, 0) {
			return Capability{
				RuleID: "R-RENAME-COLUMN-INPLACE", Method: MethodInplace, Lock: "NONE", Native: true,
				Reason: "MySQL 8.0 renames a column as a metadata-only in-place operation",
			}
		}
		return rebuild("R-RENAME-COLUMN-COPY", "renaming a column rebuilds the table before MySQL 8.0")

	case model.OpRenameTable:
		return Capability{
			RuleID: "R-RENAME-TABLE-METADATA", Method: MethodInplace, Lock: "SHARED", Native: true,
			Reason: "renaming a table is a metadata operation that completes immediately",
		}

	case model.OpModifyColumn, model.OpChangeColumn:
		if c, ok := widenInPlace(op, v, tbl); ok {
			return c
		}
		if c, ok := relaxNullInPlace(op, tbl); ok {
			return c
		}
		return rebuild("R-MODIFY-COLUMN-COPY",
			"changing a column type or nullability is not proven in-place safe by Saksama; it is planned as a rebuild")

	case model.OpAddPrimaryKey:
		return rebuild("R-ADD-PK-COPY", "adding a primary key rebuilds the table")

	case model.OpDropPrimaryKey:
		return rebuild("R-DROP-PK-COPY", "dropping a primary key rebuilds the table")

	case model.OpConvertCharset:
		return rebuild("R-CONVERT-CHARSET-COPY", "converting the table character set rebuilds every row")

	case model.OpEngineChange:
		return rebuild("R-ENGINE-CHANGE-COPY", "changing the storage engine rebuilds the table")

	case model.OpAddForeignKey, model.OpDropForeignKey:
		return Capability{
			RuleID: "R-FK-DDL-OUT-OF-SCOPE", Method: MethodUnsupported, Blocked: true,
			Reason: "Saksama v1 does not mutate foreign key constraints; this is intentional, not a limitation to work around",
		}
	}

	return Capability{
		RuleID: "R-UNKNOWN-OP", Method: MethodUnsupported, Blocked: true,
		Reason: "operation is not understood by Saksama; unknown operations are blocked rather than guessed",
	}
}

// widenInPlace reports the one MODIFY COLUMN shape MySQL does without a rebuild:
// same type, same length-prefix width, only longer.
//
// Measured on mysql:5.7.44 (zero bytes written for varchar(32) -> varchar(64)) and
// refused there on 5.6 (error 1846), so the rule is gated at 5.7. Growing past 255
// characters changes the one-byte length prefix to two and does rebuild, which is
// why the prefix width has to match, not just the direction of the change.
func widenInPlace(op model.Op, v model.Version, tbl model.Table) (Capability, bool) {
	if op.Kind != model.OpModifyColumn || op.Column == nil {
		return Capability{}, false
	}
	if !v.AtLeast(5, 7, 0) {
		return Capability{}, false
	}
	cur, ok := findColumn(tbl, op.Column.Name)
	if !ok {
		return Capability{}, false
	}
	next := *op.Column
	switch strings.ToLower(cur.DataType) {
	case "varchar", "varbinary":
	default:
		return Capability{}, false
	}
	nextCharset := next.Charset
	if nextCharset == "" {
		nextCharset = cur.Charset
	}
	nextCollation := next.Collation
	if nextCollation == "" {
		nextCollation = cur.Collation
	}
	if !strings.EqualFold(cur.DataType, next.DataType) ||
		cur.Nullable != next.Nullable ||
		!strings.EqualFold(cur.Charset, nextCharset) ||
		!strings.EqualFold(cur.Collation, nextCollation) ||
		(cur.Default == nil) != (next.Default == nil) ||
		(cur.Default != nil && next.Default != nil && *cur.Default != *next.Default) {
		return Capability{}, false
	}
	curLen, okCur := declaredLength(cur.Type)
	nextLen, okNext := declaredLength(next.Type)
	if !okCur || !okNext || nextLen <= curLen {
		return Capability{}, false
	}
	// The length prefix is measured in bytes, not characters, and that is the
	// line MySQL draws. On a utf8mb4 column varchar(32) -> varchar(63) is in
	// place (252 bytes, still one length byte) while varchar(64) is not (256
	// bytes, two length bytes, rebuild) - measured on 8.0.46, where the server
	// accepts the first ALGORITHM=INPLACE and refuses the second with error 1846.
	perChar := charsetMaxLen(cur.Charset)
	if strings.EqualFold(cur.DataType, "varbinary") {
		perChar = 1
	}
	if lengthPrefixBytes(curLen*perChar) != lengthPrefixBytes(nextLen*perChar) {
		return Capability{}, false
	}
	return Capability{
		RuleID: "R-MODIFY-COLUMN-WIDEN-INPLACE", Method: MethodInplace, Lock: "NONE", Native: true,
		Reason: "MySQL 5.7 and later widen varchar/varbinary in place: the server rewrites no rows as long as the one- or two-byte length prefix stays the same size",
	}, true
}

func findColumn(tbl model.Table, name string) (model.Column, bool) {
	for _, c := range tbl.Columns {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return model.Column{}, false
}

// declaredLength reads "varchar(191)" -> 191.
func declaredLength(typ string) (int, bool) {
	open := strings.IndexByte(typ, '(')
	closeAt := strings.IndexByte(typ, ')')
	if open < 0 || closeAt < open {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(typ[open+1 : closeAt]))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// lengthPrefixBytes is how many bytes varchar spends on its length: one up to 255
// bytes of content, two above.
func lengthPrefixBytes(n int) int {
	if n > 255 {
		return 2
	}
	return 1
}

// inplaceRebuild is an operation the server performs without a shadow table and
// without blocking writes, but which still rewrites every row.
//
// Both halves matter and they answer different questions: LOCK=NONE is why it does
// not need a copy engine to be safe, and the rebuild is why it should have one when
// the table is big - MySQL cannot throttle, pause, resume, or cap lag on it.
func inplaceRebuild(ruleID, reason string) Capability {
	return Capability{RuleID: ruleID, Method: MethodInplace, Lock: "NONE", Rebuild: true, Native: true, Reason: reason}
}

// relaxNullInPlace reports the other MODIFY shape MySQL does in place while
// rewriting rows: keeping the type and only letting the column become nullable.
// Measured on 5.6.51, 5.7.44 and 8.0.46 - the server accepts ALGORITHM=INPLACE,
// LOCK=NONE and the tablespace id changes.
func relaxNullInPlace(op model.Op, tbl model.Table) (Capability, bool) {
	if op.Column == nil {
		return Capability{}, false
	}
	cur, ok := findColumn(tbl, op.Column.Name)
	if !ok || cur.Nullable || !op.Column.Nullable {
		return Capability{}, false
	}
	if !strings.EqualFold(cur.Type, op.Column.Type) {
		return Capability{}, false
	}
	return inplaceRebuild("R-MODIFY-COLUMN-RELAX-NULL-INPLACE",
		"MySQL performs this nullability change in place with LOCK=NONE, but the table is rebuilt: every row is rewritten"), true
}

func rebuild(ruleID, reason string) Capability {
	return Capability{RuleID: ruleID, Method: MethodCopy, Rebuild: true, Reason: reason}
}
