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
		switch {
		case anyRebuild:
			rec.Reasons = append(rec.Reasons,
				fmt.Sprintf("every operation has a verified native DDL rule; %s rebuilds the table in place, which needs no install and no restart, but MySQL cannot throttle or pause it", strings.Join(rebuildKinds(in), ", ")))
		case allInstant(in.Ops):
			rec.Reasons = append(rec.Reasons,
				"every operation has a verified native DDL rule; all changes are metadata-only INSTANT operations")
		default:
			rec.Reasons = append(rec.Reasons,
				"every operation has a verified native DDL rule; no table copy is needed")
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

// A rebuild of a table this size is seconds even on a slow disk; above it, the
// time is dominated by IO that cannot be measured from here, and that is exactly
// the case a copy engine exists for. These are policy numbers, not server facts.
const (
	nativeRebuildMaxRows  int64 = 100_000
	nativeRebuildMaxBytes int64 = 64 << 20
)

// nativeRebuildVerdict decides whether native may rebuild the table, and says why
// either way. Silence is not an option: the operator has to be able to see what
// was traded.
func nativeRebuildVerdict(in Input, rebuild []string) (bool, string) {
	if len(rebuild) == 0 {
		return true, ""
	}
	ops := strings.Join(dedupe(rebuild), ", ")
	tbl := in.Table
	size := fmt.Sprintf("%s rows / %s", humanCount(tbl.Rows), humanBytes(tbl.DataLength))

	if tbl.Rows == 0 && tbl.DataLength == 0 {
		return false, fmt.Sprintf("%s rebuilds the table, and its size was not measured: without that there is no telling whether an in-place rebuild is seconds or hours, so a copy engine is the answer",
			ops)
	}
	if tbl.Rows > nativeRebuildMaxRows || tbl.DataLength > nativeRebuildMaxBytes {
		return false, fmt.Sprintf("%s rebuilds %s in place. MySQL cannot throttle, pause, resume, or cap replication lag on that, which is the whole reason gh-ost and pt-osc exist; native is offered only below %s rows / %s",
			ops, size, humanCount(nativeRebuildMaxRows), humanBytes(nativeRebuildMaxBytes))
	}
	if in.Production && !in.AllowNativeRebuild {
		return false, fmt.Sprintf("%s rebuilds the table in place. At %s that is seconds, but production wants the throttling and pause a copy engine gives, so it stays blocked until --allow-native-rebuild says the trade is understood",
			ops, size)
	}
	return true, fmt.Sprintf("%s rebuilds the table, but at %s that is seconds: native runs it in place with LOCK=NONE and needs no install and no restart, though it cannot be throttled or paused",
		ops, size)
}

func humanCount(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	// Group thousands without importing a formatting package for four lines.
	s := fmt.Sprintf("%d", n)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
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
			Remediation: "fix the unmet requirement listed for each engine above - a copy engine needs a key to chunk on, and neither engine can work while a foreign key covers the column being changed - then plan again. `saksama advise` prints the same matrix with what each engine wants",
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
			Remediation: "if other tooling refers to the constraint names, rename them back in a separate change after the copy: `ALTER TABLE <table> RENAME INDEX _<name> TO <name>` (Saksama does not edit foreign keys in v1), and update the verification list in the session record to match",
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
				Remediation: "native avoids the copy and the extra disk; keep this engine only if you want its throttling or its cut-over pause. To switch: plan again with `saksama engine set native`, or clear the recorded choice with `saksama engine clear`",
			})
		}
	}

	// When native was passed over only because a rebuild needs a decision, say so.
	// Otherwise the operator reads "pt-osc is not installed" and never learns that
	// the server could have done it - the exact advice nobody can follow this
	// tool keeps having to fix.
	if rec.Engine != EngineNative && rec.Engine != EngineNone {
		if native, ok := findAssessment(assessments, EngineNative); ok && native.Verdict != VerdictSuitable {
			for _, req := range native.Requirements {
				if req.Met || req.Name != "no table rebuild required" || req.Detail == "" {
					continue
				}
				out = append(out, Check{
					ID: "R13", Severity: SeverityWarn,
					Title:       "native could rebuild this table, but that needs a decision",
					Detail:      req.Detail,
					Remediation: "if rebuilding this table in place is acceptable, re-run with --allow-native-rebuild (production only; it needs no install and no restart), otherwise keep the copy engine for its throttling and pause",
				})
				break
			}
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

	// A native rebuild is cheap and needs nothing installed, but it is not free:
	// say what it does not give, in production and outside it.
	if rec.Engine == EngineNative {
		if kinds := rebuildKinds(in); len(kinds) > 0 {
			out = append(out, Check{
				ID: "R12", Severity: SeverityWarn,
				Title: "native will rebuild this table in place",
				Detail: fmt.Sprintf("%s rewrites %s rows / %s inside one ALTER statement. It does not block writes, but it cannot be throttled, paused, resumed, or stopped cheaply: cancelling it rolls the whole rebuild back",
					strings.Join(kinds, ", "), humanCount(in.Table.Rows), humanBytes(in.Table.DataLength)),
				Remediation: "run it in a quiet window, or switch to an engine that can be paused: `saksama engine set gh-ost` (or `pt-osc`) and plan again if you need throttling, a pause, or a lag cap",
			})
		}
	}

	if rec.Engine == EngineGhOst && in.Server.IsManaged() {
		out = append(out, Check{
			ID: "R5", Severity: SeverityWarn,
			Title:       "managed server may refuse gh-ost",
			Detail:      "gh-ost needs privileges a managed service may not grant",
			Remediation: "ask for rds_superuser on RDS, or run the copy with the other engine: `saksama engine set pt-osc`. Managed services rarely grant what gh-ost needs, and pt-osc asks for less",
		})
	}

	if rec.Engine == EngineGhOst && in.Live != nil && !in.Live.ReplicaConfigured {
		out = append(out, Check{
			ID: "R6", Severity: SeverityWarn,
			Title:       "no replica: the copy load lands on the primary",
			Detail:      "gh-ost will run with --allow-on-master",
			Remediation: "gh-ost copies from the primary unless a replica exists: either run it in a quiet window, or point the copy at a replica with `gh-ost --host=<replica>` once one exists. Nothing in this plan changes where the load lands",
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

// rebuildKinds names the operations that rewrite rows, for the sentences that
// have to state the cost.
func rebuildKinds(in Input) []string {
	var out []string
	for _, op := range in.Ops {
		if c := capabilityFor(op, in.Server, in.Table); c.Rebuild {
			out = append(out, string(op.Kind))
		}
	}
	return dedupe(out)
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

	if engine == EngineNative && allInstant(in.Ops) {
		return DurationEstimate{
			FromSeconds: 1, ToSeconds: 2, Confidence: "high",
			Basis: "INSTANT operations only modify metadata; the table is not scanned or rebuilt",
		}
	}

	// Measured beats assumed. This machine has run migrations before, and what it
	// actually achieved is a better input than a throughput picked out of the air -
	// especially because the assumption below spans a factor of ten.
	if measured, ok := measuredDuration(in.History, engine, size); ok {
		return measured
	}

	note := singleObservationNote(in.History, engine)
	// The count is reported even when it is not enough to build a range: a plan that
	// quotes one measurement and says nothing about it in JSON would be hiding the
	// only evidence it has.
	available := usableObservations(in.History, engine)
	var est DurationEstimate
	switch engine {
	case EngineNative:
		from := int(size / (100 << 20))
		to := int(size / (20 << 20))
		if from < 2 {
			from = 2
		}
		if to < from {
			to = from
		}
		est = DurationEstimate{
			FromSeconds: from, ToSeconds: to, Confidence: "medium",
			Basis: "in-place index build scans and sorts the table; throughput assumed between 20 and 100 MB/s",
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
		est = DurationEstimate{
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
		est = DurationEstimate{
			FromSeconds: from, ToSeconds: to, Confidence: "low",
			Basis: "row-by-row copy driven by triggers; throughput assumed between 2 and 15 MB/s and actual speed depends on load and throttling",
		}
	}
	est.Observations = available
	if note != "" {
		est.Basis += "; " + note
	}
	if est.FromSeconds == 0 && est.ToSeconds == 0 {
		return DurationEstimate{
			FromSeconds: 0, ToSeconds: 0, Confidence: "high",
			Basis: "no execution is planned",
		}
	}
	return est
}

// measuredDuration turns earlier runs on this machine into a range.
//
// Two runs is the floor: one number is a point, not a range, and a range invented
// around a single sample would be an assumption wearing a measurement's clothes.
// The throughput is the transferable quantity - the same machine copying 40 GB and
// 400 GB does not scale by table size alone, but bytes per second does.
func measuredDuration(history []Observation, engine Engine, size int64) (DurationEstimate, bool) {
	var slowest, fastest int64 // bytes per second, slowest first
	used := 0
	for _, o := range history {
		if o.Engine != engine || o.DataBytes <= 0 || o.Seconds <= 0 {
			continue
		}
		tp := o.DataBytes / int64(o.Seconds)
		if tp <= 0 {
			continue
		}
		if used == 0 || tp < slowest {
			slowest = tp
		}
		if tp > fastest {
			fastest = tp
		}
		used++
	}
	if used < 2 || slowest <= 0 || fastest <= 0 {
		return DurationEstimate{}, false
	}

	from := secondsAt(size, fastest)
	to := secondsAt(size, slowest)
	if to <= from {
		to = from + 1
	}

	basis := fmt.Sprintf(
		"measured on this machine from %d earlier %s run(s): %s/s to %s/s, so this table is %s to %s",
		used, engine, humanBytes(slowest), humanBytes(fastest),
		HumanDuration(from), HumanDuration(to))
	if spread := fastest / slowest; spread >= 4 {
		basis += fmt.Sprintf("; the spread is wide (%dx), so treat it as a hint rather than a schedule", spread)
	}

	return DurationEstimate{
		FromSeconds: from, ToSeconds: to, Confidence: "medium", Basis: basis,
		Observations: used,
	}, true
}

// usableObservations counts the earlier runs this estimate could have used: same
// engine, a size, and a duration. It is what the plan reports as its evidence.
func usableObservations(history []Observation, engine Engine) int {
	n := 0
	for _, o := range history {
		if o.Engine == engine && o.DataBytes > 0 && o.Seconds > 0 {
			n++
		}
	}
	return n
}

// singleObservationNote is what a lone earlier run is worth: a data point, not a
// range. It goes into the assumed basis so the number on screen has a measurement
// next to it without pretending to be one.
func singleObservationNote(history []Observation, engine Engine) string {
	used, best := 0, int64(0)
	for _, o := range history {
		if o.Engine != engine || o.DataBytes <= 0 || o.Seconds <= 0 {
			continue
		}
		tp := o.DataBytes / int64(o.Seconds)
		if tp > 0 {
			used++
			best = tp
		}
	}
	if used != 1 {
		return ""
	}
	return fmt.Sprintf("%s/s was measured once on this machine; one run is a point, not a range", humanBytes(best))
}

func secondsAt(size, bytesPerSecond int64) int {
	if bytesPerSecond <= 0 {
		return 0
	}
	sec := size / bytesPerSecond
	if rem := size % bytesPerSecond; rem != 0 {
		sec++
	}
	if sec < 1 {
		sec = 1
	}
	return int(sec)
}

// HumanDuration is how long a range endpoint reads on screen. It is exported so
// the renderer can use it too: the basis line and the DURATION line describe the
// same range, and describing it two ways invites a reader to distrust both.
func HumanDuration(sec int) string {
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	if sec < 3600 {
		return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
	}
	return fmt.Sprintf("%dh%02dm", sec/3600, (sec%3600)/60)
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
