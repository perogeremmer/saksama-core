package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/introspect"
)

// SchemaReport writes the database-wide engine fitness table.
//
// This is the "inspect it yourself" view: no table name required, every table
// judged on its own shape, biggest first so the expensive ones are on top.
func SchemaReport(w io.Writer, schema string, rows []analyzer.TableAssessment, artifacts []introspect.Artifact) error {
	return SchemaReportStyled(w, schema, rows, artifacts, true)
}

// SchemaReportStyled lets the caller choose plain output for scripts and logs.
func SchemaReportStyled(w io.Writer, schema string, rows []analyzer.TableAssessment, artifacts []introspect.Artifact, border bool) error {
	var b strings.Builder

	fmt.Fprintf(&b, "ENGINE SUITABILITY for schema %s\n", schema)
	if len(rows) == 0 {
		b.WriteString("no tables found\n")
		_, err := io.WriteString(w, b.String())
		return err
	}
	fmt.Fprintf(&b, "%d table(s), sorted by size. These are the engines for a rebuild;\n", len(rows))
	b.WriteString("an index-only change needs no engine at all on MySQL 8.0.\n\n")

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

	b.WriteString("\nNEXT\n")
	b.WriteString("  record your decision per table:  saksama engine set gh-ost -d <db> -t <table>\n")
	b.WriteString("  inspect one table in detail:     saksama advise --live -d <db> -t <table>\n")

	_, err := io.WriteString(w, b.String())
	return err
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
	return string(r.RebuildEngine)
}
