// Package render turns a Plan into human-readable text.
//
// Output is intentionally ASCII-only: Saksama runs on servers with TERM=dumb,
// locale C, and unpredictable SSH clients, so box-drawing characters are a
// liability rather than a feature (PRD NFR-4).
package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
)

// PlanCard writes the human-readable plan.
func PlanCard(w io.Writer, p analyzer.Plan) error {
	var b strings.Builder

	label := func(k, v string) {
		fmt.Fprintf(&b, "%-10s %s\n", k, v)
	}

	label("ALTER", oneLine(p.Alter))
	label("TARGET", targetLine(p))
	label("ENGINE", engineLine(p))
	label("DURATION", durationLine(p.Duration))

	if p.Schema.EstimatedRows > 0 || p.Schema.DataBytes > 0 {
		label("TABLE", tableLine(p))
	}
	label("FINGERPRINT", p.SchemaFingerprint)

	b.WriteString("\nWHY\n")
	if len(p.Recommendation.Reasons) == 0 {
		b.WriteString("  (no recommendation)\n")
	}
	for _, r := range p.Recommendation.Reasons {
		fmt.Fprintf(&b, "  - %s\n", r)
	}

	if len(p.Operations) > 0 {
		b.WriteString("\nOPERATIONS\n")
		for _, op := range p.Operations {
			method := string(op.Method)
			if op.Lock != "" {
				method += ", LOCK=" + op.Lock
			}
			fmt.Fprintf(&b, "  %-12s %-22s %s\n", method, op.RuleID, oneLine(op.Raw))
		}
	}

	if len(p.Checks) > 0 {
		b.WriteString("\nFINDINGS\n")
		for _, c := range p.Checks {
			fmt.Fprintf(&b, "  %-7s [%s] %s\n", string(c.Severity), c.ID, c.Title)
			if c.Detail != "" {
				fmt.Fprintf(&b, "          %s\n", oneLine(c.Detail))
			}
			if c.Remediation != "" {
				fmt.Fprintf(&b, "          action: %s\n", oneLine(c.Remediation))
			}
			if c.MySQLError > 0 {
				fmt.Fprintf(&b, "          mysql: ERROR %d\n", c.MySQLError)
			}
		}
	}

	b.WriteString("\nRECOVERY\n")
	fmt.Fprintf(&b, "  Before execution: %s\n", p.Recovery.BeforeExecution)
	if p.Recovery.DuringExecution != "" {
		fmt.Fprintf(&b, "  During execution: %s\n", p.Recovery.DuringExecution)
	}
	if p.Recovery.AfterSuccess != "" {
		fmt.Fprintf(&b, "  After success:    %s\n", p.Recovery.AfterSuccess)
	}
	if p.Recovery.HoldCutover {
		b.WriteString("  Hold cutover:     supported; the production table is not swapped until you run 'saksama cutover'\n")
	}

	if len(p.Verification) > 0 {
		b.WriteString("\nVERIFY AFTER\n")
		for _, v := range p.Verification {
			fmt.Fprintf(&b, "  %-20s %s\n", v.Kind, v.Name)
		}
	}

	if len(p.Notes) > 0 {
		b.WriteString("\nNOTES\n")
		for _, n := range p.Notes {
			fmt.Fprintf(&b, "  - %s\n", oneLine(n))
		}
	}

	b.WriteString("\n")
	fmt.Fprintf(&b, "RECOMMEND  %s\n", p.Recommendation.Engine)
	if len(p.Recommendation.Fallbacks) > 0 {
		var fb []string
		for _, f := range p.Recommendation.Fallbacks {
			fb = append(fb, string(f))
		}
		fmt.Fprintf(&b, "FALLBACKS  %s (manual override only)\n", strings.Join(fb, ", "))
	}
	fmt.Fprintf(&b, "BLOCKERS   %d\n", p.Recommendation.Blockers)
	if p.Recommendation.Blockers > 0 {
		fmt.Fprintf(&b, "RESULT     refused\n")
	} else {
		fmt.Fprintf(&b, "RESULT     ready (nothing has been changed)\n")
	}

	_, err := io.WriteString(w, b.String())
	return err
}

func engineLine(p analyzer.Plan) string {
	eng := string(p.Recommendation.Engine)
	if p.Recommendation.Engine == analyzer.EngineNone {
		return "none (blocked)"
	}
	var methods []string
	for _, op := range p.Operations {
		m := string(op.Method)
		if op.Lock != "" {
			m += ", LOCK=" + op.Lock
		}
		methods = append(methods, m)
	}
	if len(methods) > 0 {
		return fmt.Sprintf("%s [%s] (%s)", eng, strings.Join(dedupe(methods), " + "), p.Server.Version)
	}
	return fmt.Sprintf("%s (%s)", eng, p.Server.Version)
}

func durationLine(d analyzer.DurationEstimate) string {
	if d.FromSeconds == 0 && d.ToSeconds == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%s - %s | confidence %s", humanDuration(d.FromSeconds), humanDuration(d.ToSeconds), d.Confidence)
}

func targetLine(p analyzer.Plan) string {
	name := p.Target.Table
	if p.Target.Database != "" {
		name = p.Target.Database + "." + p.Target.Table
	}
	line := fmt.Sprintf("%s @ %s", name, orDash(p.Target.Host))
	if p.Target.Production {
		line += " [production]"
	}
	return line
}

func tableLine(p analyzer.Plan) string {
	rows := "unknown rows"
	if p.Schema.EstimatedRows > 0 {
		rows = fmt.Sprintf("~%s rows", humanCount(p.Schema.EstimatedRows))
	}
	size := humanBytes(p.Schema.DataBytes + p.Schema.IndexBytes)
	fk := fmt.Sprintf("%d out / %d in FK", p.Schema.OutgoingFK, p.Schema.IncomingFK)
	return fmt.Sprintf("%s, %s, %d cols, %d indexes, %s", rows, size, p.Schema.Columns, p.Schema.Indexes, fk)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.Join(strings.Fields(s), " ")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
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

func humanDuration(sec int) string {
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	if sec < 3600 {
		return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
	}
	return fmt.Sprintf("%dh%02dm", sec/3600, (sec%3600)/60)
}

func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func humanCount(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}
