// Package render turns a Plan into human-readable text.
//
// Output is intentionally ASCII-only: Saksama runs on servers with TERM=dumb,
// locale C, and unpredictable SSH clients, so box-drawing characters are a
// liability rather than a feature.
package render

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
)

// Card formatting. Output stays ASCII-only (see the package comment), so alignment
// and word wrapping are the only tools available - and they are the two that matter:
// a value running past the terminal folds at an arbitrary place, which is exactly
// what makes a plan hard to read at 2am.
const (
	defaultCardWidth = 96
	minCardWidth     = 64
	maxCardWidth     = 140
)

// cardWidth is the line width to format for.
//
// COLUMNS is a shell variable that is usually not exported, so it is honoured when it
// happens to be there and SAKSAMA_WIDTH is the explicit knob. The default is a width
// most terminals have; the clamp keeps a silly value from making the card either a
// ribbon or an unreadable slab.
func cardWidth() int {
	for _, key := range []string{"SAKSAMA_WIDTH", "COLUMNS"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return clampWidth(n)
			}
		}
	}
	return defaultCardWidth
}

func clampWidth(n int) int {
	if n < minCardWidth {
		return minCardWidth
	}
	if n > maxCardWidth {
		return maxCardWidth
	}
	return n
}

// wrapText breaks one-line text into lines no longer than limit, on word
// boundaries. A word longer than the limit is left whole: breaking a fingerprint or
// a path mid-token produces something that cannot be copied or searched for.
func wrapText(s string, limit int) []string {
	s = oneLine(s)
	if s == "" {
		return nil
	}
	if limit < 1 {
		limit = 1
	}
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= limit:
			line += " " + word
		default:
			out = append(out, line)
			line = word
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}

// writeWrapped writes text at the given indent, wrapped to the card width. The
// first line gets `first`, later lines get `rest`; both are printed after the indent.
func writeWrapped(b *strings.Builder, indent int, first, rest, text string, width int) {
	lines := wrapText(text, width-indent-len(first))
	if len(lines) == 0 {
		return
	}
	pad := strings.Repeat(" ", indent)
	fmt.Fprintf(b, "%s%s%s\n", pad, first, lines[0])
	for _, line := range lines[1:] {
		fmt.Fprintf(b, "%s%s%s\n", pad, strings.Repeat(" ", len(first)), line)
	}
	_ = rest
}

// PlanCard writes the human-readable plan.
func PlanCard(w io.Writer, p analyzer.Plan) error {
	var b strings.Builder
	width := cardWidth()

	// One label column for the whole card, so ALTER and RESULT line up with each
	// other instead of each block inventing its own gutter.
	labelWidth := 11

	label := func(k, v string) {
		v = oneLine(v)
		lines := wrapText(v, width-labelWidth-1)
		if len(lines) == 0 {
			fmt.Fprintf(&b, "%-*s\n", labelWidth, k)
			return
		}
		fmt.Fprintf(&b, "%-*s %s\n", labelWidth, k, lines[0])
		for _, line := range lines[1:] {
			fmt.Fprintf(&b, "%s %s\n", strings.Repeat(" ", labelWidth), line)
		}
	}

	label("ALTER", p.Alter)
	label("TARGET", targetLine(p))
	label("ENGINE", engineLine(p))
	label("DURATION", durationLine(p.Duration))
	if p.Schema.EstimatedRows > 0 || p.Schema.DataBytes > 0 {
		label("TABLE", tableLine(p))
	}
	label("FINGERPRINT", p.SchemaFingerprint)
	if line := enginesLine(p); line != "" {
		label("ENGINES", line)
	}

	b.WriteString("\nWHY\n")
	if len(p.Recommendation.Reasons) == 0 {
		b.WriteString("  (no recommendation)\n")
	}
	for _, r := range p.Recommendation.Reasons {
		writeWrapped(&b, 2, "- ", "- ", r, width)
	}

	if len(p.Operations) > 0 {
		b.WriteString("\nOPERATIONS\n")
		// Widths come from the data: fixed gutters leave a hole when the method is
		// short, and a rule id is not always the same length.
		methodW, ruleW := 0, 0
		for _, op := range p.Operations {
			if n := len(operationMethod(op)); n > methodW {
				methodW = n
			}
			if n := len(op.RuleID); n > ruleW {
				ruleW = n
			}
		}
		for _, op := range p.Operations {
			prefix := fmt.Sprintf("  %-*s  %-*s  ", methodW, operationMethod(op), ruleW, op.RuleID)
			lines := wrapText(op.Raw, width-len(prefix))
			if len(lines) == 0 {
				fmt.Fprintf(&b, "%s\n", strings.TrimRight(prefix, " "))
				continue
			}
			fmt.Fprintf(&b, "%s%s\n", prefix, lines[0])
			for _, line := range lines[1:] {
				fmt.Fprintf(&b, "%s%s\n", strings.Repeat(" ", len(prefix)), line)
			}
		}
	}

	if len(p.Checks) > 0 {
		b.WriteString("\nFINDINGS\n")
		// Severity and id get their own columns so a list of findings reads down
		// one edge instead of zig-zagging with the length of each id.
		sevW, idW := len("BLOCKER"), 3
		for _, c := range p.Checks {
			if n := len(string(c.Severity)); n > sevW {
				sevW = n
			}
			if n := len(c.ID); n > idW {
				idW = n
			}
		}
		bodyIndent := 2 + sevW + 1 + idW + 1
		for _, c := range p.Checks {
			sev := Severity(string(c.Severity))
			if colour.Enabled {
				// pad, then paint: the width must never count escape bytes.
				sev = Severity(string(c.Severity)) + strings.Repeat(" ", sevW-len(string(c.Severity)))
			} else {
				sev = fmt.Sprintf("%-*s", sevW, string(c.Severity))
			}
			id := Dim(fmt.Sprintf("%-*s", idW, c.ID))
			head := fmt.Sprintf("  %s %s ", sev, id)
			lines := wrapText(c.Title, width-len(head))
			if len(lines) == 0 {
				fmt.Fprintf(&b, "%s\n", strings.TrimRight(head, " "))
			} else {
				fmt.Fprintf(&b, "%s%s\n", head, lines[0])
				for _, line := range lines[1:] {
					fmt.Fprintf(&b, "%s%s\n", strings.Repeat(" ", len(head)), line)
				}
			}
			if c.Detail != "" {
				writeWrapped(&b, bodyIndent, "", "", c.Detail, width)
			}
			if c.Remediation != "" {
				// A remedy that needs a query to act on cannot be one sentence:
				// the lines it already has are kept, and each is wrapped, so the
				// SQL stays copyable instead of dissolving into prose.
				for i, line := range strings.Split(strings.TrimSpace(c.Remediation), "\n") {
					if i == 0 {
						writeWrapped(&b, bodyIndent, "action: ", "", line, width)
						continue
					}
					writeWrapped(&b, bodyIndent, "", "", line, width)
				}
			}
			if c.MySQLError > 0 {
				fmt.Fprintf(&b, "%smysql: ERROR %d\n", strings.Repeat(" ", bodyIndent), c.MySQLError)
			}
		}
	}

	b.WriteString("\nRECOVERY\n")
	recovery := []struct{ k, v string }{
		{"Before execution:", p.Recovery.BeforeExecution},
		{"During execution:", p.Recovery.DuringExecution},
		{"After success:", p.Recovery.AfterSuccess},
	}
	for _, r := range recovery {
		if r.v == "" {
			continue
		}
		writeWrapped(&b, 2, fmt.Sprintf("%-18s ", r.k), "", r.v, width)
	}
	if p.Recovery.HoldCutover {
		writeWrapped(&b, 2, "Hold cutover:     ", "", "supported; the production table is not swapped until you run 'saksama cutover'", width)
	}

	if len(p.Verification) > 0 {
		b.WriteString("\nVERIFY AFTER\n")
		kindW := 0
		for _, v := range p.Verification {
			if len(v.Kind) > kindW {
				kindW = len(v.Kind)
			}
		}
		for _, v := range p.Verification {
			fmt.Fprintf(&b, "  %-*s  %s\n", kindW, v.Kind, v.Name)
		}
	}

	if len(p.Notes) > 0 {
		b.WriteString("\nNOTES\n")
		for _, n := range p.Notes {
			writeWrapped(&b, 2, "- ", "- ", n, width)
		}
	}

	// The verdict is the last thing on screen and the first thing anyone reads, so
	// it uses the same label column as the header instead of its own.
	b.WriteString("\n")
	summary := func(k, v string) {
		lines := wrapText(v, width-labelWidth-1)
		if len(lines) == 0 {
			fmt.Fprintf(&b, "%-*s\n", labelWidth, k)
			return
		}
		fmt.Fprintf(&b, "%-*s %s\n", labelWidth, k, lines[0])
		for _, line := range lines[1:] {
			fmt.Fprintf(&b, "%s %s\n", strings.Repeat(" ", labelWidth), line)
		}
	}

	summary("RECOMMEND", string(p.Recommendation.Engine))
	if len(p.Recommendation.Fallbacks) > 0 {
		var fb []string
		for _, f := range p.Recommendation.Fallbacks {
			fb = append(fb, string(f))
		}
		summary("FALLBACKS", strings.Join(fb, ", ")+" (manual override only)")
	}
	summary("BLOCKERS", fmt.Sprintf("%d", p.Recommendation.Blockers))
	switch {
	case p.Recommendation.Blockers > 0:
		summary("RESULT", Bad("refused"))
	case !p.Recommendation.Executable:
		summary("RESULT", "engine fits, but cannot run here yet")
		summary("REASON", p.Recommendation.ExecutableReason)
	case p.Recommendation.Engine == analyzer.EngineNone:
		summary("RESULT", "no execution path selected")
	default:
		summary("RESULT", Good("ready")+" (nothing has been changed)")
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// operationMethod is the OPERATIONS column: the algorithm, its lock, and whether the
// statement rewrites every row.
func operationMethod(op analyzer.OpPlan) string {
	m := string(op.Method)
	if op.Lock != "" {
		m += ", LOCK=" + op.Lock
	}
	if op.Rebuild {
		m += " +rebuild"
	}
	return m
}

func engineLine(p analyzer.Plan) string {
	eng := string(p.Recommendation.Engine)
	if p.Recommendation.Engine == analyzer.EngineNone {
		return "none (blocked)"
	}
	// The per-operation method only describes what the server *could* do. When a
	// copy engine runs, the method is the copy, so showing INSTANT there would be
	// actively misleading.
	if p.Recommendation.Engine != analyzer.EngineNative {
		label := fmt.Sprintf("%s [table copy] (%s)", eng, p.Server.Version)
		if p.Recommendation.EngineSource != "" && p.Recommendation.EngineSource != "recommendation" {
			label += " chosen by " + p.Recommendation.EngineSource
		}
		return label
	}
	var methods []string
	for _, op := range p.Operations {
		m := string(op.Method)
		if op.Lock != "" {
			m += ", LOCK=" + op.Lock
		}
		methods = append(methods, m)
	}
	if p.Recommendation.EngineSource != "" && p.Recommendation.EngineSource != "recommendation" {
		eng += " (chosen by " + p.Recommendation.EngineSource + ")"
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

// enginesLine is the one-line summary of the engine matrix. The full breakdown
// lives in `saksama advise`.
func enginesLine(p analyzer.Plan) string {
	if len(p.Engines) == 0 {
		return ""
	}
	var parts []string
	for _, a := range p.Engines {
		switch {
		case a.Verdict != analyzer.VerdictSuitable:
			parts = append(parts, fmt.Sprintf("%s no", a.Engine))
		case !a.Installed:
			parts = append(parts, fmt.Sprintf("%s not-installed", a.Engine))
		case !a.Wrapped:
			parts = append(parts, fmt.Sprintf("%s no-adapter", a.Engine))
		default:
			parts = append(parts, fmt.Sprintf("%s yes", a.Engine))
		}
	}
	return strings.Join(parts, " | ") + "   (see: saksama advise)"
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
