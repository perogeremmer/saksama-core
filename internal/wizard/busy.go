package wizard

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// busy frames. Plain ASCII, because the wizard is the only thing that knows it has a
// terminal and even here the output can be captured with tee.
var busyFrames = []string{"|", "/", "-", "\\"}

// busy is a one-line activity indicator for the wait before an answer.
//
// The wizard spends real time waiting: opening a connection, reading a schema, planning
// against a live server. Silence there is indistinguishable from a hang, and the fix
// costs one line of moving text. It never prints to a non-terminal, never leaves a
// half-written line behind, and always shows elapsed seconds so "slow" and "stuck" look
// different.
type busy struct {
	out     io.Writer
	label   string
	enabled bool

	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
	start time.Time
}

// busy starts the indicator and returns a function that stops it. The stop function is
// safe to call twice and must be called before printing the result, so the result lands
// on a clean line.
func (w *Wizard) busy(label string) func() {
	b := &busy{
		out:     w.out,
		label:   label,
		enabled: w.animate,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		start:   time.Now(),
	}
	if !b.enabled {
		close(b.done)
		return func() {}
	}
	go b.loop()
	return func() { b.finish() }
}

func (b *busy) loop() {
	defer close(b.done)
	ticker := time.NewTicker(120 * time.Millisecond)
	defer ticker.Stop()
	for i := 0; ; i++ {
		select {
		case <-b.stop:
			return
		case <-ticker.C:
			frame := busyFrames[i%len(busyFrames)]
			// \r returns to the column, \x1b[K clears the rest of the line so a
			// shorter frame never leaves debris from a longer one.
			fmt.Fprintf(b.out, "\r\x1b[K  %s %s %s", frame, b.label, elapsed(b.start))
		}
	}
}

func (b *busy) finish() {
	b.once.Do(func() {
		close(b.stop)
		<-b.done
		fmt.Fprint(b.out, "\r\x1b[K")
	})
}

func elapsed(start time.Time) string {
	d := time.Since(start)
	switch {
	case d < time.Second:
		return fmt.Sprintf("(%dms)", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("(%.1fs)", d.Seconds())
	default:
		return fmt.Sprintf("(%dm%02ds)", int(d.Minutes()), int(d.Seconds())%60)
	}
}

// note prints a short completion line for something that finished quickly enough that
// the indicator never appeared, so every action leaves a trace either way.
func (w *Wizard) note(label string, start time.Time) {
	if !w.animate {
		return
	}
	fmt.Fprintf(w.out, "  %s %s\n", strings.TrimRight(label, " "), elapsed(start))
}
