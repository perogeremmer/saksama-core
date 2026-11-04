package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/introspect"
	"github.com/perogeremmer/saksama-core/internal/model"
)

// SchemaReport writes the database-wide engine fitness table.
//
// This is the "inspect it yourself" view: no table name required, every table
// judged on its own shape, biggest first so the expensive ones are on top.
func SchemaReport(w io.Writer, schema string, rows []analyzer.TableAssessment, artifacts []introspect.Artifact, srv model.Server) error {
	return SchemaReportStyled(w, schema, rows, artifacts, srv, true)
}

// SchemaReportStyled lets the caller choose plain output for scripts and logs.
//
// srv is required because the report makes one server-level claim: whether an
// index-only change needs an engine at all. That claim is read from the version
// gated capability matrix, never written here, so the report cannot promise
// native DDL on a server that does not have it.
func SchemaReportStyled(w io.Writer, schema string, rows []analyzer.TableAssessment, artifacts []introspect.Artifact, srv model.Server, border bool) error {
	var b strings.Builder

	fmt.Fprintf(&b, "ENGINE SUITABILITY for schema %s\n", schema)
	if len(rows) == 0 {
		b.WriteString("no tables found\n")
		_, err := io.WriteString(w, b.String())
		return err
	}
	fmt.Fprintf(&b, "%d table(s), sorted by size.\n\n", len(rows))

	t := &Table{
		Border:   border,
		MaxWidth: 44,
		Columns: []Column{
			{Header: "TABLE"}, {Header: "ROWS", Align: Right}, {Header: "SIZE", Align: Right},
			{Header: "PRIMARY KEY"}, {Header: "FK", Align: Right},
			{Header: "GH-OST"}, {Header: "PT-OSC"}, {Header: "REBUILD WITH"},
		},
	}
	for _, r := range rows {
		t.Add(r.Table, humanCount(r.Rows), humanBytes(r.SizeBytes), r.PrimaryKey,
			fmt.Sprintf("%d/%d", r.FKOut, r.FKIn),
			verdictCell(r.GhOst), verdictCell(r.PtOSC), rebuildCell(r))
	}
	if err := t.Render(&b); err != nil {
		return err
	}

	// Legend: a table of yes/no needs one, or it is a guessing game.
	b.WriteString("\n")
	b.WriteString("  FK        outgoing/incoming foreign keys\n")
	b.WriteString("  GH-OST    yes when gh-ost can copy this table at all\n")
	b.WriteString("  PT-OSC    yes when pt-online-schema-change can\n")
	b.WriteString("  REBUILD   the engine to choose when this table ever needs a rebuild\n")
	b.WriteString("  VERDICT   says which of those engines you can actually run today\n")

	verdictBlock(&b, schema, rows, srv)

	// Explain the "none" rows, because "none" without a reason is useless.
	var blocked []analyzer.TableAssessment
	for _, r := range rows {
		if r.RebuildEngine == analyzer.EngineNone {
			blocked = append(blocked, r)
		}
	}
	if len(blocked) > 0 {
		b.WriteString("\nTABLES NO ENGINE CAN COPY\n")
		for _, r := range blocked {
			fmt.Fprintf(&b, "  %s\n", r.Table)
			for _, reason := range r.Blockers {
				fmt.Fprintf(&b, "    - %s\n", reason)
			}
		}
	}

	var noted []analyzer.TableAssessment
	for _, r := range rows {
		if len(r.Notes) > 0 {
			noted = append(noted, r)
		}
	}
	if len(noted) > 0 {
		b.WriteString("\nNOTES\n")
		for _, r := range noted {
			fmt.Fprintf(&b, "  %-24s %s\n", r.Table, strings.Join(r.Notes, "; "))
		}
	}

	if len(artifacts) > 0 {
		b.WriteString("\nLEFTOVER ARTIFACTS (from previous migrations, not tables you manage)\n")
		for _, a := range artifacts {
			fmt.Fprintf(&b, "  %-16s %-28s %s\n", a.Kind, a.Name, a.Evidence)
		}
		b.WriteString("  saksama cleanup verifies ownership in the session record before dropping anything.\n")
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// verdictBlock is the answer, as opposed to the evidence above it. It states
// what needs no engine, what each rebuild would use, whether that engine can run
// here, and the exact command a reader should type next.
func verdictBlock(b *strings.Builder, schema string, rows []analyzer.TableAssessment, srv model.Server) {
	if schema != "" {
		fmt.Fprintf(b, "\nVERDICT for schema %s\n", schema)
	} else {
		// Offline single-table reports have no schema name to announce.
		b.WriteString("\nVERDICT\n")
	}

	native, detail := analyzer.NativeIndexPath(srv)
	switch {
	case native:
		fmt.Fprintf(b, "  index-only change   no engine, no copy: %s\n", detail)
	case srv.Version == "":
		// Offline reports may have no server at all. Say that, rather than
		// quoting a parse error at a server that was never named.
		b.WriteString("  index-only change   unknown: no server version, so no DDL capability is claimed\n")
		b.WriteString("                      pass --mysql-version to get an answer here\n")
	default:
		fmt.Fprintf(b, "  index-only change   not native on this server: %s\n", detail)
	}

	// One group per rebuild engine, in ladder order so the output is stable.
	type group struct {
		engine  analyzer.Engine
		tables  []string
		ready   bool
		blocked string
		fix     string
	}
	var groups []*group
	index := map[analyzer.Engine]*group{}
	var refused []analyzer.TableAssessment
	for _, r := range rows {
		if r.RebuildEngine == analyzer.EngineNone {
			refused = append(refused, r)
			continue
		}
		g, ok := index[r.RebuildEngine]
		if !ok {
			g = &group{engine: r.RebuildEngine, ready: r.RebuildReady, blocked: r.RebuildBlocked, fix: r.RebuildFix}
			index[r.RebuildEngine] = g
			groups = append(groups, g)
		}
		g.tables = append(g.tables, r.Table)
	}
	// Ladder order: gh-ost before pt-osc, whatever order the rows arrived in.
	for i := 1; i < len(groups); i++ {
		for j := i; j > 0 && engineRank(groups[j].engine) < engineRank(groups[j-1].engine); j-- {
			groups[j], groups[j-1] = groups[j-1], groups[j]
		}
	}

	if len(groups) > 0 {
		b.WriteString("  if a rebuild is needed\n")
		for _, g := range groups {
			status := "ready"
			if !g.ready {
				status = orDash(g.blocked)
			}
			fmt.Fprintf(b, "    %-8s %s\n", g.engine, groupLine(g.tables, status))
		}
	}
	if len(refused) > 0 {
		names := make([]string, 0, len(refused))
		for _, r := range refused {
			names = append(names, r.Table)
		}
		reason := ""
		if len(refused[0].Blockers) > 0 {
			reason = refused[0].Blockers[0]
		}
		fmt.Fprintf(b, "  no engine           %s  (%s)\n", groupLine(names, ""), reason)
	}

	// What to type next. Every command is derived from this schema, because a
	// hardcoded example is a footgun: the engine in it may not fit the table.
	b.WriteString("\n  run this next\n")
	biggest := rows[0]
	flags := dbfFlags(schema, biggest.Table)
	fmt.Fprintf(b, "    decide a statement   saksama advise --live%s -a \"<ALTER>\"\n", flags)
	if biggest.RebuildEngine != analyzer.EngineNone {
		fmt.Fprintf(b, "    record the engine    saksama engine set %s%s\n", biggest.RebuildEngine, flags)
	}
	for _, g := range groups {
		if g.ready || g.fix == "" {
			continue
		}
		// On its own line: an install hint is often longer than the whole
		// verdict, and squeezing it into a column makes it uncopyable.
		fmt.Fprintf(b, "    unblock %s\n      %s\n", g.engine, g.fix)
	}
}

// groupLine renders "3 tables: a, b, c   ready", trimming long lists so one
// schema with 200 tables cannot bury the verdict.
func groupLine(tables []string, status string) string {
	const maxNames = 4
	noun := "tables"
	if len(tables) == 1 {
		noun = "table"
	}
	shown := tables
	extra := 0
	if len(shown) > maxNames {
		extra = len(shown) - maxNames
		shown = shown[:maxNames]
	}
	names := strings.Join(shown, ", ")
	if extra > 0 {
		names += fmt.Sprintf(", +%d more", extra)
	}
	line := fmt.Sprintf("%d %s: %s", len(tables), noun, names)
	if status != "" {
		line = fmt.Sprintf("%-46s %s", line, status)
	}
	return line
}

func engineRank(e analyzer.Engine) int {
	switch e {
	case analyzer.EngineNative:
		return 0
	case analyzer.EngineGhOst:
		return 1
	case analyzer.EnginePtOSC:
		return 2
	}
	return 3
}

// dbfFlags builds the connection part of a suggested command without inventing
// a host: the database and table are the two facts this report knows.
func dbfFlags(database, table string) string {
	var s string
	if database != "" {
		s += " -d " + database
	}
	if table != "" {
		s += " -t " + table
	}
	return s
}

func verdictCell(v analyzer.Verdict) string {
	switch v {
	case analyzer.VerdictSuitable:
		return "yes"
	case analyzer.VerdictUnsuitable:
		return "no"
	}
	return "?"
}

func rebuildCell(r analyzer.TableAssessment) string {
	if r.RebuildEngine == analyzer.EngineNone {
		return "none"
	}
	if !r.RebuildReady {
		// A cell that reads "gh-ost" while the binary is missing is the tool
		// recommending something it cannot run.
		return string(r.RebuildEngine) + " (unavailable)"
	}
	return string(r.RebuildEngine)
}
