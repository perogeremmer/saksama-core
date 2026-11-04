// Package stream reads the append-only event log of a session.
//
// The log is the contract between the core and every frontend, so it is read in
// exactly one place: the CLI prints it, the wizard renders it, and an agent will
// forward it as server-sent events. Each of those used to parse JSONL itself,
// which is how three frontends end up disagreeing about what an event means.
package stream

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/perogeremmer/saksama-core/internal/events"
)

// PollInterval is how often a follower checks for new lines. The core flushes
// every event as it is written, so this is the only latency a viewer sees.
const PollInterval = 300 * time.Millisecond

// Read returns every event currently in the log, skipping lines that are not
// valid JSON (a crash can leave a partial line, and that must not hide the sixty
// events before it).
func Read(path string) ([]events.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readAll(f)
}

func readAll(r io.Reader) ([]events.Event, error) {
	var out []events.Event
	sc := bufio.NewScanner(r)
	// A single event carries the full plan with its checks; the default 64 KiB
	// line limit is not enough for a wide schema.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev events.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// Stream is a live feed of one session's events.
type Stream struct {
	Events <-chan events.Event
	// Err receives at most one error and is then closed. A missing log at start
	// is an error; a log that disappears mid-follow is one too.
	Err <-chan error
}

// Follow tails an event log until ctx is cancelled.
//
// afterSeq lets a viewer reconnect without replaying: pass the last sequence
// number it has already shown, and everything at or below it is skipped. That is
// the same contract a web client needs, which is why it lives here rather than in
// the terminal renderer.
func Follow(ctx context.Context, path string, afterSeq int64) *Stream {
	out := make(chan events.Event, 64)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errs)
		if err := follow(ctx, path, afterSeq, out); err != nil && !errors.Is(err, context.Canceled) {
			errs <- err
		}
	}()
	return &Stream{Events: out, Err: errs}
}

func follow(ctx context.Context, path string, afterSeq int64, out chan<- events.Event) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("no event log at %s: %w", path, err)
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 64*1024)
	pending := ""
	for {
		line, err := reader.ReadString('\n')
		pending += line
		complete := strings.HasSuffix(pending, "\n")
		if complete {
			if ev, ok := parseLine(pending); ok && ev.Seq > afterSeq {
				afterSeq = ev.Seq
				select {
				case out <- ev:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			pending = ""
		}

		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if errors.Is(err, io.EOF) {
			// A partial trailing line is normal while a write is in progress; keep
			// it and try again, rather than dropping the event.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(PollInterval):
			}
			continue
		}
	}
}

func parseLine(line string) (events.Event, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return events.Event{}, false
	}
	var ev events.Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return events.Event{}, false
	}
	return ev, true
}
