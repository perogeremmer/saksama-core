// Package analyzer is the pure decision layer of Saksama.
//
// It takes a table model, a parsed ALTER, and a server description, and returns
// a Plan. It never touches a database, never mutates anything, and never guesses:
// unknown capabilities become blockers, not assumptions.
package analyzer

import (
	"fmt"
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
		return rebuild("R-ADD-COLUMN-COPY", "INSTANT ADD COLUMN requires MySQL 8.0.12+")

	case model.OpDropColumn:
		if v.AtLeast(8, 0, 29) {
			return Capability{
				RuleID: "R-DROP-COLUMN-INSTANT-8029", Method: MethodInstant, Native: true,
				Reason: "MySQL 8.0.29+ supports INSTANT DROP COLUMN. INSTANT cannot be combined with an explicit LOCK clause, so none is emitted",
			}
		}
		return rebuild("R-DROP-COLUMN-COPY", "INSTANT DROP COLUMN requires MySQL 8.0.29+; earlier versions rebuild the table")

	case model.OpSetDefault, model.OpDropDefault:
		if v.AtLeast(8, 0, 12) {
			return Capability{
				RuleID: "R-DEFAULT-INSTANT", Method: MethodInstant, Native: true,
				Reason: "changing a column default is an INSTANT metadata operation on MySQL 8.0.12+. INSTANT cannot be combined with an explicit LOCK clause, so none is emitted",
			}
		}
		return rebuild("R-DEFAULT-COPY", "changing a column default requires a rebuild before MySQL 8.0.12")

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

func rebuild(ruleID, reason string) Capability {
	return Capability{RuleID: ruleID, Method: MethodCopy, Rebuild: true, Reason: reason}
}
