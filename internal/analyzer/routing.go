package analyzer

import (
	"fmt"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// ladder is the order engines are tried in when nothing has been chosen.
//
// Cheapest and least invasive first: a native ALTER copies nothing, gh-ost is a
// single MIT binary, pt-osc needs Perl and is GPL-2.
var ladder = []Engine{EngineNative, EngineGhOst, EnginePtOSC}

// route picks the engine using the suitability matrix, then applies the
// operator's override.
//
// Routing deliberately does not re-implement engine limitations. If gh-ost
// cannot handle foreign keys, that lives in gh-ost's requirements, not in a
// separate policy rule that would drift out of sync.
func route(in Input, assessments []EngineAssessment, anyRebuild, anyNative, anyBlocked bool, projected model.Table) Recommendation {
	rec := Recommendation{Engine: EngineNone, EngineSource: "recommendation"}
	_ = anyRebuild
	_ = anyNative

	if anyBlocked {
		rec.Reasons = append(rec.Reasons, "an operation is unsupported or unsafe")
	}

	// 1. What would the rules pick on their own?
	//
	// Prefer an engine this server can actually start. A server without binary
	// logging would otherwise get a recommendation it cannot follow, when the
	// other copy engine would work right now. The blocked engine still says why in
	// its own assessment, and an explicit choice is honoured below.
	for _, want := range ladder {
		if a, ok := findAssessment(assessments, want); ok && a.Verdict == VerdictSuitable && a.EnvironmentReady() {
			rec.Engine = want
			break
		}
	}
	if rec.Engine == EngineNone {
		// Nothing is fully available: name the best fit anyway, so the plan can say
		// what to fix rather than shrugging.
		for _, want := range ladder {
			if a, ok := findAssessment(assessments, want); ok && a.Verdict == VerdictSuitable {
				rec.Engine = want
				break
			}
		}
	}
	rec.Analyzer = rec.Engine

	switch rec.Engine {
	case EngineNative:
		rec.Reasons = append(rec.Reasons,
			"every operation has a verified native DDL rule; no table copy is needed")
		if allInstant(in.Ops) {
			rec.Reasons = append(rec.Reasons, "all changes are metadata-only INSTANT operations")
		}
	case EngineGhOst:
		rec.Reasons = append(rec.Reasons,
			"the change needs a table rebuild, and gh-ost is the cheapest engine that fits this table and this server")
	case EnginePtOSC:
		if gh, ok := findAssessment(assessments, EngineGhOst); ok && gh.Verdict == VerdictSuitable && !gh.EnvironmentReady() {
			rec.Reasons = append(rec.Reasons,
				"gh-ost suits this table but this server cannot start it, so the copy engine that works now is pt-osc")
		} else {
			rec.Reasons = append(rec.Reasons,
				"the change needs a table rebuild; gh-ost does not fit this table, and pt-osc does")
		}
	}

	// 2. The operator's decision wins, even when it disagrees.
	if in.EngineHint != "" {
		source := in.EngineHintSource
		if source == "" {
			source = "flag"
		}
		rec.EngineSource = source
		if in.EngineHint != rec.Analyzer {
			rec.Reasons = append(rec.Reasons, fmt.Sprintf(
				"engine overridden to %s by %s (analyzer would have picked %s)",
				in.EngineHint, source, rec.Analyzer))
		}
		rec.Engine = in.EngineHint
	}

	// 3. Fallbacks are informational only; nothing is used implicitly.
	if rec.Engine != EngineNone {
		for _, want := range ladder {
			if want == rec.Engine {
				continue
			}
			if a, ok := findAssessment(assessments, want); ok && a.Verdict == VerdictSuitable {
				rec.Fallbacks = append(rec.Fallbacks, want)
			}
		}
	}

	return rec
}

// engineChecks turns the routing decision into blockers the operator can act on.
func engineChecks(in Input, assessments []EngineAssessment, rec Recommendation) []Check {
	var out []Check

	effective, ok := findAssessment(assessments, rec.Engine)
	if rec.Engine == EngineNone {
		var lines []string
		for _, a := range assessments {
			if a.Verdict == VerdictSuitable {
				continue
			}
			if unmet := unmetRequirements(a); len(unmet) > 0 {
				lines = append(lines, fmt.Sprintf("%s: %s", a.Engine, strings.Join(unmet, ", ")))
			}
		}
		out = append(out, Check{
			ID: chkNoEngineFits, Severity: SeverityBlocker,
			Title:       "no engine can perform this change",
			Detail:      "every candidate fails at least one requirement. " + strings.Join(lines, "; "),
			Remediation: "resolve the requirement above (add a key, remove the conflicting constraint, or rewrite the change) and plan again",
		})
		return out
	}

	if ok && effective.Verdict != VerdictSuitable {
		out = append(out, Check{
			ID: chkChosenEngineUnsuitable, Severity: SeverityBlocker,
			Title: fmt.Sprintf("%s cannot perform this change", effective.Engine),
			Detail: fmt.Sprintf("unmet requirement(s): %s",
				strings.Join(unmetRequirements(effective), ", ")),
			Remediation: fmt.Sprintf("choose another engine (see `saksama advise`), or clear the recorded choice with `saksama engine clear`"),
		})
	}

	// Engine-specific operational caveats that are not go/no-go decisions.
	if ok && effective.Verdict == VerdictSuitable {
		if n := len(effectiveOwner(assessments, rec)); n > 0 {
			_ = n
		}
	}

	if ok && effective.Verdict == VerdictSuitable && !effective.Installed {
		out = append(out, Check{
			ID: chkEngineNotInstalled, Severity: SeverityBlocker,
			Title:       fmt.Sprintf("%s is not installed on this machine", effective.Engine),
			Detail:      "the engine fits this change, but Saksama cannot start it",
			Remediation: effective.Install,
		})
	}

	// Server configuration is a precondition, not a detail. An engine that suits
	// the table perfectly still cannot start without binary logging.
	if ok && effective.Verdict == VerdictSuitable && effective.Installed && !effective.EnvironmentReady() {
		var detail []string
		for _, r := range effective.Environment {
			if !r.Met {
				detail = append(detail, fmt.Sprintf("%s (%s)", r.Name, r.Detail))
			}
		}
		out = append(out, Check{
			ID: "R8", Severity: SeverityBlocker,
			Title:       fmt.Sprintf("the server is not configured for %s", effective.Engine),
			Detail:      strings.Join(detail, "; "),
			Remediation: strings.Join(FixHints(SettingChecks(in.Settings)), "; "),
		})
	}

	return out
}

// engineWarnings reports caveats that matter when a copy engine is running but
// do not change the go/no-go decision.
// hasMetadataLockInstrument reports whether the server has the table gh-ost uses to
// verify its cut-over lock.
func hasMetadataLockInstrument(srv model.Server) bool {
	v, err := model.ParseVersion(srv.Version)
	if err != nil {
		return false
	}
	return v.AtLeast(5, 7, 0)
}

func engineWarnings(in Input, assessments []EngineAssessment, rec Recommendation) []Check {
	var out []Check

	effective, ok := findAssessment(assessments, rec.Engine)
	if !ok || effective.Verdict != VerdictSuitable {
		return nil
	}

	if rec.Engine == EngineGhOst && !hasMetadataLockInstrument(in.Server) {
		// gh-ost verifies the cut-over's metadata lock through
		// performance_schema.metadata_locks and refuses to start without it. On 5.6
		// the wrapper passes --skip-metadata-lock-check so the run can happen at
		// all, which means gh-ost cannot warn about a lock queue either. Saying so
		// is the difference between a blind spot and a hidden one.
		out = append(out, Check{
			ID: "R11", Severity: SeverityWarn,
			Title:       "gh-ost cannot check the metadata lock queue on this server",
			Detail:      "performance_schema.metadata_locks does not exist before MySQL 5.7, so gh-ost runs with --skip-metadata-lock-check and will not detect a pending lock queue at cut-over",
			Remediation: "check SHOW PROCESSLIST and INFORMATION_SCHEMA.INNODB_TRX immediately before applying, or run this migration on 5.7 or newer",
		})
	}

	if rec.Engine == EnginePtOSC && len(in.Table.ForeignKeys) > 0 {
		var names []string
		for _, fk := range in.Table.ForeignKeys {
			names = append(names, fk.Name)
		}
		out = append(out, Check{
			ID: "R10", Severity: SeverityWarn,
			Title: "pt-osc will rename this table's foreign key constraints",
			Detail: fmt.Sprintf("%s will keep its foreign keys, but with an underscore prefix: %s",
				strings.Join(names, ", "), "_"+strings.Join(names, ", _")),
			Remediation: "rename them back in a separate, reviewed change if other tooling refers to the constraint names; Saksama does not edit foreign keys in v1",
		})
	}

	if rec.Engine == EnginePtOSC && len(in.Table.ReferencedBy) > 0 {
		out = append(out, Check{
			ID: "R4", Severity: SeverityWarn,
			Title:       "incoming foreign keys need an explicit strategy",
			Detail:      fmt.Sprintf("%d constraint(s) on other tables reference this table", len(in.Table.ReferencedBy)),
			Remediation: "pt-osc needs --alter-foreign-keys-method (auto is used here); rows in those tables are briefly rewritten, so schedule accordingly",
		})
	}

	// Choosing a copy engine where native would do is legal but expensive: it
	// copies the whole table and doubles the disk for nothing. Say so once.
	if rec.Engine != EngineNative && rec.Engine != EngineNone {
		if native, ok := findAssessment(assessments, EngineNative); ok && native.Verdict == VerdictSuitable {
			out = append(out, Check{
				ID: "R7", Severity: SeverityWarn,
				Title:       "a copy engine was chosen although native would suffice",
				Detail:      fmt.Sprintf("%s was chosen, but MySQL can do this change in place", rec.Engine),
				Remediation: "native avoids copying the table and needs no extra disk; keep the copy engine only if you want its throttling or cut-over pause",
			})
		}
	}

	// Say out loud when a cheaper engine was passed over because of the server.
	if rec.Engine != EngineGhOst && rec.Engine != EngineNone {
		if gh, ok := findAssessment(assessments, EngineGhOst); ok && gh.Verdict == VerdictSuitable && !gh.EnvironmentReady() {
			out = append(out, Check{
				ID: "R9", Severity: SeverityWarn,
				Title:       "gh-ost was passed over: the server is not configured for it",
				Detail:      strings.Join(gh.EnvironmentProblems(), ", "),
				Remediation: strings.Join(FixHints(SettingChecks(in.Settings)), "; "),
			})
		}
	}

	if rec.Engine == EngineGhOst && in.Server.IsManaged() {
		out = append(out, Check{
			ID: "R5", Severity: SeverityWarn,
			Title:       "managed server may refuse gh-ost",
			Detail:      "gh-ost needs privileges a managed service may not grant",
			Remediation: "on RDS this usually needs rds_superuser; otherwise use pt-osc on the same table",
		})
	}

	if rec.Engine == EngineGhOst && in.Live != nil && !in.Live.ReplicaConfigured {
		out = append(out, Check{
			ID: "R6", Severity: SeverityWarn,
			Title:       "no replica: the copy load lands on the primary",
			Detail:      "gh-ost will run with --allow-on-master",
			Remediation: "schedule a quiet window, or add a replica so the copy can read from it",
		})
	}

	return out
}

func effectiveOwner([]EngineAssessment, Recommendation) []string { return nil }

func findAssessment(all []EngineAssessment, e Engine) (EngineAssessment, bool) {
	for _, a := range all {
		if a.Engine == e {
			return a, true
		}
	}
	return EngineAssessment{}, false
}

func unmetRequirements(a EngineAssessment) []string {
	var out []string
	for _, r := range a.Requirements {
		if !r.Met {
			out = append(out, r.Name)
		}
	}
	return out
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

// estimateDuration still needs to know whether Native was chosen.
func estimateDuration(in Input, tbl model.Table, engine Engine) DurationEstimate {
	size := tbl.SizeBytes()
	if size == 0 && tbl.Rows > 0 {
		size = tbl.Rows * 500
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
		from := int(size / (100 << 20))
		to := int(size / (20 << 20))
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

	case EnginePtOSC:
		from := int(size / (15 << 20))
		to := int(size / (2 << 20))
		if from < 30 {
			from = 30
		}
		if to < from {
			to = from
		}
		return DurationEstimate{
			FromSeconds: from, ToSeconds: to, Confidence: "low",
			Basis: "row-by-row copy driven by triggers; throughput assumed between 2 and 15 MB/s and actual speed depends on load and throttling",
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
	case EngineGhOst, EnginePtOSC:
		return DiskEstimate{
			RequiredBytes: size * 2,
			Note:          "the copy table plus the retained old table peak at roughly twice the table size",
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
	case EnginePtOSC:
		return Recovery{
			BeforeExecution: "cancel stops the engine; the production table is untouched",
			DuringExecution: "cancel stops the engine; the production table is untouched, artifacts are cleaned up",
			AfterSuccess:    "no automatic data rollback: pt-osc swaps as soon as the copy finishes, so writes after that point live only in the new table",
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

	for _, fk := range projected.AllForeignKeys() {
		add(Verification{Kind: "foreign_key_present", Name: fk.Name})
	}

	return out
}
