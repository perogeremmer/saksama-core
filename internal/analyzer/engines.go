package analyzer

import (
	"fmt"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// Verdict answers one narrow question: can this engine do this job at all?
//
// Verdict deliberately does NOT mix in "do we have an adapter" or "is the binary
// installed". Those are separate facts (Wrapped, Installed) because conflating an
// engine's limitation with our own is how people end up blaming their schema for
// a missing feature.
type Verdict string

const (
	VerdictSuitable   Verdict = "suitable"
	VerdictUnsuitable Verdict = "unsuitable"
)

// Requirement is one precondition the chosen tool needs.
type Requirement struct {
	Name   string `json:"name"`
	Met    bool   `json:"met"`
	Detail string `json:"detail"`
}

// Evidence is a schema fact and why it matters for this engine.
//
// This is the core of the advice: the user asked "look at my column types and
// tell me what fits", so every conclusion must be traceable to an observed fact.
type Evidence struct {
	Fact   string `json:"fact"`
	Effect string `json:"effect"`
}

// EngineAssessment is the per-engine verdict, with the reasoning made explicit.
type EngineAssessment struct {
	Engine       Engine        `json:"engine"`
	Verdict      Verdict       `json:"verdict"`
	Summary      string        `json:"summary"`
	Requirements []Requirement `json:"requirements"`
	Evidence     []Evidence    `json:"evidence"`
	Pros         []string      `json:"pros,omitempty"`
	Cons         []string      `json:"cons,omitempty"`
	// Command is a ready-to-run invocation for this table. Secrets are never
	// included: both engines can prompt.
	Command string `json:"command,omitempty"`
	// Steps are what the user does after (or instead of) running the command.
	Steps []string `json:"steps,omitempty"`
	// Environment lists the server prerequisites, with what is actually set.
	Environment []Requirement `json:"environment,omitempty"`
	// Installed reports whether the engine binary exists on this machine.
	Installed bool `json:"installed"`
	// Wrapped reports whether this build can drive the engine itself.
	Wrapped bool `json:"wrapped"`
	// Install is the hint shown when the binary is missing locally.
	Install string   `json:"install,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

// Runnable reports whether Saksama can execute this engine for this change.
//
// Three independent things must hold: the engine must be able to do this change,
// the binary must exist, and the server must be configured for it. They are
// reported separately so a fix is obvious.
func (x EngineAssessment) Runnable() bool {
	if x.Verdict != VerdictSuitable || !x.Installed || !x.Wrapped {
		return false
	}
	return x.EnvironmentReady()
}

// EnvironmentReady reports whether every server-side prerequisite is met.
func (x EngineAssessment) EnvironmentReady() bool {
	for _, r := range x.Environment {
		if !r.Met {
			return false
		}
	}
	return true
}

// EnvironmentProblems names the unmet server prerequisites.
func (x EngineAssessment) EnvironmentProblems() []string {
	var out []string
	for _, r := range x.Environment {
		if !r.Met {
			out = append(out, r.Name)
		}
	}
	return out
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

// Install hints point at Saksama's own installer rather than at a URL, because
// the URL is not the hard part: choosing the right asset for this platform,
// verifying it, and putting it where this build will find it is.
//
// internal/enginesetup owns the actions and the checksums. These strings are what
// a plan or a failure message shows, and they must name a command that exists.
const (
	GhOstInstall = "saksama engine install gh-ost"
	PtOSCInstall = "saksama engine install pt-osc"
)

// AdviceInput is everything the engine matrix needs.
type AdviceInput struct {
	Input     Input
	Projected model.Table
	// User and Port appear in generated commands.
	User string
	Port string
	Host string
}

// AssessEngines answers "which tool fits this table, and why".
func AssessEngines(in Input, projected model.Table) []EngineAssessment {
	a := AdviceInput{Input: in, Projected: projected, User: in.DBUser, Port: in.DBPort, Host: in.Host}
	return []EngineAssessment{
		a.assessNative(),
		a.assessGhOst(),
		a.assessPtOSC(),
	}
}

// --- shared helpers --------------------------------------------------------

func (a *AdviceInput) schemaFacts() []Evidence {
	t := a.Projected
	var out []Evidence

	if pk, ok := t.PrimaryKey(); ok {
		switch {
		case len(pk.Parts) == 1:
			col, _ := t.Column(pk.Parts[0].Column)
			fact := fmt.Sprintf("primary key is a single column: %s %s", col.Name, col.Type)
			out = append(out, Evidence{Fact: fact, Effect: "both copy engines can chunk by range on it"})
			if col.IsUUIDLike() {
				out = append(out, Evidence{
					Fact:   fmt.Sprintf("primary key %s looks like a random UUID (%s)", col.Name, col.Type),
					Effect: "range chunks cover scattered index pages, so copy throughput is lower and less predictable than an auto-increment key",
				})
			}
			if strings.Contains(strings.ToLower(col.Extra), "auto_increment") {
				out = append(out, Evidence{
					Fact:   fmt.Sprintf("primary key %s is auto_increment", col.Name),
					Effect: "monotonic: the cheapest possible chunking for a copy engine",
				})
			}
		case len(pk.Parts) > 1:
			names := make([]string, 0, len(pk.Parts))
			for _, p := range pk.Parts {
				names = append(names, p.Column)
			}
			out = append(out, Evidence{
				Fact:   fmt.Sprintf("composite primary key (%s)", strings.Join(names, ", ")),
				Effect: "gh-ost chunks by a single column and cannot use this; pt-osc can",
			})
		}
	} else {
		out = append(out, Evidence{
			Fact:   "no primary key",
			Effect: "neither gh-ost nor pt-osc can chunk this table safely; add a key first",
		})
	}

	if fks := t.ForeignKeys; len(fks) > 0 {
		var desc []string
		for _, fk := range fks {
			d := fmt.Sprintf("%s(%s) -> %s", fk.Name, strings.Join(fk.Columns, ","), fk.RefTable)
			if fk.HasAction() {
				d += fmt.Sprintf(" on delete %s/on update %s", fk.OnDelete, fk.OnUpdate)
			}
			desc = append(desc, d)
		}
		out = append(out, Evidence{
			Fact:   fmt.Sprintf("%d outgoing foreign key(s): %s", len(fks), strings.Join(desc, "; ")),
			Effect: "gh-ost does not support foreign keys at all; pt-osc 3.x copies them",
		})
	}
	if fks := t.ReferencedBy; len(fks) > 0 {
		var desc []string
		for _, fk := range fks {
			desc = append(desc, fmt.Sprintf("%s.%s", fk.RefTable, strings.Join(fk.Columns, ",")))
		}
		out = append(out, Evidence{
			Fact:   fmt.Sprintf("%d incoming foreign key(s) from: %s", len(fks), strings.Join(desc, "; ")),
			Effect: "pt-osc needs --alter-foreign-keys-method; gh-ost cannot do this at all",
		})
	}

	if t.Partitioned {
		out = append(out, Evidence{
			Fact:   "table is partitioned",
			Effect: "gh-ost does not support partitioned tables; pt-osc does",
		})
	}

	if len(t.Triggers) > 0 {
		out = append(out, Evidence{
			Fact:   fmt.Sprintf("%d existing trigger(s): %s", len(t.Triggers), strings.Join(t.Triggers, ", ")),
			Effect: "both copy engines create their own triggers; existing ones must be free of name conflicts",
		})
	}

	var blobCols, generatedCols, jsonCols, mb3Cols []string
	for _, c := range t.Columns {
		switch {
		case c.IsBlobLike():
			blobCols = append(blobCols, c.Name)
		}
		if c.Generated {
			generatedCols = append(generatedCols, c.Name)
		}
		if strings.EqualFold(c.DataType, "json") {
			jsonCols = append(jsonCols, c.Name)
		}
		if strings.EqualFold(c.Charset, "utf8") || strings.EqualFold(c.Charset, "utf8mb3") {
			mb3Cols = append(mb3Cols, c.Name)
		}
	}
	if len(blobCols) > 0 {
		out = append(out, Evidence{
			Fact:   fmt.Sprintf("%d large column(s): %s", len(blobCols), strings.Join(blobCols, ", ")),
			Effect: "row-by-row copy moves these too, so total copy time tracks table size rather than row count",
		})
	}
	if len(generatedCols) > 0 {
		out = append(out, Evidence{
			Fact:   fmt.Sprintf("generated column(s): %s", strings.Join(generatedCols, ", ")),
			Effect: "generated values are recomputed on insert; both engines handle this, but verify the expression still resolves",
		})
	}
	if len(jsonCols) > 0 {
		out = append(out, Evidence{
			Fact:   fmt.Sprintf("JSON column(s): %s", strings.Join(jsonCols, ", ")),
			Effect: "JSON cannot be indexed directly; no effect on the copy itself",
		})
	}
	if len(mb3Cols) > 0 {
		out = append(out, Evidence{
			Fact:   fmt.Sprintf("utf8mb3 column(s): %s", strings.Join(mb3Cols, ", ")),
			Effect: "deprecated charset; a copy preserves it, so plan the utf8mb4 conversion separately",
		})
	}

	if size := t.SizeBytes(); size > 0 {
		out = append(out, Evidence{
			Fact:   fmt.Sprintf("table size %s across %d column(s)", humanBytes(size), len(t.Columns)),
			Effect: fmt.Sprintf("a copy-based engine needs roughly %s of extra disk and reads the whole table once", humanBytes(size*2)),
		})
	}

	return out
}

func (a *AdviceInput) newAssessment(e Engine, summary string) EngineAssessment {
	return EngineAssessment{Engine: e, Summary: summary}
}

func (x *EngineAssessment) require(name string, met bool, detail string) {
	x.Requirements = append(x.Requirements, Requirement{Name: name, Met: met, Detail: detail})
}

func (x *EngineAssessment) finalize() EngineAssessment {
	for _, r := range x.Requirements {
		if !r.Met {
			x.Verdict = VerdictUnsuitable
			return *x
		}
	}
	x.Verdict = VerdictSuitable
	return *x
}

func (x *EngineAssessment) binary(in Input, name, install string) *EngineBinary {
	x.Install = install
	b, ok := in.Binaries[name]
	if !ok {
		// Not probed (--no-probe). Assume the common case rather than pretending
		// the engine is unavailable.
		x.Installed = true
		return nil
	}
	x.Installed = b.Available
	if !b.Available {
		// The probe's own words when it has them: "not installed" and "installed
		// but cannot start" are different problems with different fixes.
		detail := b.Error
		if detail == "" {
			detail = fmt.Sprintf("%s is not installed on this machine", name)
		}
		x.Notes = append(x.Notes, detail)
	}
	return &b
}

// --- native ----------------------------------------------------------------

func (a *AdviceInput) assessNative() EngineAssessment {
	in := a.Input
	x := a.newAssessment(EngineNative, "run ALTER TABLE directly and let MySQL do it in place")
	x.Installed = true // the server is the engine; nothing to install
	x.Wrapped = true
	x.Environment = EngineEnvironment(a.Input.Settings, EngineNative)

	for _, op := range in.Ops {
		c := capabilityFor(op, in.Server, in.Table)
		x.Evidence = append(x.Evidence, Evidence{
			Fact:   fmt.Sprintf("%s -> %s %s", op.Kind, c.Method, c.Lock),
			Effect: c.Reason,
		})
	}

	var rebuild, blocked []string
	for _, op := range in.Ops {
		c := capabilityFor(op, in.Server, in.Table)
		switch {
		case c.Blocked:
			blocked = append(blocked, string(op.Kind))
		case c.Rebuild:
			rebuild = append(rebuild, string(op.Kind))
		}
	}

	x.require("operation is understood",
		len(in.Ops) > 0 && len(blocked) == 0,
		iff(len(blocked) == 0 && len(in.Ops) > 0, "every operation has a verified rule",
			"not supported: "+strings.Join(append(blocked, "unknown"), ", ")))
	x.require("no table rebuild required",
		len(rebuild) == 0,
		iff(len(rebuild) == 0, "MySQL can do this in place or instantly",
			"these operations rebuild the table: "+strings.Join(rebuild, ", ")))

	touched := touchedColumns(in.Ops)
	var fkHits []string
	for _, fk := range a.Projected.AllForeignKeys() {
		for _, col := range append(append([]string{}, fk.Columns...), fk.RefColumns...) {
			if touched[strings.ToLower(col)] {
				fkHits = append(fkHits, fmt.Sprintf("%s.%s", fk.Name, col))
			}
		}
	}
	x.require("no foreign-key column is modified",
		len(fkHits) == 0,
		iff(len(fkHits) == 0, "no constrained column is touched",
			"MySQL refuses this (1832): "+strings.Join(dedupe(fkHits), ", ")))

	x.Pros = append(x.Pros,
		"no copy and no shadow table when the rule says INPLACE or INSTANT",
		"shortest possible operation: nothing to clean up if it fails",
		"no extra disk, no trigger overhead, no replica needed")
	x.Cons = append(x.Cons,
		"still needs the metadata lock at the end, so one long query can queue the whole server",
		"only works for operations MySQL can do in place; a type change or a charset conversion is not one of them")

	if x.Verdict = verdictOf(&x); x.Verdict == VerdictSuitable {
		x.Command = a.nativeSQL()
	}
	if x.Verdict == VerdictSuitable && x.Command != "" {
		x.Steps = []string{
			"dry run first:  saksama apply " + a.targetFlags() + " --dry-run",
			"then apply:     saksama apply " + a.targetFlags(),
		}
	}

	return x.finalize()
}

func (a *AdviceInput) nativeSQL() string {
	if len(a.Input.Ops) == 0 {
		return ""
	}
	var clauses []string
	for _, op := range a.Input.Ops {
		clauses = append(clauses, strings.TrimSpace(op.Raw))
	}
	target := "`" + a.Projected.Name + "`"
	if a.Projected.Schema != "" {
		target = "`" + a.Projected.Schema + "`." + target
	}
	return fmt.Sprintf("ALTER TABLE %s %s;", target, strings.Join(clauses, ", "))
}

func (a *AdviceInput) targetFlags() string {
	parts := []string{}
	if a.Host != "" {
		parts = append(parts, "-h "+a.Host)
	}
	if a.Projected.Schema != "" {
		parts = append(parts, "-d "+a.Projected.Schema)
	}
	if a.Projected.Name != "" {
		parts = append(parts, "-t "+a.Projected.Name)
	}
	parts = append(parts, `-a "`+strings.Join(opRaw(a.Input.Ops), ", ")+`"`)
	return strings.Join(parts, " ")
}

func opRaw(ops []model.Op) []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, strings.TrimSpace(op.Raw))
	}
	return out
}

// --- gh-ost ----------------------------------------------------------------

func (a *AdviceInput) assessGhOst() EngineAssessment {
	in, t := a.Input, a.Projected
	x := a.newAssessment(EngineGhOst, "GitHub's gh-ost: copy into a ghost table, cut over atomically")
	x.Wrapped = true
	x.Environment = EngineEnvironment(a.Input.Settings, EngineGhOst)

	if v, err := model.ParseVersion(in.Server.Version); err != nil {
		x.require("server version is known", false, "cannot parse "+in.Server.Version)
	} else {
		x.require("MySQL 5.7 or newer", v.AtLeast(5, 7, 0),
			iff(v.AtLeast(5, 7, 0), "server is "+v.String(), "gh-ost needs binlog_row_image and a 5.7+ server"))
	}

	x.require("InnoDB storage engine", strings.EqualFold(t.Engine, "InnoDB"),
		iff(strings.EqualFold(t.Engine, "InnoDB"), "table is InnoDB", "table engine is "+t.Engine))

	fks := t.AllForeignKeys()
	x.require("no foreign keys in or out", len(fks) == 0,
		iff(len(fks) == 0, "no constraint involves this table",
			fmt.Sprintf("%d constraint(s) involve this table; gh-ost has no foreign key support", len(fks))))

	pk, hasPK := t.PrimaryKey()
	switch {
	case hasPK && len(pk.Parts) == 1:
		col, _ := t.Column(pk.Parts[0].Column)
		x.require("single-column key to chunk by", true,
			"primary key is "+col.Name+" ("+col.Type+")")
		if col.IsUUIDLike() {
			x.Cons = append(x.Cons,
				fmt.Sprintf("primary key %s is a random UUID: gh-ost will scan scattered index ranges, so expect a much slower copy", col.Name))
		}
	default:
		x.require("single-column key to chunk by", false,
			iff(!hasPK, "the table has no primary key", fmt.Sprintf("the primary key has %d columns; gh-ost chunks by one", len(pk.Parts))))
	}

	x.require("not partitioned", !t.Partitioned,
		iff(!t.Partitioned, "table is not partitioned", "gh-ost does not support partitioned tables"))

	// gh-ost needs row-based binlog for the copy to be consistent.
	x.require("row-based binlog", true, "gh-ost verifies binlog_format=ROW at startup and stops if it is not")

	bin := x.binary(in, "gh-ost", "install: "+GhOstInstall)
	if bin != nil && bin.Available {
		x.Notes = append(x.Notes, "found locally: "+bin.Path+" ("+bin.Version+")")
	}

	if in.Live != nil && !in.Live.ReplicaConfigured {
		x.Notes = append(x.Notes,
			"no replica detected: add --allow-on-master, and accept that the copy load lands on the primary")
	}

	x.Pros = append(x.Pros,
		"single static binary, MIT licensed, no Perl",
		"can postpone the cut-over so you verify the ghost table before the swap",
		"throttles on replica lag as well as server load",
		"can read the copy from a replica, keeping the primary free")
	x.Cons = append(x.Cons,
		"cannot do foreign keys at all, in either direction",
		"chunks by a single column, so a composite primary key is out",
		"no support for partitioned tables",
		"random UUID keys make the copy slow and its ETA unreliable")

	if x.Verdict = verdictOf(&x); x.Verdict == VerdictSuitable {
		x.Command = a.ghOstCommand()
		x.Steps = []string{
			"check it will do what you think: add --dry-run to the command above",
			"then replace --dry-run with --execute",
			"run with --hold-cutover, so the swap waits for you",
			"verify the ghost table before the cut-over; after the swap there is no data rollback",
		}
	}

	return x.finalize()
}

func (a *AdviceInput) ghOstCommand() string {
	alter := strings.Join(opRaw(a.Input.Ops), ", ")
	user := a.User
	if user == "" {
		user = "root"
	}
	port := a.Port
	if port == "" {
		port = "3306"
	}
	var b strings.Builder
	b.WriteString("gh-ost \\\n")
	fmt.Fprintf(&b, "  --host=%s --port=%s --user=%s --ask-pass \\\n", a.Host, port, user)
	fmt.Fprintf(&b, "  --database=%s --table=%s \\\n", a.Projected.Schema, a.Projected.Name)
	fmt.Fprintf(&b, "  --alter=%q \\\n", alter)
	b.WriteString("  --max-load=Threads_running=50 --critical-load=Threads_running=100 \\\n")
	if a.Input.Live == nil || !a.Input.Live.ReplicaConfigured {
		b.WriteString("  --allow-on-master \\\n")
	}
	b.WriteString("  --dry-run")
	return b.String()
}

// --- pt-osc ----------------------------------------------------------------

func (a *AdviceInput) assessPtOSC() EngineAssessment {
	in, t := a.Input, a.Projected
	x := a.newAssessment(EnginePtOSC, "Percona Toolkit's pt-online-schema-change: triggers plus a shadow table")
	x.Wrapped = true
	x.Environment = EngineEnvironment(a.Input.Settings, EnginePtOSC)

	x.require("InnoDB storage engine", strings.EqualFold(t.Engine, "InnoDB"),
		iff(strings.EqualFold(t.Engine, "InnoDB"), "table is InnoDB", "table engine is "+t.Engine))

	_, hasKey := anyUniqueKey(t)
	x.require("primary or unique key to chunk by", hasKey,
		iff(hasKey, "the table can be chunked by its key", "pt-osc refuses tables without a primary or unique key"))

	if v, err := model.ParseVersion(in.Server.Version); err == nil {
		x.require("MySQL 5.1 or newer", v.AtLeast(5, 1, 0), "server is "+v.String())
	}

	// pt-osc 3.x copies foreign keys on the altered table; incoming constraints
	// need an explicit method.
	if len(t.ReferencedBy) > 0 {
		x.require("incoming foreign keys handled explicitly", true,
			fmt.Sprintf("%d incoming constraint(s): add --alter-foreign-keys-method=auto", len(t.ReferencedBy)))
	}

	bin := x.binary(in, "pt-online-schema-change", "install: "+PtOSCInstall)
	if bin != nil && bin.Available {
		x.Notes = append(x.Notes, "found locally: "+bin.Path+" ("+bin.Version+")")
		if strings.HasPrefix(bin.Version, "2.") {
			x.Cons = append(x.Cons, "version 2.x installed: narrower ALTER grammar and no --resume")
		}
	}

	x.Pros = append(x.Pros,
		"handles composite primary keys",
		"copies foreign keys on the altered table natively",
		"supports partitioned tables and older MySQL versions",
		"mature: the widest ALTER grammar of the three")
	x.Cons = append(x.Cons,
		"needs Perl and the DBI/DBD::mysql modules",
		"GPL-2: fine to install, restrictive if you redistribute it",
		"chunking by triggers means write overhead for the whole copy",
		"no built-in cut-over gate; the swap happens as soon as the copy finishes",
		"incoming foreign keys need --alter-foreign-keys-method, and that window is not free")

	if x.Verdict = verdictOf(&x); x.Verdict == VerdictSuitable {
		x.Command = a.ptOSCCommand()
		x.Steps = []string{
			"review the statements first: the command ends in --print, which only prints",
			"then swap --print for --execute",
			"if you have incoming foreign keys, keep --alter-foreign-keys-method=auto and read Percona's notes on it",
			"pt-osc swaps as soon as the copy finishes; there is no built-in pause to verify first",
		}
	}

	return x.finalize()
}

func (a *AdviceInput) ptOSCCommand() string {
	alter := strings.Join(opRaw(a.Input.Ops), ", ")
	user := a.User
	if user == "" {
		user = "root"
	}
	port := a.Port
	if port == "" {
		port = "3306"
	}
	dsn := fmt.Sprintf("D=%s,t=%s,h=%s,P=%s,u=%s", a.Projected.Schema, a.Projected.Name, a.Host, port, user)

	var b strings.Builder
	b.WriteString("pt-online-schema-change \\\n")
	fmt.Fprintf(&b, "  --alter=%q \\\n", alter)
	b.WriteString("  --max-load Threads_running=50 --critical-load Threads_running=100 \\\n")
	b.WriteString("  --chunk-size 1000 \\\n")
	if len(a.Projected.ReferencedBy) > 0 {
		b.WriteString("  --alter-foreign-keys-method=auto \\\n")
	}
	fmt.Fprintf(&b, "  --print \\\n  %s\n", dsn)
	return b.String()
}

// --- helpers ---------------------------------------------------------------

func verdictOf(x *EngineAssessment) Verdict {
	for _, r := range x.Requirements {
		if !r.Met {
			return VerdictUnsuitable
		}
	}
	return VerdictSuitable
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
