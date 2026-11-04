package cli

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/cred"
	"github.com/perogeremmer/saksama-core/internal/engine"
	"github.com/perogeremmer/saksama-core/internal/introspect"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// cmdCleanup drops the artifacts a session recorded as its own.
//
// It refuses to work from a name pattern. A table called _orders_old might belong
// to a migration, or might be something a person created deliberately, and the name
// cannot tell you which. Ownership is recorded when the engine runs, so cleanup
// drops only what it can prove it made.
func cmdCleanup(args []string, stdout, stderr io.Writer) int {
	var conn connFlags

	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addConnFlags(fs, &conn)
	dryRunFlag := hasFlag(args, "--dry-run")
	args = removeFlag(args, "--dry-run")
	fs.Bool("dry-run", false, "show what would be dropped, and stop")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	dryRun := &dryRunFlag
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "saksama cleanup: session id required")
		return ExitUsage
	}

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama cleanup: %v\n", err)
		return ExitInternal
	}
	id := fs.Arg(0)
	sess, err := store.Load(id)
	if err != nil {
		fmt.Fprintf(stderr, "saksama cleanup: %v\n", err)
		return ExitUsage
	}

	deletable := sess.DeletableArtifacts()
	if len(sess.Artifacts) == 0 {
		fmt.Fprintf(stdout, "session %s recorded no artifacts (engine %q creates none of its own)\n", id, sess.Engine)
		fmt.Fprintln(stdout, "nothing to clean up")
		return ExitOK
	}
	if len(deletable) == 0 {
		fmt.Fprintf(stdout, "session %s is still %s, so its artifacts may be needed for recovery\n", id, sess.State)
		fmt.Fprintln(stdout, "nothing was dropped")
		return ExitOK
	}

	if *dryRun {
		fmt.Fprintf(stdout, "would drop %d artifact(s):\n", len(deletable))
		for _, a := range deletable {
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

	dropped, failed := dropArtifacts(db, sess.Database, deletable, stdout)

	for _, name := range dropped {
		for _, a := range deletable {
			if a.Name == name {
				sess.ForgetsArtifact(a.Kind, a.Name)
			}
		}
	}
	if err := store.Save(sess); err != nil {
		fmt.Fprintf(stderr, "saksama cleanup: %v\n", err)
		return ExitInternal
	}

	fmt.Fprintf(stdout, "dropped %d of %d\n", len(dropped), len(deletable))
	if failed > 0 {
		fmt.Fprintf(stderr, "%d artifact(s) could not be dropped\n", failed)
		return ExitInternal
	}
	return ExitOK
}

// dropArtifacts removes the recorded objects, one statement each, and reports what
// actually worked.
func dropArtifacts(db *sql.DB, database string, artifacts []state.ArtifactRecord, out io.Writer) (dropped []string, failed int) {
	ctx := context.Background()
	for _, a := range artifacts {
		var stmt string
		switch a.Kind {
		case "table":
			stmt = "DROP TABLE IF EXISTS " + engine.QuoteIdent(database) + "." + engine.QuoteIdent(a.Name)
		case "trigger":
			stmt = "DROP TRIGGER IF EXISTS " + engine.QuoteIdent(database) + "." + engine.QuoteIdent(a.Name)
		default:
			fmt.Fprintf(out, "  skip    %s %s: unknown artifact kind\n", a.Kind, a.Name)
			failed++
			continue
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			fmt.Fprintf(out, "  FAILED  %-8s %s: %v\n", a.Kind, a.Name, err)
			failed++
			continue
		}
		fmt.Fprintf(out, "  dropped %-8s %s\n", a.Kind, a.Name)
		dropped = append(dropped, a.Name)
	}
	return dropped, failed
}

// pendingArtifacts lists what a session still owns and has not dropped, which is
// what `status` shows after a failure.
func pendingArtifacts(sess *state.Session) []state.ArtifactRecord {
	return sess.Artifacts
}

var (
	_ = cred.Conn{}
	_ = introspect.Artifact{}
	_ = strings.TrimSpace
)
