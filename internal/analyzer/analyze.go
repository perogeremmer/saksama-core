package analyzer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// ContractVersion is the machine-readable output contract version.
const ContractVersion = 1

// Severity ranks a check result.
type Severity string

const (
	SeverityBlocker Severity = "BLOCKER"
	SeverityWarn    Severity = "WARN"
	SeverityInfo    Severity = "INFO"
)

// Check is one evaluated rule with an actionable explanation.
type Check struct {
	ID          string   `json:"id"`
	Severity    Severity `json:"severity"`
	Title       string   `json:"title"`
	Detail      string   `json:"detail"`
	Remediation string   `json:"remediation,omitempty"`
	MySQLError  int      `json:"mysql_error,omitempty"`
}

// Engine names an execution path.
type Engine string

const (
	EngineNative Engine = "native"
	EngineGhOst  Engine = "gh-ost"
	EnginePtOSC  Engine = "pt-osc"
	EngineNone   Engine = "none"
)

// Target identifies the migration subject.
type Target struct {
	Host       string `json:"host"`
	Database   string `json:"database"`
	Table      string `json:"table"`
	Production bool   `json:"production"`
}

// ServerInfo is the server as observed.
type ServerInfo struct {
	Version          string `json:"version"`
	Vendor           string `json:"vendor"`
	Managed          bool   `json:"managed"`
	DefaultCharset   string `json:"default_charset,omitempty"`
	DefaultCollation string `json:"default_collation,omitempty"`
}

// SchemaInfo summarises the table the plan is based on.
type SchemaInfo struct {
	Engine        string `json:"engine"`
	RowFormat     string `json:"row_format"`
	Charset       string `json:"charset"`
	Collation     string `json:"collation"`
	Columns       int    `json:"columns"`
	Indexes       int    `json:"indexes"`
	OutgoingFK    int    `json:"outgoing_fk"`
	IncomingFK    int    `json:"incoming_fk"`
	EstimatedRows int64  `json:"estimated_rows"`
	DataBytes     int64  `json:"data_bytes"`
	IndexBytes    int64  `json:"index_bytes"`
	Partitioned   bool   `json:"partitioned"`
	Source        string `json:"source"` // live | ddl | json
}

// OpPlan is the per-operation verdict.
type OpPlan struct {
	Kind    model.OpKind `json:"kind"`
	Raw     string       `json:"raw"`
	RuleID  string       `json:"rule_id"`
	Method  Method       `json:"method"`
	Lock    string       `json:"lock,omitempty"`
	Reason  string       `json:"reason"`
	Blocked bool         `json:"blocked,omitempty"`
}

// Recommendation is the routing decision.
type Recommendation struct {
	// Analyzer is what the rules would pick on their own.
	Analyzer Engine `json:"analyzer"`
	// Engine is what will actually run: the analyzer's pick unless the operator
	// recorded a choice.
	Engine Engine `json:"engine"`
	// EngineSource says where Engine came from: recommendation | flag | recorded.
	EngineSource string   `json:"engine_source"`
	Fallbacks    []Engine `json:"fallbacks"`
	Reasons      []string `json:"reasons"`
	Blocked      bool     `json:"blocked"`
	Blockers     int      `json:"blockers"`
	Warnings     int      `json:"warnings"`
	Infos        int      `json:"infos"`
	// Executable reports whether this build can actually run the recommended
	// engine. A plan can be safe and still not executable, and saying so is the
	// difference between a tool you trust and a tool that lies.
	Executable bool `json:"executable"`
	// ExecutableReason explains a false Executable.
	ExecutableReason string `json:"executable_reason,omitempty"`
}

// DurationEstimate is deliberately a range with a confidence label.
type DurationEstimate struct {
	FromSeconds int    `json:"from_seconds"`
	ToSeconds   int    `json:"to_seconds"`
	Confidence  string `json:"confidence"` // low | medium | high
	Basis       string `json:"basis"`
}

// DiskEstimate describes additional bytes the plan will need.
type DiskEstimate struct {
	RequiredBytes int64  `json:"required_bytes"`
	Note          string `json:"note"`
}

// Recovery describes what is actually recoverable at each phase.
type Recovery struct {
	BeforeExecution string `json:"before_execution"`
	DuringExecution string `json:"during_execution"`
	AfterSuccess    string `json:"after_success"`
	HoldCutover     bool   `json:"hold_cutover"`
}

// Verification is a post-execution assertion.
type Verification struct {
	Kind  string `json:"kind"` // index_exists | column_exists | index_absent | foreign_key_present
	Name  string `json:"name"`
	Table string `json:"table,omitempty"`
}

// Plan is the complete analyzer output. It is the product API (PRD FR-5).
type Plan struct {
	ContractVersion   int                `json:"contract_version"`
	GeneratedAt       string             `json:"generated_at"`
	Target            Target             `json:"target"`
	Alter             string             `json:"alter"`
	AlterNormalized   string             `json:"alter_normalized,omitempty"`
	Server            ServerInfo         `json:"server"`
	Schema            SchemaInfo         `json:"schema"`
	SchemaFingerprint string             `json:"schema_fingerprint"`
	Operations        []OpPlan           `json:"operations"`
	Recommendation    Recommendation     `json:"recommendation"`
	Checks            []Check            `json:"checks"`
	Duration          DurationEstimate   `json:"duration"`
	Disk              DiskEstimate       `json:"disk"`
	Recovery          Recovery           `json:"recovery"`
	Verification      []Verification     `json:"verification"`
	Engines           []EngineAssessment `json:"engines"`
	// TableFacts are the observed schema facts the advice is based on, stated
	// once so every conclusion is traceable to something real.
	TableFacts []Evidence `json:"table_facts"`
	Notes      []string   `json:"notes,omitempty"`
}

// Input is everything the analyzer needs. No database handle, ever.
type Input struct {
	Server    model.Server
	Table     model.Table
	Ops       []model.Op
	RawAlter  string
	Algorithm string
	Lock      string
	Host      string
	// DBUser and DBPort appear in generated engine commands.
	DBUser       string
	DBPort       string
	Production   bool
	SchemaSource string   // live | ddl | json
	Ignored      []string // schema fragments the parser could not model
	EngineHint   Engine   // resolved override; empty means auto
	// EngineHintSource records whether the override came from a flag or a
	// recorded decision, so the audit trail can tell them apart.
	EngineHintSource string
	// Binaries are the engine executables found on this machine, injected so the
	// analyzer never spawns a process.
	Binaries map[string]EngineBinary
	MaxLoad  int   // optional: current Threads_running observation
	FreeDisk int64 // optional: bytes free on the data volume
	// Live is the observation collected from a real server. Nil means the plan
	// is offline, and every live-only check says so instead of passing silently.
	Live *LiveState
}

// Analyze produces a Plan. It is a pure function.
func Analyze(in Input) Plan {
	now := time.Now().UTC()

	p := Plan{
		ContractVersion: ContractVersion,
		GeneratedAt:     now.Format(time.RFC3339),
		Target: Target{
			Host:       in.Host,
			Database:   firstNonEmpty(in.Table.Schema, in.Server.Host, ""),
			Table:      in.Table.Name,
			Production: in.Production,
		},
		Alter:             strings.TrimSpace(in.RawAlter),
		AlterNormalized:   normalizeAlter(in.Ops),
		SchemaFingerprint: Fingerprint(in.Table),
		Server: ServerInfo{
			Version:          in.Server.Version,
			Vendor:           in.Server.Vendor,
			Managed:          in.Server.IsManaged(),
			DefaultCharset:   in.Server.DefaultChar,
			DefaultCollation: in.Server.DefaultColl,
		},
		Schema: SchemaInfo{
			Engine:        in.Table.Engine,
			RowFormat:     in.Table.RowFormat,
			Charset:       in.Table.Charset,
			Collation:     in.Table.Collation,
			Columns:       len(in.Table.Columns),
			Indexes:       len(in.Table.Indexes),
			OutgoingFK:    len(in.Table.ForeignKeys),
			IncomingFK:    len(in.Table.ReferencedBy),
			EstimatedRows: in.Table.Rows,
			DataBytes:     in.Table.DataLength,
			IndexBytes:    in.Table.IndexLength,
			Partitioned:   in.Table.Partitioned,
			Source:        in.SchemaSource,
		},
	}

	if len(in.Ignored) > 0 {
		p.Notes = append(p.Notes, fmt.Sprintf("schema parser ignored %d fragment(s): %s",
			len(in.Ignored), strings.Join(in.Ignored, "; ")))
	}

	// Safety net: a plan with no operations must never be reported as ready.
	ops := in.Ops
	if len(ops) == 0 {
		ops = []model.Op{{Kind: model.OpUnknown, Raw: in.RawAlter}}
		p.Notes = append(p.Notes, "no ALTER operations were recognised; treating the request as unknown")
	}

	// Per-operation capability verdicts.
	anyRebuild := false
	anyNative := false
	anyBlocked := false
	for _, op := range ops {
		cap := capabilityFor(op, in.Server, in.Table)
		p.Operations = append(p.Operations, OpPlan{
			Kind: op.Kind, Raw: op.Raw, RuleID: cap.RuleID,
			Method: cap.Method, Lock: cap.Lock, Reason: cap.Reason, Blocked: cap.Blocked,
		})
		if cap.Blocked {
			anyBlocked = true
		}
		if cap.Rebuild {
			anyRebuild = true
		}
		if cap.Native {
			anyNative = true
		}
	}

	// Project the post-ALTER schema, then run structural checks against it.
	projected, simNotes := applyOps(in.Table, ops)
	if len(simNotes) > 0 {
		p.Notes = append(p.Notes, simNotes...)
	}

	p.Checks = append(p.Checks, checksCapability(in, ops, anyBlocked, anyRebuild)...)
	p.Checks = append(p.Checks, checksCharset(in, projected)...)
	p.Checks = append(p.Checks, checksIndexes(in, projected)...)
	p.Checks = append(p.Checks, checksForeignKeys(in, projected)...)
	p.Checks = append(p.Checks, checksOperational(in, anyRebuild)...)
	p.Checks = append(p.Checks, liveChecks(in, projected)...)

	// Engine suitability drives routing, so it must be computed first.
	p.Engines = AssessEngines(in, projected)
	p.TableFacts = (&AdviceInput{Input: in, Projected: projected}).schemaFacts()

	// Routing.
	rec := route(in, p.Engines, anyRebuild, anyNative, anyBlocked, projected)
	p.Checks = append(p.Checks, engineChecks(p.Engines, rec)...)
	p.Checks = append(p.Checks, engineWarnings(in, p.Engines, rec)...)
	for _, c := range p.Checks {
		switch c.Severity {
		case SeverityBlocker:
			rec.Blockers++
		case SeverityWarn:
			rec.Warnings++
		case SeverityInfo:
			rec.Infos++
		}
	}
	// A blocker erases the engine when the change itself cannot run: an unknown
	// operation, an impossible index, a touched constrained column. The exception
	// is "engine not installed" (R3): that must keep naming the engine, because
	// install this and retry is the whole answer.
	hardBlockers := 0
	for _, c := range p.Checks {
		if c.Severity != SeverityBlocker || c.ID == chkEngineNotInstalled {
			continue
		}
		hardBlockers++
	}
	if rec.Blockers > 0 {
		rec.Blocked = true
		if hardBlockers > 0 {
			rec.Engine = EngineNone
			rec.Reasons = append(rec.Reasons, fmt.Sprintf("%d blocker(s) prevent any engine from running this change", hardBlockers))
		} else {
			rec.Reasons = append(rec.Reasons, fmt.Sprintf("%d blocker(s) prevent execution", rec.Blockers))
		}
	}

	// Whether the effective engine can actually be driven is a property of the
	// assessment, not of the safety rules. Keeping them apart is what stops
	// "unsuitable" and "not installed" from being conflated.
	if a, ok := findAssessment(p.Engines, rec.Engine); ok {
		rec.Executable = a.Runnable() && !rec.Blocked
		switch {
		case a.Verdict != VerdictSuitable:
			rec.ExecutableReason = "the engine cannot perform this change"
		case !a.Installed:
			rec.ExecutableReason = "the engine binary is not installed on this machine"
		case !a.Wrapped:
			rec.ExecutableReason = "this build has no adapter for the engine"
		case rec.Blocked:
			rec.ExecutableReason = fmt.Sprintf("%d blocker(s) must be resolved first", rec.Blockers)
		}
	}
	p.Recommendation = rec

	p.Duration = estimateDuration(in, projected, rec.Engine)
	p.Disk = estimateDisk(projected, rec.Engine)
	p.Recovery = recoveryFor(rec.Engine)
	p.Verification = verifications(in, projected)

	sort.SliceStable(p.Checks, func(i, j int) bool {
		return severityRank(p.Checks[i].Severity) > severityRank(p.Checks[j].Severity)
	})

	return p
}

func hasCheckID(checks []Check, id string) bool {
	for _, c := range checks {
		if c.ID == id {
			return true
		}
	}
	return false
}

func severityRank(s Severity) int {
	switch s {
	case SeverityBlocker:
		return 3
	case SeverityWarn:
		return 2
	default:
		return 1
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func normalizeAlter(ops []model.Op) string {
	parts := make([]string, 0, len(ops))
	for _, op := range ops {
		parts = append(parts, op.Raw)
	}
	return strings.Join(parts, ", ")
}

// Fingerprint produces a stable digest of the schema-relevant table state.
// It intentionally ignores volatile values (estimated rows, sizes) so that a
// plan stays comparable while the table keeps growing.
func Fingerprint(t model.Table) string {
	var b strings.Builder
	b.WriteString("v1\x00")
	b.WriteString(strings.ToLower(t.Schema) + "." + strings.ToLower(t.Name) + "\x00")
	b.WriteString(strings.ToLower(t.Engine) + "\x00")
	b.WriteString(strings.ToUpper(t.RowFormat) + "\x00")
	b.WriteString(strings.ToLower(t.Charset) + "\x00")
	b.WriteString(strings.ToLower(t.Collation) + "\x00")

	cols := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		nullable := "NOT NULL"
		if c.Nullable {
			nullable = "NULL"
		}
		extra := c.Extra
		if c.Generated {
			extra += ":generated"
		}
		cols = append(cols, strings.ToLower(c.Name)+":"+strings.ToLower(c.Type)+":"+
			strings.ToLower(c.Charset)+":"+strings.ToLower(c.Collation)+":"+nullable+":"+extra)
	}
	sort.Strings(cols)
	for _, c := range cols {
		b.WriteString("col:" + c + "\x00")
	}

	idxs := make([]string, 0, len(t.Indexes))
	for _, ix := range t.Indexes {
		parts := make([]string, 0, len(ix.Parts))
		for _, p := range ix.Parts {
			s := strings.ToLower(p.Column)
			if p.PrefixLen > 0 {
				s += fmt.Sprintf("(%d)", p.PrefixLen)
			}
			parts = append(parts, s)
		}
		idxs = append(idxs, strings.ToLower(ix.Name)+":"+strings.ToLower(strings.Join(parts, ","))+
			fmt.Sprintf(":u=%t:p=%t", ix.Unique, ix.Primary))
	}
	sort.Strings(idxs)
	for _, ix := range idxs {
		b.WriteString("idx:" + ix + "\x00")
	}

	fks := make([]string, 0, len(t.ForeignKeys)+len(t.ReferencedBy))
	for _, fk := range t.ForeignKeys {
		fks = append(fks, fmt.Sprintf("out:%s:%s->%s.%s:%s/%s",
			strings.ToLower(fk.Name), strings.ToLower(strings.Join(fk.Columns, ",")),
			strings.ToLower(fk.RefTable), strings.ToLower(strings.Join(fk.RefColumns, ",")),
			fk.OnDelete, fk.OnUpdate))
	}
	for _, fk := range t.ReferencedBy {
		fks = append(fks, fmt.Sprintf("in:%s:%s:%s->%s:%s/%s",
			strings.ToLower(fk.Name), strings.ToLower(fk.RefTable),
			strings.ToLower(strings.Join(fk.RefColumns, ",")),
			strings.ToLower(strings.Join(fk.Columns, ",")),
			fk.OnDelete, fk.OnUpdate))
	}
	sort.Strings(fks)
	for _, fk := range fks {
		b.WriteString("fk:" + fk + "\x00")
	}

	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}
