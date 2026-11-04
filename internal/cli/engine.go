package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/perogeremmer/saksama-core/internal/probe"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// cmdEngine implements the decision boundary: the analyzer recommends, the
// operator chooses, and `apply` executes the choice.
func cmdEngine(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		engineUsage(stderr)
		return ExitUsage
	}

	switch args[0] {
	case "list", "ls":
		return cmdEngineList(args[1:], stdout, stderr)
	case "set":
		return cmdEngineSet(args[1:], stdout, stderr)
	case "show":
		return cmdEngineShow(args[1:], stdout, stderr)
	case "clear", "unset":
		return cmdEngineClear(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		engineUsage(stdout)
		return ExitOK
	}

	fmt.Fprintf(stderr, "saksama engine: unknown subcommand %q\n\n", args[0])
	engineUsage(stderr)
	return ExitUsage
}

func engineUsage(w io.Writer) {
	fmt.Fprint(w, `saksama engine - inspect and record which migration engine to use.

The analyzer recommends an engine; nothing is decided until you set it.

Usage:
  saksama engine list                 engines, versions found locally, recorded choices
  saksama engine set <name>           record a choice (default engine)
  saksama engine set <name> -d <db> -t <table>
                                      record a choice for one table
  saksama engine show [-d db -t table]
                                      what would be used right now
  saksama engine clear [-d db -t table]
                                      forget a recorded choice

Names: native | gh-ost | pt-osc   (aliases: ghost, pt, percona, ddl)

Resolution order for plan/apply:
  1. --engine on the command line
  2. recorded choice for database.table
  3. recorded default choice
  4. analyzer recommendation

Examples:
  saksama advise --live -h db -d shop -t orders -a "MODIFY COLUMN ..."
  saksama engine set gh-ost -d shop -t orders
  saksama apply  -h db -d shop -t orders -a "MODIFY COLUMN ..."
`)
}

func cmdEngineList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("engine list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "JSON output")
	noProbe := fs.Bool("no-probe", false, "skip probing installed binaries")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama engine: %v\n", err)
		return ExitInternal
	}
	choices, err := store.ListEngineChoices()
	if err != nil {
		fmt.Fprintf(stderr, "saksama engine: %v\n", err)
		return ExitInternal
	}

	var bins map[string]struct {
		Path, Version string
		Available     bool
	}
	if !*noProbe {
		p := probe.Binaries()
		bins = make(map[string]struct {
			Path, Version string
			Available     bool
		}, len(p))
		for name, b := range p {
			bins[name] = struct {
				Path, Version string
				Available     bool
			}{b.Path, b.Version, b.Available}
		}
	}

	if *jsonOut {
		type row struct {
			Engine    string `json:"engine"`
			Wrapped   bool   `json:"wrapped"`
			Installed bool   `json:"installed"`
			Path      string `json:"path,omitempty"`
			Version   string `json:"version,omitempty"`
			Note      string `json:"note"`
		}
		rows := []row{
			{Engine: "native", Wrapped: true, Installed: true, Note: "executed by MySQL itself; nothing to install"},
			{Engine: "gh-ost", Wrapped: true, Note: "MIT, single binary"},
			{Engine: "pt-osc", Wrapped: true, Note: "GPL-2, separate install, never bundled"},
		}
		for i := range rows {
			binName := "gh-ost"
			if rows[i].Engine == "pt-osc" {
				binName = "pt-online-schema-change"
			}
			if rows[i].Engine == "native" {
				continue
			}
			if b, ok := bins[binName]; ok {
				rows[i].Installed = b.Available
				rows[i].Path = b.Path
				rows[i].Version = b.Version
			}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"engines": rows, "choices": choices})
		return ExitOK
	}

	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ENGINE\tWRAPPED\tINSTALLED\tVERSION\tNOTE")
	fmt.Fprintf(tw, "native\tyes\tyes\t%s\texecuted by MySQL itself\n", "-")
	for _, e := range []struct{ name, bin, note string }{
		{"gh-ost", "gh-ost", "copy engine, MIT"},
		{"pt-osc", "pt-online-schema-change", "copy engine, GPL-2, separate install"},
	} {
		installed, version := "unknown", "-"
		if bins != nil {
			if b, ok := bins[e.bin]; ok {
				version = b.Version
				if b.Available {
					installed = "yes"
				} else {
					installed = "no"
				}
			}
		}
		if bins == nil {
			installed = "not probed"
		}
		fmt.Fprintf(tw, "%s\tyes\t%s\t%s\t%s\n", e.name, installed, version, e.note)
	}
	tw.Flush()

	fmt.Fprintln(stdout)
	if len(choices) == 0 {
		fmt.Fprintln(stdout, "no engine recorded: plan/apply will follow the analyzer recommendation")
		fmt.Fprintf(stdout, "record one with:  saksama engine set gh-ost -d <db> -t <table>\n")
		return ExitOK
	}
	tw = tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SCOPE\tENGINE\tSET AT\tNOTE")
	for _, c := range choices {
		scope := c.Target
		if scope == "" {
			scope = "(default)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", scope, c.Engine, c.SetAt, c.Note)
	}
	tw.Flush()
	return ExitOK
}

func cmdEngineSet(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("engine set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	database := fs.String("d", "", "database")
	fs.StringVar(database, "database", "", "database")
	table := fs.String("t", "", "table")
	fs.StringVar(table, "table", "", "table")
	note := fs.String("note", "", "why you chose it (recorded in the audit trail)")

	// The engine name is positional and people type it first, before the flags.
	positional, flagArgs := extractPositionals(args, map[string]bool{
		"d": true, "database": true, "t": true, "table": true, "note": true,
	})
	if err := fs.Parse(flagArgs); err != nil {
		return ExitUsage
	}
	if len(positional) == 0 {
		fmt.Fprintln(stderr, "saksama engine set: engine name required (native, gh-ost, pt-osc)")
		return ExitUsage
	}

	engine, err := state.NormalizeEngine(positional[0])
	if err != nil {
		fmt.Fprintf(stderr, "saksama engine set: %v\n", err)
		return ExitUsage
	}
	if (*database == "") != (*table == "") {
		fmt.Fprintln(stderr, "saksama engine set: give both -d and -t, or neither for the default")
		return ExitUsage
	}

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama engine set: %v\n", err)
		return ExitInternal
	}

	choice := state.EngineChoice{
		Engine: engine,
		Target: state.NormalizeTarget(*database, *table),
		Actor:  currentActor(),
		Note:   *note,
	}
	if err := store.SetEngineChoice(choice); err != nil {
		fmt.Fprintf(stderr, "saksama engine set: %v\n", err)
		return ExitInternal
	}

	scope := choice.Target
	if scope == "" {
		scope = "all tables (default)"
	}
	fmt.Fprintf(stdout, "engine set to %s for %s\n", engine, scope)
	fmt.Fprintf(stdout, "state: %s\n", store.Dir())
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "check it:  saksama engine show"+
		iffStr(choice.Target != "", " -d "+*database+" -t "+*table, ""))
	fmt.Fprintln(stdout, "undo it:   saksama engine clear"+
		iffStr(choice.Target != "", " -d "+*database+" -t "+*table, ""))
	return ExitOK
}

func cmdEngineShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("engine show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	database := fs.String("d", "", "database")
	fs.StringVar(database, "database", "", "database")
	table := fs.String("t", "", "table")
	fs.StringVar(table, "table", "", "table")
	jsonOut := fs.Bool("json", false, "JSON output")
	_, flagArgs := extractPositionals(args, map[string]bool{"d": true, "database": true, "t": true, "table": true})
	if err := fs.Parse(flagArgs); err != nil {
		return ExitUsage
	}

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama engine: %v\n", err)
		return ExitInternal
	}

	choice, err := store.ResolveEngine(*database, *table)
	if err != nil {
		fmt.Fprintf(stderr, "saksama engine: %v\n", err)
		return ExitInternal
	}

	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"choice": choice})
		return ExitOK
	}

	scope := state.NormalizeTarget(*database, *table)
	if scope == "" {
		scope = "(default)"
	}
	if choice == nil {
		fmt.Fprintf(stdout, "scope      %s\n", scope)
		fmt.Fprintf(stdout, "engine     (not set)\n")
		fmt.Fprintf(stdout, "behaviour  plan/apply follow the analyzer recommendation\n")
		return ExitOK
	}
	fmt.Fprintf(stdout, "scope      %s\n", scope)
	fmt.Fprintf(stdout, "engine     %s\n", choice.Engine)
	if choice.Target != "" && choice.Target != scope {
		fmt.Fprintf(stdout, "source     default choice (no table-specific override)\n")
	}
	fmt.Fprintf(stdout, "set at     %s\n", choice.SetAt)
	if choice.Actor != "" {
		fmt.Fprintf(stdout, "by         %s\n", choice.Actor)
	}
	if choice.Note != "" {
		fmt.Fprintf(stdout, "note       %s\n", choice.Note)
	}
	return ExitOK
}

func cmdEngineClear(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("engine clear", flag.ContinueOnError)
	fs.SetOutput(stderr)
	database := fs.String("d", "", "database")
	fs.StringVar(database, "database", "", "database")
	table := fs.String("t", "", "table")
	fs.StringVar(table, "table", "", "table")
	_, flagArgs := extractPositionals(args, map[string]bool{"d": true, "database": true, "t": true, "table": true})
	if err := fs.Parse(flagArgs); err != nil {
		return ExitUsage
	}
	if (*database == "") != (*table == "") {
		fmt.Fprintln(stderr, "saksama engine clear: give both -d and -t, or neither for the default")
		return ExitUsage
	}

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama engine clear: %v\n", err)
		return ExitInternal
	}
	target := state.NormalizeTarget(*database, *table)
	if err := store.ClearEngineChoice(target); err != nil {
		fmt.Fprintf(stderr, "saksama engine clear: %v\n", err)
		return ExitInternal
	}
	scope := target
	if scope == "" {
		scope = "the default choice"
	}
	fmt.Fprintf(stdout, "cleared %s\n", scope)
	return ExitOK
}

func iffStr(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

var _ = strings.TrimSpace
