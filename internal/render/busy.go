package render

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// busyFrames. Plain ASCII, because the output can be captured with tee.
var busyFrames = []string{"|", "/", "-", "\\"}

// Busy is a one-line activity indicator and the writer for the progress lines
// that share its stream.
//
// The wait before an answer is indistinguishable from a hang, and the fix costs
// one line of moving text. `apply` is the other user: the engine prints its own
// progress lines on the same stream, and a frame and a note written without
// coordination would fight for the terminal, the loser being whichever wrote
// last. Busy owns that line: the frame and the note each take the same lock and
// each begin by clearing the line, so neither can write into the middle of the
// other.
type Busy struct {
	out     io.Writer
	enabled bool

	mu     sync.Mutex
	cancel func() // stops the current run; nil when idle
}

// NewBusy returns an indicator for out. enabled gates only the moving frame:
// the progress lines are content, and a captured session must still get them.
func NewBusy(out io.Writer, enabled bool) *Busy {
	return &Busy{out: out, enabled: enabled}
}

// Start begins the indicator and returns the function that stops it. The stop
// function is safe to call twice, and must be called before printing a result,
// so the result lands on a clean line. Starting a second run stops the first.
func (b *Busy) Start(label string) func() {
	b.Stop()
	if !b.enabled {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	start := time.Now()
	go b.loop(label, start, stop, done)

	var once sync.Once
	cancel := func() {
		once.Do(func() { b.finish(stop, done) })
	}
	b.mu.Lock()
	b.cancel = cancel
	b.mu.Unlock()
	return cancel
}

// Stop ends any current run and clears its line.
func (b *Busy) Stop() {
	b.mu.Lock()
	cancel := b.cancel
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (b *Busy) finish(stop chan struct{}, done chan struct{}) {
	close(stop)
	<-done
	b.mu.Lock()
	defer b.mu.Unlock()
	fmt.Fprint(b.out, "\r\x1b[K")
	b.cancel = nil
}

func (b *Busy) loop(label string, start time.Time, stop chan struct{}, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(120 * time.Millisecond)
	defer ticker.Stop()
	for i := 0; ; i++ {
		select {
		case <-stop:
			return
		case <-ticker.C:
			frame := busyFrames[i%len(busyFrames)]
			// \r returns to the column, \x1b[K clears the rest of the line so a
			// shorter frame never leaves debris from a longer one.
			b.write(fmt.Sprintf("\r\x1b[K  %s %s %s", frame, label, Elapsed(start)))
		}
	}
}

// Line writes one progress line on the indicator's stream. When the indicator is
// running it clears the moving line first, so the note starts at the column and
// the next frame draws below it; when it is not, this is a plain line, which is
// what a captured session gets.
func (b *Busy) Line(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancel == nil {
		fmt.Fprint(b.out, text+"\n")
		return
	}
	fmt.Fprint(b.out, "\r\x1b[K"+text+"\n")
}

// write emits one complete frame under the lock, so a concurrent progress line
// cannot land inside it.
func (b *Busy) write(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fmt.Fprint(b.out, s)
}

// Elapsed formats a duration the way the indicator reports it, so "slow" and
// "stuck" look different at a glance.
func Elapsed(start time.Time) string {
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
