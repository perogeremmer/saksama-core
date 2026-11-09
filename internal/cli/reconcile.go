package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/perogeremmer/saksama-core/internal/core"
	"github.com/perogeremmer/saksama-core/internal/render"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// cmdReconcile reports sessions that never reached a terminal state, and what the
// database says about them now.
//
// This is the honest answer after a crash: not "it failed", not "it is running",
// but "here is what exists, and here is your next step".
func cmdReconcile(args []string, stdout, stderr io.Writer) int {
	var conn connFlags

	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addConnFlags(fs, &conn)
	jsonOut := fs.Bool("json", false, "JSON output")
	retire := fs.String("retire", "",
		"mark this session failed and release its table lock; only works once the engine process is confirmed gone")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama reconcile: %v\n", err)
		return ExitInternal
	}

	// Retiring is the answer to reconcile's own advice, and it needs no database:
	// the engine is gone by definition, so the observation happened already.
	if *retire != "" {
		runner := &core.Runner{Store: store}
		res, err := runner.Retire(context.Background(), *retire)
		if err != nil {
			fmt.Fprintf(stderr, "saksama reconcile --retire: %v\n", err)
			return classifyStoreError(err)
		}
		// The exit code follows the outcome, like cancel: a live engine is
		// refused (cancel it instead), an ended session is a no-op.
		code := ExitOK
		if res.Outcome == core.RetireEngineAlive {
			code = ExitUsage
		}
		fmt.Fprintln(stdout, res.Note)
		return code
	}

	c, err := conn.resolve()
	if err != nil {
		fmt.Fprintf(stderr, "saksama reconcile: %v\n", err)
		return ExitUsage
	}
	db, err := openDB(c)
	if err != nil {
		fmt.Fprintf(stderr, "saksama reconcile: %v\n", err)
		return ExitInternal
	}
	defer db.Close()

	runner := core.New(db, store)
	findings, err := runner.Reconcile(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "saksama reconcile: %v\n", err)
		return ExitInternal
	}

	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(findings)
		return ExitOK
	}

	if len(findings) == 0 {
		fmt.Fprintln(stdout, "no session is in an unfinished state")
		// A corrupt record would otherwise make this the answer while artifacts
		// sit on the database.
		unreadableNote(store, stdout)
		return ExitOK
	}

	table := render.Table{
		Border:   true,
		MaxWidth: 44,
		Columns: []render.Column{
			{Header: "SESSION"}, {Header: "TABLE"}, {Header: "STATE"},
			{Header: "ENGINE"}, {Header: "LEFTOVERS"}, {Header: "NEXT"},
		},
	}
	for _, f := range findings {
		engineState := "gone"
		if f.EngineAlive {
			engineState = "alive"
		}
		leftovers := fmt.Sprint(len(f.Observed))
		if len(f.Observed) > 0 {
			leftovers = fmt.Sprintf("%d: %v", len(f.Observed), f.Observed)
		}
		table.Add(f.Session, f.Table, string(f.State), engineState, leftovers, f.Verdict)
	}
	_ = table.Render(stdout)

	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Nothing was changed. A session in an unfinished state has to be observed")
	fmt.Fprintln(stdout, "before anyone touches it, because it is neither obviously failed nor")
	fmt.Fprintln(stdout, "obviously running.")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "  alive engine      saksama attach <session>   or   saksama cancel <session>")
	fmt.Fprintln(stdout, "  leftovers         saksama cleanup <session> --dry-run, then cleanup")
	fmt.Fprintln(stdout, "  nothing left      saksama reconcile --retire <session>, then re-plan")
	return ExitOK
}

var _ = state.StateRunning
