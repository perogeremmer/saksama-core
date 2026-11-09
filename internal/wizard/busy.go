package wizard

import (
	"fmt"
	"strings"
	"time"

	"github.com/perogeremmer/saksama-core/internal/render"
)

// busy runs the shared one-line indicator on the wizard's output. The wizard
// spends real time waiting: opening a connection, reading a schema, planning
// against a live server, and holding the line open while an apply runs. The
// frames, the elapsed time and the line discipline live in render, so the CLI's
// apply covers its silences with the same indicator.
func (w *Wizard) busy(label string) func() {
	return render.NewBusy(w.out, w.animate).Start(label)
}

// note prints a short completion line for something that finished quickly enough that
// the indicator never appeared, so every action leaves a trace either way.
func (w *Wizard) note(label string, start time.Time) {
	if !w.animate {
		return
	}
	fmt.Fprintf(w.out, "  %s %s\n", strings.TrimRight(label, " "), render.Elapsed(start))
}
