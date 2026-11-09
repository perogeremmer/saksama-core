package render

import (
	"bytes"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBusyAnimatesAndClearsItsLine(t *testing.T) {
	var out bytes.Buffer
	b := NewBusy(&out, true)

	stop := b.Start("reading the schema")
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

func TestBusyIsSilentWhenNotEnabled(t *testing.T) {
	var out bytes.Buffer
	b := NewBusy(&out, false)
	stop := b.Start("connecting")
	stop()
	if out.String() != "" {
		t.Fatalf("a disabled indicator wrote %q", out.String())
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
		if got := Elapsed(time.Now().Add(-c.d)); got != c.want {
			t.Errorf("Elapsed(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// With the indicator off, Line is a plain line: the progress output of a captured
// run must be exactly what it always was.
func TestLineIsPlainWhenIdle(t *testing.T) {
	var out bytes.Buffer
	b := NewBusy(&out, true)
	b.Line("  [%5.1f%%] copying", 42.0)
	if got := out.String(); got != "  [ 42.0%] copying\n" {
		t.Fatalf("an idle Line wrote %q", got)
	}
}

// The contract the apply output depends on: the moving frame and the engine's
// progress lines share one stream, and neither may write into the middle of the
// other. A torn line here is a percentage the operator cannot read.
func TestFrameAndLineNeverShareALine(t *testing.T) {
	var out safeBuffer
	b := NewBusy(&out, true)

	stop := b.Start("copying")
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			b.Line("  [%5.1f%%] note %d", float64(n)/10, n)
		}(i)
	}
	wg.Wait()
	stop()

	got := out.String()
	// A spinner frame is never terminated by a newline, so every completed line
	// is either a note or an empty leftover. Each note must be intact: cleared,
	// then the whole line, then the newline - with no frame text inside it.
	lineRe := regexp.MustCompile(`^\r\x1b\[K  \[\s*\d+\.\d%\] note \d+$`)
	seen := map[string]bool{}
	for _, line := range strings.Split(got, "\n") {
		if !strings.Contains(line, "note") {
			continue
		}
		if !lineRe.MatchString(line) {
			t.Fatalf("a torn progress line: %q", line)
		}
		if strings.Contains(line, "copying") {
			t.Fatalf("frame text landed inside a progress line: %q", line)
		}
		seen[line] = true
	}
	if len(seen) != 200 {
		t.Fatalf("expected 200 intact notes, got %d distinct lines", len(seen))
	}
}

// safeBuffer so the indicator goroutine and the test can both touch the buffer.
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
