package cli

import (
	"context"
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
	var conn connFlags

	fs := flag.NewFlagSet("cutover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addConnFlags(fs, &conn)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "saksama cutover: session id required")
		return ExitUsage
	}

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama cutover: %v\n", err)
		return ExitInternal
	}
	runner := &core.Runner{Store: store}

	id := fs.Arg(0)
	sess, err := runner.ReleaseCutover(context.Background(), id)
	if err != nil {
		fmt.Fprintf(stderr, "saksama cutover: %v\n", err)
		// A refused release is usually the state doing its job, not a typo; only
		// a missing session is the operator's to fix.
		return classifyStoreError(err)
	}

	fmt.Fprintf(stdout, "cutover released for session %s\n", id)
	fmt.Fprintf(stdout, "engine %s was waiting on %s\n", sess.Engine, "the gate file")
	fmt.Fprintln(stdout, "the swap happens now; the process that started the engine reports the outcome.")
	fmt.Fprintf(stdout, "follow it with: saksama attach %s\n", id)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "there is no data rollback after a cutover: after this point, new writes")
	fmt.Fprintln(stdout, "live only in the new table.")
	return ExitOK
}
