package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/enginesetup"
)

// Doctor writes the readiness checklist for one or more engines.
//
// It is a checklist, not a table: each line is a fact someone has to act on, and
// the fix belongs next to the failure rather than in a legend.
func Doctor(w io.Writer, lists []enginesetup.Checklist, border bool) error {
	var b strings.Builder

	for i, c := range lists {
		if i > 0 {
			b.WriteString("\n")
		}
		evidence := c.Evidence
		if evidence == "" {
			evidence = "unrecorded"
		}
		fmt.Fprintf(&b, "%s %s (%s)\n", strings.ToUpper(c.Engine), orDash(c.Version), evidence)
		if c.Target != "" {
			fmt.Fprintf(&b, "  this machine  %s\n", c.Target)
		}

		nameWidth := 0
		for _, it := range c.Items {
			nameWidth = maxInt(nameWidth, len(it.Name))
		}

		for _, it := range c.Items {
			fmt.Fprintf(&b, "  [%s] %-*s  %s\n", stateMark(it.State), nameWidth, it.Name, it.Detail)
			if it.Fix != "" {
				fmt.Fprintf(&b, "  %-*s  -> %s\n", len("[    ]")+1+nameWidth, "", it.Fix)
			}
		}

		b.WriteString("\n")
		fmt.Fprintf(&b, "  verdict  %s\n", c.Verdict)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// stateMark keeps the marks the same width so the columns line up: a FAIL next to
// an ok has to be visible from across the room.
func stateMark(state string) string {
	switch state {
	case enginesetup.StateOK:
		return "  ok"
	case enginesetup.StateMissing:
		return "FAIL"
	}
	return "  ??"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
