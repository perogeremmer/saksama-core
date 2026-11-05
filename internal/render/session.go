package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/state"
)

// Session prints the durable record of one migration.
//
// It is a renderer, not a decision: everything shown here already happened, and
// the ordering (state, statements, verification, artifacts, error) follows the
// order a person reads them when something went wrong.
func Session(w io.Writer, s *state.Session) error {
	var b strings.Builder

	fmt.Fprintf(&b, "SESSION    %s\n", s.ID)
	fmt.Fprintf(&b, "STATE      %s", s.State)
	if s.Phase != "" {
		fmt.Fprintf(&b, " (%s)", s.Phase)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "TARGET     %s.%s @ %s\n", s.Database, s.Table, s.Host)
	fmt.Fprintf(&b, "ALTER      %s\n", s.Alter)
	fmt.Fprintf(&b, "ENGINE     %s\n", s.Engine)
	if s.Actor != "" {
		fmt.Fprintf(&b, "ACTOR      %s\n", s.Actor)
	}

	// A blocked session has to show the plan's findings: `apply` points here for
	// "what to do about each blocker", so this is where that has to be true. The
	// stored plan is the same document the operator saw at plan time.
	if blocked := storedFindings(s.Plan); len(blocked) > 0 {
		b.WriteString("\nBLOCKERS\n")
		for _, f := range blocked {
			fmt.Fprintf(&b, "  [%s] %s\n", f.ID, f.Title)
			if f.Detail != "" {
				fmt.Fprintf(&b, "         %s\n", oneLine(f.Detail))
			}
			if f.Remediation != "" {
				for i, line := range strings.Split(strings.TrimSpace(f.Remediation), "\n") {
					label := "         "
					if i == 0 {
						label += "action: "
					}
					fmt.Fprintf(&b, "%s%s\n", label, oneLine(line))
				}
			}
		}
	}

	if len(s.Approvals) > 0 {
		b.WriteString("\nAPPROVALS\n")
		for _, a := range s.Approvals {
			fmt.Fprintf(&b, "  %s by %s  %s\n", a.Action, a.Actor, a.Note)
		}
	}

	if len(s.Statements) > 0 {
		b.WriteString("\nSTATEMENTS\n")
		for i, st := range s.Statements {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, st.SQL)
			if st.DurationMS > 0 || st.RowsAffected > 0 {
				fmt.Fprintf(&b, "     %dms, %d rows affected\n", st.DurationMS, st.RowsAffected)
			}
			if st.Error != "" {
				fmt.Fprintf(&b, "     error: %s\n", st.Error)
			}
		}
	}

	if len(s.Verification) > 0 {
		b.WriteString("\nVERIFICATION\n")
		for _, v := range s.Verification {
			mark := "ok  "
			if !v.OK {
				mark = "FAIL"
			}
			fmt.Fprintf(&b, "  [%s] %-20s %s  %s\n", mark, v.Kind, v.Name, v.Detail)
		}
	}

	if len(s.Artifacts) > 0 {
		b.WriteString("\nOWNED ARTIFACTS\n")
		for _, a := range s.Artifacts {
			ownership := "kept for recovery"
			if a.SafeToDelete {
				ownership = "deletable"
			}
			fmt.Fprintf(&b, "  %-8s %-32s %s (%s)\n", a.Kind, a.Name, ownership, a.Engine)
		}
	}

	if s.CutoverFlagFile != "" {
		fmt.Fprintf(&b, "\nCUTOVER    held; release with: saksama cutover %s\n", s.ID)
	}

	if s.Error != "" {
		fmt.Fprintf(&b, "\nERROR      %s\n", s.Error)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// storedFindings reads the blockers out of a session's stored plan. A session that
// was refused kept the whole plan, which is the only place the remedies live.
func storedFindings(raw json.RawMessage) []struct {
	ID          string
	Title       string
	Detail      string
	Remediation string
} {
	if len(raw) == 0 {
		return nil
	}
	var plan struct {
		Checks []struct {
			ID          string `json:"id"`
			Severity    string `json:"severity"`
			Title       string `json:"title"`
			Detail      string `json:"detail"`
			Remediation string `json:"remediation"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil
	}
	var out []struct {
		ID          string
		Title       string
		Detail      string
		Remediation string
	}
	for _, c := range plan.Checks {
		if c.Severity != "BLOCKER" {
			continue
		}
		out = append(out, struct {
			ID          string
			Title       string
			Detail      string
			Remediation string
		}{c.ID, c.Title, c.Detail, c.Remediation})
	}
	return out
}
