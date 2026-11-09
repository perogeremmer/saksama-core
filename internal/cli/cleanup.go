package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/perogeremmer/saksama-core/internal/core"
)

// cmdCleanup removes the objects a session recorded as its own.
//
// Ownership, not a name pattern: a table called `_orders_old` might belong to a
// migration, or might be something a person created deliberately, and the name
// cannot tell you which.
func cmdCleanup(args []string, stdout, stderr io.Writer) int {
	var conn connFlags

	// `--dry-run` is parsed, not sniffed out of the argument list. The old
	// spelling looked for the exact token and removed it, then handed the rest to
	// the parser - which stops at the session id, so a misspelling like `--dryrun`
	// was accepted in silence and the command went on to drop the artifacts for
	// real. A flag this one turns a rehearsal into a deletion must be exact, and an
	// unknown one must be refused.
	positional, flagArgs := extractPositionals(args, connValueFlags())

	fsNew := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	fsNew.SetOutput(stderr)
	addConnFlags(fsNew, &conn)
	dryRunFlag := fsNew.Bool("dry-run", false, "show what would be dropped, and stop (accepted in any position)")
	if err := fsNew.Parse(flagArgs); err != nil {
		return ExitUsage
	}
	if len(positional) == 0 {
		fmt.Fprintln(stderr, "saksama cleanup: session id required")
		return ExitUsage
	}

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama cleanup: %v\n", err)
		return ExitInternal
	}

	// The read-only pass decides whether a connection is needed at all. A session
	// with nothing to drop, or one whose objects a recovery still needs, must not
	// require credentials: refusing is the answer, and it is answerable offline.
	plan, err := core.New(nil, store).Cleanup(context.Background(), positional[0], true)
	if err != nil {
		fmt.Fprintf(stderr, "saksama cleanup: %v\n", err)
		// A mistyped session id is the operator's typo here too. This command used
		// to answer it with the internal-error code while every other command
		// answered 2, which is exactly the inconsistency classifyStoreError exists
		// to end - and it was the one caller that did not use it.
		return classifyStoreError(err)
	}
	if plan.Refused != "" {
		fmt.Fprintln(stdout, plan.Refused)
		return ExitOK
	}
	if *dryRunFlag {
		fmt.Fprintf(stdout, "would drop %d artifact(s):\n", len(plan.Items))
		for _, a := range plan.Items {
			fmt.Fprintf(stdout, "  %-8s %s\n", a.Kind, a.Name)
		}
		return ExitOK
	}

	c, err := conn.resolve()
	if err != nil {
		fmt.Fprintf(stderr, "saksama cleanup: %v\n", err)
		return ExitUsage
	}
	db, err := openDB(c)
	if err != nil {
		fmt.Fprintf(stderr, "saksama cleanup: %v\n", err)
		return ExitInternal
	}
	defer db.Close()

	res, err := core.New(db, store).Cleanup(context.Background(), positional[0], false)
	if err != nil {
		fmt.Fprintf(stderr, "saksama cleanup: %v\n", err)
		return ExitInternal
	}

	dropped, failed := 0, 0
	for _, a := range res.Items {
		if a.Err != "" {
			fmt.Fprintf(stdout, "  FAILED  %-8s %s: %s\n", a.Kind, a.Name, a.Err)
			failed++
			continue
		}
		fmt.Fprintf(stdout, "  dropped %-8s %s\n", a.Kind, a.Name)
		dropped++
	}
	fmt.Fprintf(stdout, "dropped %d of %d\n", dropped, len(res.Items))
	if failed > 0 {
		fmt.Fprintf(stderr, "%d artifact(s) could not be dropped\n", failed)
		return ExitInternal
	}
	return ExitOK
}
