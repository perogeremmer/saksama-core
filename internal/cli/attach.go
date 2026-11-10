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
	if code, done := parseFlags(fs, flagArgs); done {
		return code
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
			return classifyStoreError(err)
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
	defer signal.Stop(sig)
	go func() {
		select {
		case <-sig:
			cancel()
			// The first interrupt detaches; a second one means the operator wants
			// out even if the read loop is momentarily stuck. With Notify still
			// registered and the handler goroutine gone, the signal would land in
			// a channel nobody reads and there would be no escape but kill -9.
			signal.Stop(sig)
			<-sig
			os.Exit(ExitInternal)
		case <-stopped:
		}
	}()

	s := stream.Follow(ctx, path, *since)
	ended := false
	for ev := range s.Events {
		show(ev)
		switch ev.Type {
		case events.TypeSessionCompleted, events.TypeSessionFailed, events.TypeSessionCancelled:
			// The migration has told its whole story. Ending here is what a
			// consumer of --json needs - a CI step piping this command must not
			// wait forever after the session is over - and a plain-text reader
			// loses nothing by returning instead of waiting for ctrl-c.
			ended = true
		}
		if ended {
			cancel()
			break
		}
	}
	if err, ok := <-s.Err; ok && err != nil {
		// A typo in the session id is the operator's to fix; anything else is ours.
		fmt.Fprintf(stderr, "saksama attach: %v\n", err)
		return classifyStoreError(err)
	}
	if ended {
		return ExitOK
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
	force := fs.Bool("force-kill", false,
		"send SIGKILL if the engine ignores SIGTERM; the engine then cannot drop its own shadow table")
	positional, flagArgs := extractPositionals(args, nil)
	if code, done := parseFlags(fs, flagArgs); done {
		return code
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
	res, err := runner.Cancel(context.Background(), positional[0], *force)
	if err != nil {
		fmt.Fprintf(stderr, "saksama cancel: %v\n", err)
		return classifyStoreError(err)
	}

	// The exit code is decided by the outcome, before the output mode chooses how
	// the outcome is described: --json and plain text must be one contract.
	code := ExitOK
	switch res.Outcome {
	case core.CancelNoEngine, core.CancelNotRunning:
		code = ExitUsage
	}

	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		return code
	}

	fmt.Fprintln(stdout, res.Note)
	if code != ExitOK {
		return code
	}
	switch res.Outcome {
	case core.CancelStopped:
		fmt.Fprintln(stdout, "the production table was not swapped if the engine had not cut over yet.")
		fmt.Fprintf(stdout, "check for leftovers: saksama status %s\n", res.ID)
		fmt.Fprintf(stdout, "remove them once you have checked: saksama cleanup %s\n", res.ID)
	}
	return ExitOK
}
