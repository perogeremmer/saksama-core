package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
)

// Environment renders the server-settings doctor report.
//
// This is the part people skip and then lose an afternoon to: an engine can be
// perfectly suited to a table and still refuse to start because binary logging is
// off. Showing the current value next to the required one turns that into a
// two-second read.
func Environment(w io.Writer, checks []analyzer.SettingCheck, border bool) error {
	if len(checks) == 0 {
		return nil
	}

	var b strings.Builder
	b.WriteString("SERVER SETTINGS (what the engines require, against what this server reports)\n")

	allUnknown := true
	for _, c := range checks {
		if c.Status != analyzer.EnvUnknown {
			allUnknown = false
		}
	}
	if allUnknown {
		b.WriteString("\n  not inspected: plan with --live to read these from the server.\n")
		if _, err := io.WriteString(w, b.String()); err != nil {
			return err
		}
		return nil
	}

	t := &Table{
		Border:   border,
		MaxWidth: 34,
		Columns: []Column{
			{Header: "SETTING"}, {Header: "CURRENT"}, {Header: "REQUIRED"},
			{Header: "STATUS"}, {Header: "NEEDED BY"}, {Header: "WHY"},
		},
	}
	for _, c := range checks {
		t.Add(c.Setting, c.Current, c.Required, statusMark(c.Status), joinEngines(c.NeededBy), c.Note)
	}
	if err := t.Render(&b); err != nil {
		return err
	}

	// The table itself stays plain on purpose: its column widths are measured from
	// cell text, and an escape sequence counted as characters would misalign every
	// row or truncate mid-sequence. Colour goes where the layout cannot be hurt.
	fails, warns, unknown := analyzer.EnvironmentSummary(checks)
	fmt.Fprintf(&b, "\n  %s, %s, %s\n",
		tally(fails, "blocking", Bad), tally(warns, "advisory", Warn), tally(unknown, "unknown", Dim))

	if hints := analyzer.FixHints(checks); len(hints) > 0 {
		b.WriteString("\n" + Bad("FIX FIRST") + "\n")
		for _, h := range hints {
			fmt.Fprintf(&b, "  - %s\n", h)
		}
	}
	b.WriteString("\n  these settings decide whether an engine can start, not whether it suits the table.\n")
	b.WriteString("  both matter: see ENGINE SUITABILITY below.\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// tally writes "3 blocking", coloured by what the number means. Zero stays plain: a
// count of nothing is not a warning.
func tally(n int, noun string, paint func(string) string) string {
	text := fmt.Sprintf("%d %s", n, noun)
	if n == 0 {
		return text
	}
	return paint(text)
}

// statusMark is a short ASCII marker so the column scans vertically.
func statusMark(s analyzer.EnvStatus) string {
	switch s {
	case analyzer.EnvOK:
		return "ok"
	case analyzer.EnvFail:
		return "FAIL"
	case analyzer.EnvWarn:
		return "warn"
	}
	return "?"
}

func joinEngines(engines []analyzer.Engine) string {
	if len(engines) == 0 {
		return "advisory"
	}
	parts := make([]string, 0, len(engines))
	for _, e := range engines {
		parts = append(parts, string(e))
	}
	return strings.Join(parts, ", ")
}

// EngineEnvironmentTable renders the per-engine prerequisite list, for the
// single-table report where space allows the detail.
func EngineEnvironmentTable(w io.Writer, e analyzer.EngineAssessment, border bool) error {
	if len(e.Environment) == 0 {
		return nil
	}
	t := &Table{
		Border:   border,
		MaxWidth: 52,
		Columns: []Column{
			{Header: "PREREQUISITE"}, {Header: "STATUS"}, {Header: "DETAIL"},
		},
	}
	for _, r := range e.Environment {
		status := "ok"
		if !r.Met {
			status = "FAIL"
		}
		t.Add(r.Name, status, r.Detail)
	}
	return t.Render(w)
}

var _ = fmt.Sprintf
