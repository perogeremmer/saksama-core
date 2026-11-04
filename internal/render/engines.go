package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
)

// EngineMatrix writes the "which tool fits this table?" report.
//
// It shows every engine, including the ones that do not fit, because the reason
// an engine does not fit is the most useful thing this tool can tell you. It also
// shows which engine the operator already recorded, so a mismatch between the
// recommendation and the decision is impossible to miss.
func EngineMatrix(w io.Writer, p analyzer.Plan, chosen string) error {
	var b strings.Builder

	target := p.Target.Table
	if p.Target.Database != "" {
		target = p.Target.Database + "." + target
	}
	fmt.Fprintf(&b, "ENGINE SUITABILITY for %s\n", target)
	fmt.Fprintf(&b, "ALTER   %s\n", oneLine(p.Alter))
	fmt.Fprintf(&b, "SERVER  %s%s\n", p.Server.Version, iffStr(p.Server.Managed, " (managed service)", ""))
	fmt.Fprintf(&b, "TABLE   %d cols, %d indexes, %d out / %d in FK%s\n",
		p.Schema.Columns, p.Schema.Indexes, p.Schema.OutgoingFK, p.Schema.IncomingFK,
		iffStr(p.Schema.Partitioned, ", partitioned", ""))

	if len(p.TableFacts) > 0 {
		b.WriteString("\nWHAT THIS TABLE LOOKS LIKE\n")
		for _, f := range p.TableFacts {
			fmt.Fprintf(&b, "  * %s\n", f.Fact)
			fmt.Fprintf(&b, "      %s\n", f.Effect)
		}
	}

	b.WriteString("\n")
	for _, a := range p.Engines {
		b.WriteString(engineBlock(a))
	}

	b.WriteString("VERDICT\n")
	fmt.Fprintf(&b, "  analyzer recommends  %s\n", p.Recommendation.Engine)
	if chosen != "" {
		if chosen == string(p.Recommendation.Engine) {
			fmt.Fprintf(&b, "  recorded choice      %s (agrees)\n", chosen)
		} else {
			fmt.Fprintf(&b, "  recorded choice      %s (differs from the recommendation)\n", chosen)
			fmt.Fprintf(&b, "  the recorded choice wins; undo with: saksama engine clear\n")
		}
	} else {
		fmt.Fprintf(&b, "  recorded choice      none, so the recommendation would be used\n")
		fmt.Fprintf(&b, "  decide with          saksama engine set <native|gh-ost|pt-osc>\n")
	}

	_, err := io.WriteString(w, b.String())
	return err
}

func engineBlock(a analyzer.EngineAssessment) string {
	var b strings.Builder

	header := fmt.Sprintf("%-9s %s", strings.ToUpper(string(a.Engine)), strings.ToUpper(string(a.Verdict)))
	status := engineStatus(a)
	if status != "" {
		header += "  (" + status + ")"
	}
	fmt.Fprintf(&b, "%s\n", header)
	fmt.Fprintf(&b, "  %s\n", a.Summary)

	if len(a.Requirements) > 0 {
		for _, r := range a.Requirements {
			mark := "  ok"
			if !r.Met {
				mark = "FAIL"
			}
			fmt.Fprintf(&b, "  [%s] %-33s %s\n", mark, r.Name, r.Detail)
		}
	}

	if len(a.Evidence) > 0 {
		fmt.Fprintf(&b, "  evidence for this engine:\n")
		for _, e := range a.Evidence {
			fmt.Fprintf(&b, "    * %s\n        %s\n", e.Fact, e.Effect)
		}
	}

	if len(a.Pros) > 0 {
		b.WriteString("  pros:\n")
		for _, p := range a.Pros {
			fmt.Fprintf(&b, "    + %s\n", p)
		}
	}
	if len(a.Cons) > 0 {
		b.WriteString("  cons:\n")
		for _, c := range a.Cons {
			fmt.Fprintf(&b, "    - %s\n", c)
		}
	}
	for _, n := range a.Notes {
		fmt.Fprintf(&b, "  note  %s\n", n)
	}

	if a.Command != "" {
		fmt.Fprintf(&b, "  command (what Saksama runs):\n")
		for _, line := range strings.Split(a.Command, "\n") {
			fmt.Fprintf(&b, "    %s\n", strings.TrimRight(line, "\\ "))
		}
	}
	if len(a.Steps) > 0 {
		b.WriteString("  next:\n")
		for _, s := range a.Steps {
			fmt.Fprintf(&b, "    - %s\n", s)
		}
	}

	b.WriteString("\n")
	return b.String()
}

// engineStatus summarises availability without muddying the verdict.
func engineStatus(a analyzer.EngineAssessment) string {
	switch {
	case a.Verdict == analyzer.VerdictUnsuitable:
		return ""
	case !a.Installed:
		return "not installed"
	case !a.Wrapped:
		return "no adapter in this build"
	}
	return "ready"
}

func iffStr(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}
