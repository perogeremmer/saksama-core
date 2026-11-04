package analyzer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// TableAssessment is the engine verdict for one table, judged on the table's
// shape rather than on a specific ALTER.
//
// This is what lets an operator ask "which engine suits my tables?" without
// naming one: the copy engines' requirements are mostly properties of the table,
// not of the change.
type TableAssessment struct {
	Schema      string `json:"schema"`
	Table       string `json:"table"`
	Rows        int64  `json:"rows"`
	SizeBytes   int64  `json:"size_bytes"`
	PrimaryKey  string `json:"primary_key"`
	FKOut       int    `json:"fk_out"`
	FKIn        int    `json:"fk_in"`
	Partitioned bool   `json:"partitioned"`
	Engine      string `json:"engine"`
	// RebuildEngine is the engine to use if this table ever needs a rebuild, or
	// "none" when no engine can copy it.
	RebuildEngine Engine `json:"rebuild_engine"`
	// Blockers explains a "none", in the operator's words.
	Blockers []string `json:"blockers,omitempty"`
	GhOst    Verdict  `json:"gh_ost"`
	PtOSC    Verdict  `json:"pt_osc"`
	Notes    []string `json:"notes,omitempty"`
}

// AssessSchema judges every table in a schema for the copy engines.
func AssessSchema(tables []model.Table, srv model.Server, binaries map[string]EngineBinary, settings map[string]string) []TableAssessment {
	env := SettingsFromMap(settings)
	out := make([]TableAssessment, 0, len(tables))

	for _, t := range tables {
		a := TableAssessment{
			Schema: t.Schema, Table: t.Name,
			Rows: t.Rows, SizeBytes: t.SizeBytes(),
			FKOut: len(t.ForeignKeys), FKIn: len(t.ReferencedBy),
			Partitioned: t.Partitioned,
			Engine:      t.Engine,
			PrimaryKey:  describeKey(t),
		}

		in := Input{Server: srv, Table: t, Binaries: binaries, Settings: env}
		for _, assess := range AssessEngines(in, t) {
			switch assess.Engine {
			case EngineGhOst:
				a.GhOst = assess.Verdict
			case EnginePtOSC:
				a.PtOSC = assess.Verdict
			}
		}

		switch {
		case a.GhOst == VerdictSuitable && ghOstEnvReady(env):
			a.RebuildEngine = EngineGhOst
		case a.PtOSC == VerdictSuitable:
			a.RebuildEngine = EnginePtOSC
			switch {
			case a.GhOst == VerdictSuitable:
				a.Notes = append(a.Notes,
					"gh-ost suits this table but the server is not configured for it; see SERVER SETTINGS")
			default:
				a.Notes = append(a.Notes, "pt-osc only: gh-ost cannot handle this table")
			}
		default:
			a.RebuildEngine = EngineNone
			a.Blockers = rebuildBlockers(t)
		}

		if t.Partitioned {
			a.Notes = append(a.Notes, "partitioned")
		}
		if len(t.Triggers) > 0 {
			a.Notes = append(a.Notes, fmt.Sprintf("%d existing trigger(s)", len(t.Triggers)))
		}
		if !strings.EqualFold(t.Engine, "InnoDB") {
			a.Notes = append(a.Notes, "engine is "+t.Engine)
		}

		out = append(out, a)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].SizeBytes > out[j].SizeBytes })
	return out
}

// ghOstEnvReady reports whether the server is configured for gh-ost. Table shape
// is not enough: a server without binary logging cannot run it at all.
func ghOstEnvReady(settings ServerSettings) bool {
	for _, r := range EngineEnvironment(settings, EngineGhOst) {
		if !r.Met {
			return false
		}
	}
	return true
}

func describeKey(t model.Table) string {
	pk, ok := t.PrimaryKey()
	if !ok {
		if t.HasUniqueKey() {
			return "(unique only)"
		}
		return "(none)"
	}
	if len(pk.Parts) == 1 {
		col, _ := t.Column(pk.Parts[0].Column)
		return fmt.Sprintf("%s %s", col.Name, col.Type)
	}
	names := make([]string, 0, len(pk.Parts))
	for _, p := range pk.Parts {
		names = append(names, p.Column)
	}
	return fmt.Sprintf("composite(%s)", strings.Join(names, ","))
}

// rebuildBlockers explains why no copy engine can take a table.
func rebuildBlockers(t model.Table) []string {
	var out []string

	_, hasKey := func() (bool, bool) {
		p, u := false, false
		for _, ix := range t.Indexes {
			if ix.Primary {
				p = true
			}
			if ix.Unique {
				u = true
			}
		}
		return p, p || u
	}()

	if !hasKey {
		out = append(out, "no primary key or unique key to chunk by")
	}
	if t.PrimaryKeyParts() > 1 {
		out = append(out, "composite primary key: gh-ost cannot chunk it, and pt-osc needs a key (it has one)")
	}
	if len(t.ForeignKeys) > 0 || len(t.ReferencedBy) > 0 {
		out = append(out, fmt.Sprintf("foreign keys involved (%d out, %d in): gh-ost cannot, pt-osc needs a key", len(t.ForeignKeys), len(t.ReferencedBy)))
	}
	if t.Partitioned {
		out = append(out, "partitioned: gh-ost cannot copy it")
	}
	if !strings.EqualFold(t.Engine, "InnoDB") {
		out = append(out, "storage engine is "+t.Engine+"; both copy engines need InnoDB")
	}
	return out
}
