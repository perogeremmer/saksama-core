package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/perogeremmer/saksama-core/internal/core"
)

// cmdCutover releases a held cutover.
//
// The engine is already waiting: it finished the copy and will not swap until the
// gate file disappears. This command removes it, which is the verification gate the
// product exists for. It talks to the state directory, not to a daemon, so it works
// from another terminal, another ssh session, or after a reconnect.
func cmdCutover(args []string, stdout, stderr io.Writer) int {
	// No connection flags: releasing a gate removes a file in the state directory
	// and records the decision. The engine that is waiting reports the outcome.
	// They used to be declared and never read, which told an operator that -h
	// selected the server it would talk to.
	positional, flagArgs := extractPositionals(args, nil)

	fs := flag.NewFlagSet("cutover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(flagArgs); err != nil {
		return ExitUsage
	}
	if len(positional) == 0 {
		fmt.Fprintln(stderr, "saksama cutover: session id required")
		return ExitUsage
	}

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama cutover: %v\n", err)
		return ExitInternal
	}
	runner := &core.Runner{Store: store}

	sess, err := runner.ReleaseCutover(context.Background(), positional[0])
	if err != nil {
		fmt.Fprintf(stderr, "saksama cutover: %v\n", err)
		// A session that never held a cutover is a precondition the operator can
		// read and fix - not an internal failure, which is what this used to
		// report - and a missing session is a typo. Everything else is ours.
		if errors.Is(err, core.ErrNoHeldCutover) {
			return ExitUsage
		}
		return classifyStoreError(err)
	}
	id := positional[0]

	fmt.Fprintf(stdout, "cutover released for session %s\n", id)
	fmt.Fprintf(stdout, "engine %s was waiting on %s\n", sess.Engine, "the gate file")
	fmt.Fprintln(stdout, "the swap happens now; the process that started the engine reports the outcome.")
	fmt.Fprintf(stdout, "follow it with: saksama attach %s\n", id)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "there is no data rollback after a cutover: after this point, new writes")
	fmt.Fprintln(stdout, "live only in the new table.")
	return ExitOK
}
