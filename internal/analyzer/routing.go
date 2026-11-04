package analyzer

import (
	"fmt"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// routingChecks emits blockers that come from the engine decision itself rather
// than from a single rule.
func routingChecks(in Input, anyRebuild bool, projected model.Table) []Check {
	if !anyRebuild {
		return nil
	}
	var out []Check

	pk, hasPK := projected.PrimaryKey()
	if !hasPK {
		out = append(out, Check{
			ID: chkCopyEngineNoPK, Severity: SeverityBlocker,
			Title:       "a copy engine is required but the table has no primary key",
			Detail:      "this ALTER needs a table rebuild, and both gh-ost and pt-online-schema-change require a primary or unique key to chunk safely",
			Remediation: "add a primary key in a separate migration first, then run this change",
		})
	} else if len(pk.Parts) > 1 {
		out = append(out, Check{
			ID: chkCompositePKNoGhOst, Severity: SeverityBlocker,
			Title:       "a copy engine is required but the primary key is composite",
			Detail:      "gh-ost supports single-column keys only, and the pt-online-schema-change adapter is not enabled in v1",
			Remediation: "enable the pt-osc adapter, add a surrogate single-column key, or rewrite the change as a native operation",
		})
	}

	if fks := projected.AllForeignKeys(); len(fks) > 0 {
		out = append(out, Check{
			ID: chkFKCopyBlocked, Severity: SeverityBlocker,
			Title: "no available copy engine supports this table's foreign keys",
			Detail: fmt.Sprintf("%s has %d foreign key constraint(s) in or out; gh-ost does not support foreign keys, "+
				"and the pt-online-schema-change adapter is not enabled in this build", projected.Name, len(fks)),
			Remediation: "use a verified native operation, enable the pt-osc adapter, or run a reviewed maintenance procedure; " +
				"Saksama will not drop and recreate constraints automatically",
		})
	}

	return out
}

// route applies the engine ladder from docs/DECISIONS.md D2.
func route(in Input, anyRebuild, anyNative, anyBlocked bool, projected model.Table) Recommendation {
	rec := Recommendation{Engine: EngineNone}

	switch {
	case anyBlocked:
		rec.Reasons = append(rec.Reasons, "an operation is unsupported or unsafe")

	case anyRebuild:
		pk, hasPK := projected.PrimaryKey()
		switch {
		case !hasPK:
			rec.Reasons = append(rec.Reasons, "rebuild required but no primary key is available for chunking")
		case len(pk.Parts) > 1:
			rec.Reasons = append(rec.Reasons, "rebuild required with a composite primary key; gh-ost cannot chunk it")
		case len(projected.AllForeignKeys()) > 0:
			rec.Reasons = append(rec.Reasons, "rebuild required on a table with foreign keys; copy engines are blocked in v1")
		default:
			rec.Engine = EngineGhOst
			rec.Reasons = append(rec.Reasons,
				"at least one operation requires a table rebuild, so a copy engine is needed",
				"the table has a single-column primary key and no foreign keys, which satisfies gh-ost preconditions")
			if in.Server.IsManaged() {
				rec.Reasons = append(rec.Reasons,
					"managed server detected: gh-ost may need additional privileges (for example rds_superuser)")
			}
			if !hasReplicaHint(in) {
				rec.Reasons = append(rec.Reasons,
					"no replica was described, so gh-ost must run with --allow-on-master; throttling will be conservative")
			}
		}

	case anyNative:
		rec.Engine = EngineNative
		rec.Reasons = append(rec.Reasons,
			"every operation is supported by a verified native DDL rule; no table copy is needed")
		switch {
		case allInstant(in.Ops):
			rec.Reasons = append(rec.Reasons, "all operations are metadata-only INSTANT changes")
		default:
			rec.Reasons = append(rec.Reasons, "the operation is in-place and permits concurrent DML")
		}
	}

	if in.EngineHint != "" && in.EngineHint != rec.Engine {
		rec.Reasons = append(rec.Reasons, fmt.Sprintf("user override requested %s", in.EngineHint))
		rec.Engine = in.EngineHint
	}

	// Execution capability of this build, independent of safety.
	rec.Executable, rec.ExecutableReason = executableHere(rec.Engine)

	// Fallbacks are informational; they are never used implicitly.
	if rec.Engine == EngineNative {
		rec.Fallbacks = []Engine{EngineGhOst}
	}
	if rec.Engine == EngineGhOst {
		rec.Fallbacks = []Engine{EnginePtOSC}
	}

	return rec
}

// executableHere reports whether this build actually implements the engine.
//
// Only the native path exists today. Saying "recommended: gh-ost" while being
// unable to run gh-ost would be the most damaging kind of dishonesty: the plan
// card would read as ready and `apply` would then refuse.
func executableHere(e Engine) (bool, string) {
	switch e {
	case EngineNative:
		return true, ""
	case EngineGhOst:
		return false, "the gh-ost adapter is not implemented in this build"
	case EnginePtOSC:
		return false, "the pt-online-schema-change adapter is not implemented in this build"
	}
	return false, "no execution path was selected"
}

func allInstant(ops []model.Op) bool {
	for _, op := range ops {
		switch op.Kind {
		case model.OpAddColumn, model.OpDropColumn, model.OpSetDefault, model.OpDropDefault:
		default:
			return false
		}
	}
	return true
}

func hasReplicaHint(Input) bool { return false }

// estimateDuration returns a range plus confidence. It never returns a single
// authoritative number, because a single number is a lie (PRD §14).
func estimateDuration(in Input, tbl model.Table, engine Engine) DurationEstimate {
	size := tbl.SizeBytes()
	if size == 0 && tbl.Rows > 0 {
		size = tbl.Rows * 500 // rough row-width assumption
	}
	if size == 0 {
		return DurationEstimate{
			FromSeconds: 60, ToSeconds: 6 * 3600, Confidence: "low",
			Basis: "table size is unknown; the range is a wide worst-case bound",
		}
	}

	switch engine {
	case EngineNative:
		if allInstant(in.Ops) {
			return DurationEstimate{
				FromSeconds: 1, ToSeconds: 2, Confidence: "high",
				Basis: "INSTANT operations only modify metadata; the table is not scanned or rebuilt",
			}
		}
		from := int(size / (100 << 20)) // 100 MB/s
		to := int(size / (20 << 20))    // 20 MB/s
		if from < 2 {
			from = 2
		}
		if to < from {
			to = from
		}
		return DurationEstimate{
			FromSeconds: from, ToSeconds: to, Confidence: "medium",
			Basis: "in-place index build scans and sorts the table; throughput assumed between 20 and 100 MB/s with no local history",
		}

	case EngineGhOst:
		from := int(size / (20 << 20))
		to := int(size / (2 << 20))
		if from < 30 {
			from = 30
		}
		if to < from {
			to = from
		}
		return DurationEstimate{
			FromSeconds: from, ToSeconds: to, Confidence: "low",
			Basis: "row-by-row copy plus trigger overhead; throughput assumed between 2 and 20 MB/s and actual speed depends on load and throttling",
		}
	}

	return DurationEstimate{
		FromSeconds: 0, ToSeconds: 0, Confidence: "high",
		Basis: "no execution is planned",
	}
}

func estimateDisk(tbl model.Table, engine Engine) DiskEstimate {
	size := tbl.SizeBytes()
	switch engine {
	case EngineNative:
		if tbl.IndexLength > 0 {
			return DiskEstimate{
				RequiredBytes: tbl.IndexLength / 4,
				Note:          "in-place index build needs temporary work space inside the tablespace",
			}
		}
		return DiskEstimate{RequiredBytes: 0, Note: "metadata-only change; no measurable disk growth"}
	case EngineGhOst:
		return DiskEstimate{
			RequiredBytes: size * 2,
			Note:          "the ghost table plus the retained old table peak at roughly twice the table size",
		}
	}
	return DiskEstimate{RequiredBytes: 0, Note: "no execution is planned"}
}

func recoveryFor(engine Engine) Recovery {
	switch engine {
	case EngineNative:
		return Recovery{
			BeforeExecution: "nothing has been mutated",
			DuringExecution: "the server owns the DDL; cancellation may not be immediate and depends on the algorithm phase",
			AfterSuccess:    "forward migration only; schema changes are not data-rollback safe",
		}
	case EngineGhOst:
		return Recovery{
			BeforeExecution: "cancel stops the engine; the production table is untouched",
			DuringExecution: "cancel stops the engine; the production table is untouched, artifacts are cleaned up",
			AfterSuccess:    "no automatic data rollback: after cutover, new writes live only in the new table",
			HoldCutover:     true,
		}
	}
	return Recovery{BeforeExecution: "no execution is planned"}
}

func verifications(in Input, projected model.Table) []Verification {
	var out []Verification
	seen := map[string]bool{}
	add := func(v Verification) {
		key := v.Kind + "|" + v.Name
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, v)
	}

	for _, op := range in.Ops {
		switch op.Kind {
		case model.OpAddIndex, model.OpAddUniqueIndex, model.OpAddFulltext, model.OpAddSpatial:
			name := op.IndexName
			if name == "" && len(op.Parts) > 0 {
				name = op.Parts[0].Column
			}
			add(Verification{Kind: "index_exists", Name: name})
		case model.OpDropIndex:
			add(Verification{Kind: "index_absent", Name: op.IndexName})
		case model.OpAddColumn:
			if op.Column != nil {
				add(Verification{Kind: "column_exists", Name: op.Column.Name})
			}
		case model.OpDropColumn:
			if op.Column != nil {
				add(Verification{Kind: "column_absent", Name: op.Column.Name})
			}
		case model.OpAddPrimaryKey:
			add(Verification{Kind: "index_exists", Name: "PRIMARY"})
		}
	}

	// Always assert that constraints survived untouched.
	for _, fk := range projected.AllForeignKeys() {
		add(Verification{Kind: "foreign_key_present", Name: fk.Name})
	}

	return out
}

func engineLabel(e Engine) string {
	switch e {
	case EngineNative:
		return "native MySQL DDL"
	case EngineGhOst:
		return "gh-ost"
	case EnginePtOSC:
		return "pt-online-schema-change"
	}
	return strings.ToLower(string(e))
}
