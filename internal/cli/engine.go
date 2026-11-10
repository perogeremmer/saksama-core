package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/perogeremmer/saksama-core/internal/core"
	"github.com/perogeremmer/saksama-core/internal/probe"
	"github.com/perogeremmer/saksama-core/internal/render"
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
	case "doctor", "check":
		return cmdEngineDoctor(args[1:], stdout, stderr)
	case "install":
		return cmdEngineInstall(args[1:], stdout, stderr)
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
  saksama engine doctor [<name>]      can this engine run here? binary, deps, server
  saksama engine install <name>       fetch the verified build for this machine

Names: native | gh-ost | pt-osc   (aliases: ghost, pt, percona, ddl)

Scopes, from most to least specific:
  saksama engine set gh-ost -d shop -t orders   one table
  saksama engine set pt-osc -d shop             every table in shop
  saksama engine set pt-osc --global            every table, everywhere

Resolution order for plan/apply:
  1. --engine on the command line
  2. recorded choice for database.table
  3. recorded choice for the database
  4. recorded global choice
  5. analyzer recommendation

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
	plain := fs.Bool("plain", false, "borderless output, for scripts and logs")
	if code, done := parseFlags(fs, args); done {
		return code
	}
	if code := refuseStrayArguments(fs, "engine list", stderr); code != ExitOK {
		return code
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

	engines := render.Table{
		Border: !*plain,
		Columns: []render.Column{
			{Header: "ENGINE"}, {Header: "WRAPPED"}, {Header: "INSTALLED"},
			{Header: "VERSION"}, {Header: "NOTE"},
		},
	}
	engines.Add("native", "yes", "yes", "-", "executed by MySQL itself")
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
		engines.Add(e.name, "yes", installed, version, e.note)
	}
	_ = engines.Render(stdout)

	fmt.Fprintln(stdout)
	if len(choices) == 0 {
		fmt.Fprintln(stdout, "no engine recorded: plan/apply will follow the analyzer recommendation")
		fmt.Fprintf(stdout, "record one with:  saksama engine set gh-ost -d <db> -t <table>\n")
		return ExitOK
	}
	recorded := render.Table{
		Border: !*plain,
		Columns: []render.Column{
			{Header: "SCOPE"}, {Header: "APPLIES TO"}, {Header: "ENGINE"},
			{Header: "SET AT"}, {Header: "NOTE"},
		},
	}
	for _, c := range choices {
		recorded.Add(string(c.Scope), c.Label(), c.Engine, c.SetAt, c.Note)
	}
	_ = recorded.Render(stdout)
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
	global := fs.Bool("global", false, "apply to every table on every database")

	// The engine name is positional and people type it first, before the flags.
	positional, flagArgs := extractPositionals(args, map[string]bool{
		"d": true, "database": true, "t": true, "table": true, "note": true,
	})
	if code, done := parseFlags(fs, flagArgs); done {
		return code
	}
	if len(positional) == 0 {
		fmt.Fprintln(stderr, "saksama engine set: engine name required (native, gh-ost, pt-osc)")
		return ExitUsage
	}

	choice, err := state.NewChoice(positional[0], *global, *database, *table)
	if err != nil {
		fmt.Fprintf(stderr, "saksama engine set: %v\n", err)
		return ExitUsage
	}
	choice.Actor = currentActor()
	choice.Note = *note

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama engine set: %v\n", err)
		return ExitInternal
	}
	if err := store.SetEngineChoice(choice); err != nil {
		fmt.Fprintf(stderr, "saksama engine set: %v\n", err)
		return ExitInternal
	}

	fmt.Fprintf(stdout, "engine set to %s for %s\n", choice.Engine, describeScope(choice))
	fmt.Fprintf(stdout, "state: %s\n", store.Dir())
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "check it:  saksama engine show%s\n", core.EngineScopeFlags(choice))
	fmt.Fprintf(stdout, "undo it:   %s\n", core.EngineClearCommandLine(choice))
	return ExitOK
}

func describeScope(c state.EngineChoice) string {
	switch c.Scope {
	case state.ScopeDatabase:
		return "every table in " + c.Database
	case state.ScopeTable:
		return c.Database + "." + c.Table
	}
	return "every table on every database (global)"
}

func cmdEngineShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("engine show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	database := fs.String("d", "", "database")
	fs.StringVar(database, "database", "", "database")
	table := fs.String("t", "", "table")
	fs.StringVar(table, "table", "", "table")
	jsonOut := fs.Bool("json", false, "JSON output")
	global := fs.Bool("global", false, "ask about the global choice")
	_, flagArgs := extractPositionals(args, map[string]bool{"d": true, "database": true, "t": true, "table": true})
	if code, done := parseFlags(fs, flagArgs); done {
		return code
	}
	if *global && (*database != "" || *table != "") {
		fmt.Fprintln(stderr, "saksama engine show: --global cannot be combined with -d or -t")
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

	asked := "global"
	switch {
	case *database != "" && *table != "":
		asked = *database + "." + *table
	case *database != "":
		asked = *database + ".*"
	default:
		// Global: report the global record itself, not a resolution.
		choice, err = store.ResolveEngine("", "")
		if err != nil {
			fmt.Fprintf(stderr, "saksama engine: %v\n", err)
			return ExitInternal
		}
	}
	if choice == nil {
		fmt.Fprintf(stdout, "asking about  %s\n", asked)
		fmt.Fprintf(stdout, "engine        (not set)\n")
		fmt.Fprintf(stdout, "behaviour     plan/apply follow the analyzer recommendation\n")
		return ExitOK
	}
	fmt.Fprintf(stdout, "asking about  %s\n", asked)
	fmt.Fprintf(stdout, "engine        %s\n", choice.Engine)
	fmt.Fprintf(stdout, "from           %s choice\n", choice.Scope)
	fmt.Fprintf(stdout, "set at        %s\n", choice.SetAt)
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
	global := fs.Bool("global", false, "clear the global choice")
	_, flagArgs := extractPositionals(args, map[string]bool{"d": true, "database": true, "t": true, "table": true})
	if code, done := parseFlags(fs, flagArgs); done {
		return code
	}

	choice, err := state.NewChoice("native", *global, *database, *table)
	if err != nil {
		fmt.Fprintf(stderr, "saksama engine clear: %v\n", err)
		return ExitUsage
	}

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama engine clear: %v\n", err)
		return ExitInternal
	}
	removed, err := store.ClearEngineChoice(choice)
	if err != nil {
		fmt.Fprintf(stderr, "saksama engine clear: %v\n", err)
		return ExitInternal
	}
	if !removed {
		fmt.Fprintf(stdout, "no recorded choice for %s\n", describeScope(choice))
	} else {
		fmt.Fprintf(stdout, "cleared %s\n", describeScope(choice))
	}

	// Clearing a narrow scope says nothing about a wider one, and the wider one is
	// what still decides the engine. Say so, with the command that ends it.
	if c, err := store.ResolveEngine(*database, *table); err == nil && c != nil {
		fmt.Fprintf(stdout, "%s still applies to %s; clear it with: saksama engine clear%s\n",
			c.Engine, describeScope(choice), core.EngineScopeFlags(*c))
	}
	return ExitOK
}
