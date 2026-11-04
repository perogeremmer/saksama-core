package analyzer

import (
	"fmt"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// Verdict is how an execution path scored for one specific change.
type Verdict string

const (
	// VerdictSuitable means every requirement is met and this build can run it.
	VerdictSuitable Verdict = "suitable"
	// VerdictBlocked means a hard requirement is not met.
	VerdictBlocked Verdict = "blocked"
	// VerdictNotExecutable means the engine fits the change but this build cannot
	// run it yet. That distinction matters: the honest answer is "pt-osc is the
	// right tool here, and this version cannot drive it".
	VerdictNotExecutable Verdict = "not_executable"
)

// Requirement is one precondition for an engine.
type Requirement struct {
	Name     string `json:"name"`
	Met      bool   `json:"met"`
	Blocking bool   `json:"blocking"`
	Detail   string `json:"detail"`
}

// EngineAssessment is the per-engine answer to "which tool fits this table?".
type EngineAssessment struct {
	Engine       Engine        `json:"engine"`
	Verdict      Verdict       `json:"verdict"`
	Summary      string        `json:"summary"`
	Requirements []Requirement `json:"requirements"`
	Notes        []string      `json:"notes,omitempty"`
	// Executable reports whether this build can actually run the engine.
	Executable bool `json:"executable"`
	// BlockedBy lists the requirement names that decided the verdict.
	BlockedBy []string `json:"blocked_by,omitempty"`
}

// EngineBinary is a locally discovered engine binary, injected so the analyzer
// stays free of process execution.
type EngineBinary struct {
	Name      string
	Path      string
	Version   string
	Available bool
	Error     string
}

// AssessEngines evaluates every execution path against one concrete change.
//
// This is the "which tool should I use?" answer. It is deliberately separate from
// route(): route picks the path the tool will actually take, while AssessEngines
// explains every path, including the ones this build cannot execute.
func AssessEngines(in Input, projected model.Table) []EngineAssessment {
	return []EngineAssessment{
		assessNative(in, projected),
		assessGhOst(in, projected),
		assessPtOSC(in, projected),
	}
}

func newAssessment(e Engine, summary string) EngineAssessment {
	return EngineAssessment{Engine: e, Summary: summary, Requirements: nil}
}

func (a *EngineAssessment) require(name string, met, blocking bool, detail string) {
	a.Requirements = append(a.Requirements, Requirement{
		Name: name, Met: met, Blocking: blocking, Detail: detail,
	})
	if !met && blocking {
		a.BlockedBy = append(a.BlockedBy, name)
	}
}

func (a *EngineAssessment) finalize(executable bool) EngineAssessment {
	a.Executable = executable
	switch {
	case len(a.BlockedBy) > 0:
		a.Verdict = VerdictBlocked
	case !executable:
		a.Verdict = VerdictNotExecutable
	default:
		a.Verdict = VerdictSuitable
	}
	return *a
}

// --- native -----------------------------------------------------------------

func assessNative(in Input, projected model.Table) EngineAssessment {
	a := newAssessment(EngineNative,
		"plain ALTER TABLE, executed by MySQL itself with an explicit ALGORITHM and LOCK")

	caps, _ := capabilitySummary(in)

	// Every operation must be proven native-capable.
	unsupported := []string{}
	rebuild := []string{}
	for _, op := range in.Ops {
		c := capabilityFor(op, in.Server, in.Table)
		switch {
		case c.Blocked:
			unsupported = append(unsupported, string(op.Kind))
		case c.Rebuild:
			rebuild = append(rebuild, string(op.Kind))
		}
	}
	_ = caps

	if len(in.Ops) == 0 {
		a.require("understood operations", false, true, "no ALTER operation was recognised")
	} else {
		a.require("understood operations", len(unsupported) == 0, true,
			iff(len(unsupported) == 0, "all operations are recognised",
				"unsupported: "+strings.Join(unsupported, ", ")))
	}
	a.require("no table copy required", len(rebuild) == 0, true,
		iff(len(rebuild) == 0, "the server can do this in place or instantly",
			"these operations rebuild the table: "+strings.Join(rebuild, ", ")))

	// MySQL refuses DDL that touches a column constrained by a foreign key.
	touched := touchedColumns(in.Ops)
	var fkHits []string
	for _, fk := range in.Table.AllForeignKeys() {
		for _, col := range append(append([]string{}, fk.Columns...), fk.RefColumns...) {
			if touched[strings.ToLower(col)] {
				fkHits = append(fkHits, fmt.Sprintf("%s.%s", fk.Name, col))
			}
		}
	}
	a.require("no foreign-key column touched", len(fkHits) == 0, true,
		iff(len(fkHits) == 0, "no constrained column is modified",
			"MySQL will refuse this (1832): "+strings.Join(dedupe(fkHits), ", ")))

	if hasMethod := anyMethod(in, MethodInstant); hasMethod {
		a.Notes = append(a.Notes, "at least one change is INSTANT: metadata only, no table scan")
	}

	return a.finalize(true)
}

// --- gh-ost -----------------------------------------------------------------

func assessGhOst(in Input, projected model.Table) EngineAssessment {
	a := newAssessment(EngineGhOst,
		"row-by-row copy through a ghost table with triggers, then an atomic swap")

	v, verr := model.ParseVersion(in.Server.Version)
	if verr != nil {
		a.require("server version known", false, true, "cannot parse "+in.Server.Version)
	} else {
		a.require("MySQL 5.7 or newer", v.AtLeast(5, 7, 0), true,
			iff(v.AtLeast(5, 7, 0), "server is "+v.String(), "gh-ost requires MySQL 5.7+"))
	}

	a.require("InnoDB storage engine", strings.EqualFold(projected.Engine, "InnoDB"), true,
		iff(strings.EqualFold(projected.Engine, "InnoDB"), "table is InnoDB",
			"table engine is "+projected.Engine))

	// This is the requirement that decides the question most often.
	fks := projected.AllForeignKeys()
	a.require("no foreign keys in or out", len(fks) == 0, true,
		iff(len(fks) == 0, "no constraint involves this table",
			fmt.Sprintf("%d constraint(s) involve this table; gh-ost does not support foreign keys at all", len(fks))))

	pk, hasPK := projected.PrimaryKey()
	switch {
	case hasPK && len(pk.Parts) == 1:
		a.require("single-column primary key", true, true,
			fmt.Sprintf("primary key is %s", pk.Parts[0].Column))
	case hasPK && len(pk.Parts) > 1:
		a.require("single-column primary key", false, true,
			fmt.Sprintf("primary key has %d columns; gh-ost chunks by a single column", len(pk.Parts)))
	default:
		a.require("single-column primary key", false, true,
			"the table has no primary key; gh-ost needs one to chunk safely")
	}

	a.require("table is not partitioned", !projected.Partitioned, true,
		iff(!projected.Partitioned, "table is not partitioned",
			"gh-ost does not support partitioned tables"))

	if bin := binaryFor(in, "gh-ost"); bin != nil {
		a.require("gh-ost binary installed", bin.Available, true,
			iff(bin.Available, "found "+bin.Path+" ("+bin.Version+")",
				"not installed: "+fallback(bin.Error, "not found on PATH")))
	} else {
		a.Notes = append(a.Notes, "installed engine binaries were not probed (--no-probe)")
	}

	if !in.Table.TriggersAny() {
		a.Notes = append(a.Notes, "no existing triggers; gh-ost's triggers will not collide")
	} else {
		a.Notes = append(a.Notes, "table already has triggers; gh-ost creates three of its own, so review the names")
	}

	if in.Live != nil && !in.Live.ReplicaConfigured {
		a.Notes = append(a.Notes, "no replica detected: gh-ost must run with --allow-on-master, which puts the copy load on the primary")
	}
	if in.Server.IsManaged() {
		a.Notes = append(a.Notes, "managed server: gh-ost may need rds_superuser-style privileges")
	}
	a.Notes = append(a.Notes,
		fmt.Sprintf("estimated disk: about %s extra for the ghost table plus the retained old table", humanBytes(in.Table.SizeBytes()*2)))

	// The adapter does not exist yet, so the tool must say that plainly.
	return a.finalize(false)
}

// --- pt-osc ----------------------------------------------------------------

func assessPtOSC(in Input, projected model.Table) EngineAssessment {
	a := newAssessment(EnginePtOSC,
		"Percona Toolkit's pt-online-schema-change: triggers plus a shadow table, then RENAME swap")

	a.require("MySQL 5.1 or newer", true, true, "pt-osc supports every version Saksama plans for")
	a.require("InnoDB storage engine", strings.EqualFold(projected.Engine, "InnoDB"), true,
		iff(strings.EqualFold(projected.Engine, "InnoDB"), "table is InnoDB", "table engine is "+projected.Engine))

	_, hasUnique := anyUniqueKey(projected)
	a.require("primary or unique key", hasUnique, true,
		iff(hasUnique, "the table can be chunked", "pt-osc requires a primary or unique key"))

	if pk, ok := projected.PrimaryKey(); ok && len(pk.Parts) > 1 {
		a.Notes = append(a.Notes, fmt.Sprintf(
			"composite primary key (%d columns) is supported by pt-osc; this is its main advantage over gh-ost", len(pk.Parts)))
	}

	outgoing := len(projected.ForeignKeys)
	incoming := len(projected.ReferencedBy)
	if outgoing > 0 {
		a.Notes = append(a.Notes,
			fmt.Sprintf("%d outgoing foreign key(s): pt-osc 3.x handles these natively when copying", outgoing))
	}
	if incoming > 0 {
		a.Notes = append(a.Notes,
			fmt.Sprintf("%d incoming foreign key(s): pt-osc needs --alter-foreign-keys-method, and the window is not free", incoming))
	}

	if bin := binaryFor(in, "pt-online-schema-change"); bin != nil {
		a.require("pt-osc binary installed", bin.Available, true,
			iff(bin.Available, "found "+bin.Path+" ("+bin.Version+")",
				"not installed: "+fallback(bin.Error, "not found on PATH")+"; install the percona-toolkit package"))
		if bin.Available && strings.HasPrefix(bin.Version, "2.") {
			a.Notes = append(a.Notes,
				"Percona Toolkit 2.x: no --resume support and a narrower ALTER grammar than 3.x")
		}
	} else {
		a.Notes = append(a.Notes, "installed engine binaries were not probed (--no-probe)")
	}

	if in.Table.TriggersAny() {
		a.Notes = append(a.Notes,
			"table already has triggers; pt-osc refuses when a trigger with its own name exists")
	}
	a.Notes = append(a.Notes,
		"licensing: pt-osc is GPL-2, so it is never bundled — it is only invoked as an external binary")

	return a.finalize(false)
}

// --- helpers ---------------------------------------------------------------

func binaryFor(in Input, name string) *EngineBinary {
	if in.Binaries == nil {
		return nil
	}
	if b, ok := in.Binaries[name]; ok {
		return &b
	}
	return nil
}

func anyUniqueKey(t model.Table) (primary bool, unique bool) {
	for _, ix := range t.Indexes {
		if ix.Primary {
			primary = true
		}
		if ix.Unique {
			unique = true
		}
	}
	return primary, primary || unique
}

func anyMethod(in Input, m Method) bool {
	for _, op := range in.Ops {
		if capabilityFor(op, in.Server, in.Table).Method == m {
			return true
		}
	}
	return false
}

func capabilitySummary(in Input) (map[string]Capability, error) {
	out := map[string]Capability{}
	for _, op := range in.Ops {
		out[op.Raw] = capabilityFor(op, in.Server, in.Table)
	}
	return out, nil
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func iff(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
