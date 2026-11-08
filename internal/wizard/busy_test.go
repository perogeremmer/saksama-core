package wizard

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// safeBuffer so the spinner goroutine and the test can both touch the buffer.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The spinner is the difference between "working" and "hung". It must animate while a
// wait is real, and it must leave no debris afterwards.
func TestBusyAnimatesAndClearsItsLine(t *testing.T) {
	out := &safeBuffer{}
	w := &Wizard{out: out, animate: true}

	stop := w.busy("reading the schema")
	time.Sleep(400 * time.Millisecond)
	stop()

	got := out.String()
	if !strings.Contains(got, "reading the schema") {
		t.Fatalf("the label must be shown:\n%q", got)
	}
	if !strings.Contains(got, "\r") {
		t.Fatalf("the indicator must return to the start of the line:\n%q", got)
	}
	if !strings.HasSuffix(got, "\r\x1b[K") {
		t.Fatalf("the last write must clear the line, so the result lands clean:\n%q", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("the indicator must never scroll the screen:\n%q", got)
	}
	// Stopping twice must be safe: every caller defers it.
	stop()
}

// In a captured session there is no cursor to move, so there must be nothing at all.
func TestBusyIsSilentWhenNotAnimating(t *testing.T) {
	out := &safeBuffer{}
	w := &Wizard{out: out}
	stop := w.busy("connecting")
	stop()
	if out.String() != "" {
		t.Fatalf("a non-animated wizard wrote %q", out.String())
	}
}

func TestElapsedReadsLikeTime(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{120 * time.Millisecond, "(120ms)"},
		{2300 * time.Millisecond, "(2.3s)"},
		{75 * time.Second, "(1m15s)"},
	} {
		if got := elapsed(time.Now().Add(-c.d)); got != c.want {
			t.Errorf("elapsed(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
