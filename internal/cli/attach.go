package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/perogeremmer/saksama-core/internal/core"
	"github.com/perogeremmer/saksama-core/internal/engine/process"
	"github.com/perogeremmer/saksama-core/internal/events"
	"github.com/perogeremmer/saksama-core/internal/render"
	"github.com/perogeremmer/saksama-core/internal/stream"
)

// cmdAttach follows a session's event log and prints it as it happens.
//
// It reads the append-only log on disk rather than a live socket, which means it
// works from another terminal, after a reconnect, or hours later. The engine does
// not depend on this command existing.
func cmdAttach(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	follow := fs.Bool("follow", true, "keep following as new events arrive")
	raw := fs.Bool("json", false, "print raw JSONL instead of a summary")
	since := fs.Int64("since", 0, "skip events at or below this sequence number")
	// Flags must work on either side of the session id: `attach s1 --since 4` is
	// how people type it, and the standard flag package would silently stop at s1
	// and keep following (with --follow=false ignored, that is a hang).
	positional, flagArgs := extractPositionals(args, map[string]bool{"since": true})
	if err := fs.Parse(flagArgs); err != nil {
		return ExitUsage
	}
	if len(positional) == 0 {
		fmt.Fprintln(stderr, "saksama attach: session id required")
		return ExitUsage
	}

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama attach: %v\n", err)
		return ExitInternal
	}
	id := positional[0]
	path := store.EventLogPath(id)

	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	show := func(ev events.Event) {
		if ev.Seq <= *since {
			return
		}
		if *raw {
			_ = enc.Encode(ev)
			return
		}
		if line := render.EventLine(ev); line != "" {
			fmt.Fprintln(stdout, line)
		}
	}

	if !*follow {
		stored, err := stream.Read(path)
		if err != nil {
			fmt.Fprintf(stderr, "saksama attach: %v\n", err)
			return ExitInternal
		}
		for _, ev := range stored {
			show(ev)
		}
		return ExitOK
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-sig:
			cancel()
		case <-stopped:
		}
	}()

	s := stream.Follow(ctx, path, *since)
	for ev := range s.Events {
		show(ev)
	}
	if err, ok := <-s.Err; ok && err != nil {
		fmt.Fprintf(stderr, "saksama attach: %v\n", err)
		return ExitInternal
	}

	// Ctrl-C detaches; it does not stop anything on the server.
	if sess, err := store.Load(id); err == nil && !sess.State.Terminal() && process.Alive(sess.EnginePID) {
		fmt.Fprintf(stdout, "\ndetached. session %s is still %s; the engine (pid %d) keeps running.\n", id, sess.State, sess.EnginePID)
		fmt.Fprintf(stdout, "re-attach with: saksama attach %s\n", id)
	}
	return ExitOK
}

// cmdCancel stops a running migration by its recorded process id.
func cmdCancel(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "JSON output")
	positional, flagArgs := extractPositionals(args, nil)
	if err := fs.Parse(flagArgs); err != nil {
		return ExitUsage
	}
	if len(positional) == 0 {
		fmt.Fprintln(stderr, "saksama cancel: session id required")
		return ExitUsage
	}

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama cancel: %v\n", err)
		return ExitInternal
	}

	runner := &core.Runner{Store: store}
	res, err := runner.Cancel(context.Background(), positional[0])
	if err != nil {
		fmt.Fprintf(stderr, "saksama cancel: %v\n", err)
		return ExitUsage
	}

	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		return ExitOK
	}

	fmt.Fprintln(stdout, res.Note)
	switch res.Outcome {
	case core.CancelStopped:
		fmt.Fprintln(stdout, "the production table was not swapped if the engine had not cut over yet.")
		fmt.Fprintf(stdout, "check for leftovers: saksama status %s\n", res.ID)
		fmt.Fprintf(stdout, "remove them once you have checked: saksama cleanup %s\n", res.ID)
	case core.CancelNoEngine, core.CancelNotRunning:
		return ExitUsage
	}
	return ExitOK
}
