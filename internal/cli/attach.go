package cli

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/perogeremmer/saksama-core/internal/engine/process"
	"github.com/perogeremmer/saksama-core/internal/events"
	"github.com/perogeremmer/saksama-core/internal/state"
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
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "saksama attach: session id required")
		return ExitUsage
	}

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama attach: %v\n", err)
		return ExitInternal
	}
	id := fs.Arg(0)
	logPath := store.EventLogPath(id)

	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		close(stop)
	}()

	if err := followEvents(logPath, stdout, *raw, *follow, stop); err != nil {
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

func followEvents(path string, out io.Writer, raw, follow bool, stop <-chan struct{}) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("no event log at %s yet: %w", path, err)
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			if !raw {
				if summary := summariseEvent(line); summary != "" {
					fmt.Fprintln(out, summary)
				}
			} else {
				fmt.Fprint(out, line)
			}
		}
		if err == io.EOF {
			if !follow {
				return nil
			}
			select {
			case <-stop:
				return nil
			case <-time.After(300 * time.Millisecond):
			}
			continue
		}
		if err != nil {
			return err
		}
	}
}

// summariseEvent turns one JSONL line into the single sentence an operator needs.
func summariseEvent(line string) string {
	var ev events.Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &ev); err != nil {
		return ""
	}
	get := func(k string) string {
		if v, ok := ev.Data[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
			return fmt.Sprint(v)
		}
		return ""
	}

	switch ev.Type {
	case events.TypeSessionCreated:
		return fmt.Sprintf("session %s created for %s.%s", ev.Session, get("database"), get("table"))
	case events.TypePlanComputed:
		return fmt.Sprintf("plan: engine %s, %s blocker(s)", get("engine"), get("blockers"))
	case events.TypePhaseStarted:
		return fmt.Sprintf("phase %s started", get("phase"))
	case events.TypePhaseProgress:
		return fmt.Sprintf("phase %s: %s%% %s", get("phase"), trimFloat(get("percent")), get("note"))
	case events.TypePhaseCompleted:
		return fmt.Sprintf("phase %s done", get("phase"))
	case events.TypePhaseFailed:
		return fmt.Sprintf("phase %s FAILED: %s", get("phase"), get("error"))
	case events.TypeEngineStarted:
		return fmt.Sprintf("engine %s started (pid %s) %s", get("engine"), get("pid"), get("note"))
	case events.TypeCheckFired:
		return fmt.Sprintf("[%s] %s %s", get("severity"), get("id"), get("title"))
	case events.TypeSessionCompleted:
		return fmt.Sprintf("session %s completed, %s statement(s), %s verified", ev.Session, get("statements"), get("verified"))
	case events.TypeSessionFailed:
		return fmt.Sprintf("session %s FAILED: %s", ev.Session, get("error"))
	case events.TypeApprovalGranted:
		return fmt.Sprintf("approved by %s: %s", get("actor"), get("note"))
	}
	return ""
}

func trimFloat(s string) string {
	if i := strings.IndexByte(s, '.'); i > 0 && len(s) > i+2 {
		return s[:i+2]
	}
	return s
}

// cmdCancel stops a running migration by its recorded process id.
func cmdCancel(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "saksama cancel: session id required")
		return ExitUsage
	}

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama cancel: %v\n", err)
		return ExitInternal
	}
	id := fs.Arg(0)
	sess, err := store.Load(id)
	if err != nil {
		fmt.Fprintf(stderr, "saksama cancel: %v\n", err)
		return ExitUsage
	}
	if sess.State.Terminal() {
		fmt.Fprintf(stdout, "session %s already ended as %q, nothing to cancel\n", id, sess.State)
		return ExitOK
	}
	if sess.EnginePID == 0 {
		fmt.Fprintf(stderr, "session %s has no recorded engine process; it may be a native DDL the server owns\n", id)
		return ExitUsage
	}
	if !process.Alive(sess.EnginePID) {
		fmt.Fprintf(stdout, "engine pid %d is no longer running\n", sess.EnginePID)
		return ExitOK
	}

	fmt.Fprintf(stdout, "stopping engine pid %d for session %s...\n", sess.EnginePID, id)
	if err := process.Stop(sess.EnginePID, 20*time.Second); err != nil {
		fmt.Fprintf(stderr, "saksama cancel: %v\n", err)
		return ExitInternal
	}
	fmt.Fprintln(stdout, "stopped.")
	fmt.Fprintln(stdout, "the production table was not swapped if the engine had not cut over yet.")
	fmt.Fprintf(stdout, "check for leftovers: saksama status %s\n", id)
	fmt.Fprintf(stdout, "remove them once you have checked: saksama cleanup %s\n", id)
	return ExitOK
}

var _ = state.StateDone
