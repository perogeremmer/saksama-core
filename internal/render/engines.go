package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
)

// EngineMatrix writes the "which tool fits this table?" report.
//
// This is the answer people actually ask for: not "what will you run" but "is
// gh-ost or Percona Toolkit the right choice here, and why". It therefore shows
// every engine, including the ones this build cannot execute yet.
func EngineMatrix(w io.Writer, p analyzer.Plan) error {
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
	b.WriteString("\n")

	for _, a := range p.Engines {
		b.WriteString(engineBlock(a))
	}

	b.WriteString("VERDICT\n")
	switch {
	case p.Recommendation.Blockers > 0:
		if alt := fittingAlternative(p); alt != "" {
			fmt.Fprintf(&b, "  the chosen path is refused (%d blocker(s)), but %s\n",
				p.Recommendation.Blockers, alt)
		} else {
			fmt.Fprintf(&b, "  refused: %d blocker(s), and no engine is available in this build.\n",
				p.Recommendation.Blockers)
			b.WriteString("  see `saksama plan` for the blockers and their remediation.\n")
		}
	case p.Recommendation.Engine == analyzer.EngineNone:
		b.WriteString("  no engine satisfies the safety policy for this change\n")
	case !p.Recommendation.Executable:
		fmt.Fprintf(&b, "  best fit for this change: %s\n", p.Recommendation.Engine)
		fmt.Fprintf(&b, "  cannot run it here: %s\n", p.Recommendation.ExecutableReason)
	default:
		fmt.Fprintf(&b, "  recommended: %s (executable now)\n", p.Recommendation.Engine)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

func engineBlock(a analyzer.EngineAssessment) string {
	var b strings.Builder

	marker := "[--]"
	switch a.Verdict {
	case analyzer.VerdictSuitable:
		marker = "[ok]"
	case analyzer.VerdictBlocked:
		marker = "[x] "
	case analyzer.VerdictNotExecutable:
		marker = "[!] "
	}

	label := string(a.Engine)
	if a.Verdict == analyzer.VerdictNotExecutable {
		label += " (fits, but not executable in this build)"
	}

	fmt.Fprintf(&b, "%s %-12s %s\n", marker, strings.ToUpper(string(a.Engine)), verdictLine(a))
	fmt.Fprintf(&b, "     %s\n", a.Summary)
	for _, r := range a.Requirements {
		mark := "  ok"
		if !r.Met {
			if r.Blocking {
				mark = "FAIL"
			} else {
				mark = "warn"
			}
		}
		fmt.Fprintf(&b, "     [%s] %-32s %s\n", mark, r.Name, r.Detail)
	}
	for _, n := range a.Notes {
		fmt.Fprintf(&b, "     note  %s\n", n)
	}
	b.WriteString("\n")
	return b.String()
}

func verdictLine(a analyzer.EngineAssessment) string {
	switch a.Verdict {
	case analyzer.VerdictSuitable:
		return "SUITABLE"
	case analyzer.VerdictBlocked:
		if len(a.BlockedBy) > 0 {
			return "BLOCKED by " + strings.Join(a.BlockedBy, ", ")
		}
		return "BLOCKED"
	default:
		return "NOT EXECUTABLE HERE"
	}
}

// fittingAlternative names an engine whose own requirements pass, even though
// this build cannot execute it. That is the useful sentence when the blocker is
// about one engine's limitations rather than the change being unsafe.
func fittingAlternative(p analyzer.Plan) string {
	for _, a := range p.Engines {
		if a.Verdict == analyzer.VerdictBlocked {
			continue
		}
		if len(a.BlockedBy) > 0 {
			continue
		}
		switch a.Engine {
		case analyzer.EngineGhOst:
			return "gh-ost satisfies every requirement for this change; its adapter is not built yet"
		case analyzer.EnginePtOSC:
			return "pt-online-schema-change satisfies every requirement for this change; its adapter is not built yet"
		}
	}
	return ""
}

func iffStr(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}
