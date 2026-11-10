package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/perogeremmer/saksama-core/internal/introspect"
	"github.com/perogeremmer/saksama-core/internal/probe"
	"github.com/perogeremmer/saksama-core/internal/render"
	"github.com/perogeremmer/saksama-core/internal/support"
)

// cmdCompat answers "does this combination work?" with evidence rather than
// confidence.
func cmdCompat(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("compat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	markdown := fs.Bool("markdown", false, "emit the compatibility document")
	jsonOut := fs.Bool("json", false, "JSON output")
	noProbe := fs.Bool("no-probe", false, "skip detecting installed engines")

	var conn connFlags
	addConnFlags(fs, &conn)
	fs.Bool("no-color", false, "never use colour (already off for a pipe, NO_COLOR, TERM=dumb)")

	if code, done := parseFlags(fs, args); done {
		return code
	}
	if code := refuseStrayArguments(fs, "compat", stderr); code != ExitOK {
		return code
	}

	if *markdown {
		if err := render.Compatibility(stdout); err != nil {
			fmt.Fprintf(stderr, "saksama compat: %v\n", err)
			return ExitInternal
		}
		return ExitOK
	}

	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"version":  buildVersion(Version),
			"mysql":    support.MySQL,
			"engines":  support.Engines,
			"findings": support.Findings,
			"postgres": support.Postgres,
		})
		return ExitOK
	}

	// Local picture: what is installed, and what the server says if asked.
	var binaries map[string]probe.Result
	if !*noProbe {
		binaries = probe.Results()
	}

	serverVersion := ""
	if conn.host != "" {
		c, err := conn.resolve()
		if err != nil {
			fmt.Fprintf(stderr, "saksama compat: %v\n", err)
			return ExitUsage
		}
		db, err := openDB(c)
		if err != nil {
			fmt.Fprintf(stderr, "saksama compat: %v\n", err)
			return ExitInternal
		}
		defer db.Close()
		srv, err := introspect.NewCollector(db, "mysql", "").Server(context.Background())
		if err == nil {
			serverVersion = srv.Version
		}
	}

	if err := render.LocalCompatibility(stdout, serverVersion, binaries); err != nil {
		fmt.Fprintf(stderr, "saksama compat: %v\n", err)
		return ExitInternal
	}
	return ExitOK
}
