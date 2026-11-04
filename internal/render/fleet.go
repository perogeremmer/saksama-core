package render

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
)

// SchemaReport writes the database-wide engine fitness table.
//
// This is the "inspect it yourself" view: no table name required, every table
// judged on its own shape, biggest first so the expensive ones are on top.
func SchemaReport(w io.Writer, schema string, rows []analyzer.TableAssessment) error {
	var b strings.Builder

	fmt.Fprintf(&b, "ENGINE SUITABILITY for schema %s\n", schema)
	fmt.Fprintf(&b, "%d table(s), sorted by size. These are the engines for a rebuild;\n", len(rows))
	b.WriteString("an index-only change needs no engine at all on MySQL 8.0.\n\n")

	if len(rows) == 0 {
		b.WriteString("no tables found\n")
		_, err := io.WriteString(w, b.String())
		return err
	}

	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "TABLE\tROWS\tSIZE\tPRIMARY KEY\tFK\tGH-OST\tPT-OSC\tREBUILD WITH")
	for _, r := range rows {
		fk := fmt.Sprintf("%d/%d", r.FKOut, r.FKIn)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Table,
			humanCount(r.Rows),
			humanBytes(r.SizeBytes),
			r.PrimaryKey,
			fk,
			verdictCell(r.GhOst),
			verdictCell(r.PtOSC),
			rebuildCell(r),
		)
	}
	tw.Flush()

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
