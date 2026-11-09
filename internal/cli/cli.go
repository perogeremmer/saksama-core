// Package cli wires the commands. Every command is a thin frontend over the
// analyzer (and, in later phases, the core daemon).
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/model"
	"github.com/perogeremmer/saksama-core/internal/parser"
	"github.com/perogeremmer/saksama-core/internal/render"
	"github.com/perogeremmer/saksama-core/internal/state"
	"github.com/perogeremmer/saksama-core/internal/wizard"
)

// Version is the build version, injected with -ldflags. When nothing is
// injected, buildVersion falls back to the module version the toolchain
// recorded, so an install without -ldflags still reports the release it is.
var Version = "dev"

// Exit codes are part of the contract with CI.
const (
	ExitOK       = 0
	ExitBlocked  = 1
	ExitUsage    = 2
	ExitInternal = 3
)

// stateDir is set once from --state-dir, before any command sees its flags.
//
// It is global on purpose: the CLI and the wizard must read and write the same
// directory, and a per-command flag meant each new command had to remember to
// accept it (one of them silently did not).
var stateDir string

// Run dispatches a command line and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	args, stateDir = extractStateDir(args)
	render.UseColor(colorFor(args, stdout))

	if len(args) == 0 {
		// A bare `saksama` on a terminal is someone asking for help, not someone
		// asking for an error page. Pipes and CI keep the old behaviour, because a
		// prompt that waits for input in a pipeline is a hang.
		if isInteractive(stdout) {
			return wizard.Run(nil, stateDir, stdout, stderr)
		}
		usage(stderr)
		return ExitUsage
	}

	switch args[0] {
	case "plan":
		if hasFlag(args[1:], "--live") {
			return cmdPlanLive(removeFlag(args[1:], "--live"), stdout, stderr)
		}
		return cmdPlan(args[1:], stdout, stderr)
	case "advise":
		return cmdAdvise(args[1:], stdout, stderr)
	case "engine":
		return cmdEngine(args[1:], stdout, stderr)
	case "wizard", "interactive":
		return wizard.Run(args[1:], stateDir, stdout, stderr)
	case "compat":
		return cmdCompat(args[1:], stdout, stderr)
	case "apply":
		return cmdApply(args[1:], stdout, stderr)
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
	case "attach":
		return cmdAttach(args[1:], stdout, stderr)
	case "cancel":
		return cmdCancel(args[1:], stdout, stderr)
	case "cutover":
		return cmdCutover(args[1:], stdout, stderr)
	case "reconcile":
		return cmdReconcile(args[1:], stdout, stderr)
	case "events":
		return cmdEvents(args[1:], stdout, stderr)
	case "export":
		return cmdExport(args[1:], stdout, stderr)
	case "cleanup":
		return cmdCleanup(args[1:], stdout, stderr)
	case "schema":
		return cmdSchema(args[1:], stdout, stderr)
	case "version", "--version", "-V":
		fmt.Fprintf(stdout, "saksama %s (Saksama Core, contract v%d)\n", buildVersion(Version), analyzer.ContractVersion)
		return ExitOK
	case "help", "--help", "-help":
		usage(stdout)
		return ExitOK
	case "daemon", "recover":
		fmt.Fprintf(stderr, "saksama %s: not implemented in this build\n", args[0])
		fmt.Fprintf(stderr, "this build provides: plan, plan --live, apply, status, events, export, cleanup, schema\n")
		return ExitUsage
	default:
		fmt.Fprintf(stderr, "saksama: unknown command %q\n\n", args[0])
		usage(stderr)
		return ExitUsage
	}
}

// extractStateDir pulls the global --state-dir out of the arguments, in either
// spelling, so every command supports it without declaring it.
func extractStateDir(args []string) ([]string, string) {
	out := make([]string, 0, len(args))
	dir := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--state-dir" && i+1 < len(args):
			dir = args[i+1]
			i++
		case strings.HasPrefix(arg, "--state-dir="):
			dir = strings.TrimPrefix(arg, "--state-dir=")
		default:
			out = append(out, arg)
		}
	}
	return out, dir
}

// isInteractive reports whether stdout is a terminal, which is the only place a
// prompt is allowed to appear.
func isInteractive(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Saksama - examine before you alter.

Usage:
  saksama                   interactive menu (on a terminal; otherwise this page)
  saksama wizard            the same menu, explicitly
  saksama --help            this page
  saksama plan   --ddl <file|-> --alter "<ALTER>" --mysql-version <X.Y.Z> [options]
  saksama plan --live -h <host> -d <db> -t <table> -a "<ALTER>" [options]
  saksama advise --ddl <file|-> --alter "<ALTER>" --mysql-version <X.Y.Z> [options]
  saksama advise --live -h <host> -d <db>                   inspect every table
  saksama advise --live -h <host> -d <db> -t <table> [-a "<ALTER>"]
  saksama engine list|set|show|clear   choose the engine yourself
  saksama engine doctor [<engine>]     can it run here, and can that server host it
  saksama engine install <engine>      verified download; the checksum is checked
  saksama compat                       which MySQL and engine versions are verified
  saksama apply  -h <host> -d <db> -t <table> -a "<ALTER>" [options]
  saksama status [<session>] [--json]
  saksama attach <session>                 follow the event log, detach with ctrl-c
  saksama cancel <session>                 stop a running copy engine by pid
  saksama cutover <session>                release a held cutover, allowing the swap
  saksama reconcile                        sessions that never finished, and what is left
  saksama reconcile --retire <session>     mark a dead attempt failed, release its lock
  saksama events <session>
  saksama export <session> [--format json|markdown]
  saksama cleanup <session> [--dry-run]
  saksama schema
  saksama version

The wizard asks numbered questions, prints the equivalent non-interactive
command before it does anything, and never invents a decision: every step calls
the same core function the flag-based command calls.

Offline planning takes the table definition from SHOW CREATE TABLE output, so it
can run in CI without touching the database. --live plans against the server and
records a session; apply executes a native migration.

Connection options (plan --live, apply):
  -h, --host <name>        MySQL host
  -P, --port <n>           MySQL port
  -u, --user <name>        MySQL user
  -d, --database <name>    database
  -t, --table <name>       table
  -a, --alter <sql>        the ALTER TABLE body
  --defaults-extra-file <path>   read credentials from an option file
  --ask-pass               prompt for the password (never pass it in argv)
  --no-defaults            ignore ~/.my.cnf
  --tls <mode>             preferred|true|false|skip-verify

Engine options:
  saksama engine set <name> --global             every table, everywhere
  saksama engine set <name> -d <db>              every table in one database
  saksama engine set <name> -d <db> -t <table>   one table
  saksama engine list                            what is installed, what is chosen
  saksama engine show -d <db> -t <table>         what apply would use now
  --note <text>            why this choice was made; recorded with it (engine set)
  --plain                  one row per line, no box drawing (engine list)

Compat options:
  --json                   the verified matrix as JSON
  --markdown               the verified matrix as a markdown table
  --no-probe               do not look for gh-ost / pt-osc on PATH

Advise options:
  --live                   read from the server instead of a file
  -t, --table <name>       one table; omit it to inspect the whole schema
  -a, --alter <sql>        optional; without it the report covers table shape
  --no-probe               skip detecting gh-ost / pt-online-schema-change on PATH
  --json                   machine-readable engine matrix

Apply options:
  --production             require --confirm <host> before any mutation
  --accept-unknown-mdl     allow 5.6 in production: the lock queue cannot be seen
  --allow-native-rebuild   in production, let MySQL rebuild a small table in place
                           instead of installing a copy engine (no throttle, no pause)
  --confirm <host>         must equal the target host name when --production
  --dry-run                build statements and stop
  --engine <name>          native|gh-ost|pt-osc; overrides the recorded choice
  --force                  downgrade selected preflight aborts
  --lock-wait-timeout <n>  session lock_wait_timeout in seconds (default 10)
  --detach                 run the engine in its own process group, so closing
                           this terminal does not stop it; stop it with: saksama cancel
  --hold-cutover           pause before the swap so you can verify (gh-ost)
  --allow-argv-password    let an engine without an option file take the password
                           in argv (visible in ps); off by default
  --json                   machine-readable output

Plan options:
  --ddl <path|->         SHOW CREATE TABLE output, or - for stdin
  --schema <path>        table JSON produced by: saksama plan --dump-schema
  --dump-schema          write the parsed table JSON instead of planning
  --alter <sql>          the ALTER TABLE body, for example "ADD INDEX ix (a)"
  --mysql-version <v>    server version, for example 8.0.36 (required)
  --host <name>          target host label, used for reporting and confirmation
  --engine <name>        force native|gh-ost|pt-osc (still subject to blockers)
  --production           mark the target as production
  --accept-unknown-mdl   proceed on 5.6 where the lock queue cannot be observed
  --chunk-size <n>       rows per copy chunk (default 1000)
  --free-disk <bytes>    free space on the data volume, for the disk check
  --disk-total <bytes>   volume size, so the 10 percent floor can be applied
  --rows <n>             estimated row count (improves the duration range)
  --data-bytes <n>       table data size in bytes
  --index-bytes <n>      table index size in bytes
  --json                 emit the machine-readable plan instead of the card

Session options (status, attach, events, export, cleanup, reconcile):
  --json                   machine-readable output
  --plain                  one row per line, no box drawing (status)
  --since <n>              attach: only events after sequence n
  --follow=false           attach: print what is already logged and exit
  --format json|markdown   export: the audit trail's format
  --dry-run                cleanup: list what would be dropped, change nothing
  --retire <session>       reconcile: mark a dead attempt failed, release its lock
  --force-kill             cancel: SIGKILL an engine that ignores SIGTERM

Global options:
  --state-dir <path>       where sessions and event logs live; default
                           ~/.local/state/saksama, or SAKSAMA_STATE_DIR
  --no-color               never use colour; it is already off for a pipe,
                           when NO_COLOR is set, or when TERM=dumb

Exit codes:
  0  plan produced, no blockers
  1  plan produced with blockers (safe to use as a CI gate)
  2  usage error
  3  internal error
`)
}

type planOpts struct {
	ddlPath            string
	schemaPath         string
	dumpSchema         bool
	alter              string
	mysqlVer           string
	host               string
	engine             string
	production         bool
	acceptUnknownMDL   bool
	allowNativeRebuild bool
	freeDisk           int64
	diskTotal          int64
	rows               int64
	dataBytes          int64
	indexBytes         int64
	jsonOut            bool
}

// colorFor decides whether this invocation writes colour. Presentation only: the JSON
// output and every exit code are unaffected, and a pipe (a CI log, a test) gets exactly
// the plain text it got before.
func colorFor(args []string, stdout io.Writer) bool {
	ok, _ := render.ShouldColor(stdout, hasFlag(args, "--no-color"))
	return ok
}

func cmdPlan(args []string, stdout, stderr io.Writer) int {
	var o planOpts

	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.ddlPath, "ddl", "", "SHOW CREATE TABLE output file, or - for stdin")
	fs.StringVar(&o.schemaPath, "schema", "", "table JSON file")
	fs.BoolVar(&o.dumpSchema, "dump-schema", false, "emit parsed table JSON")
	fs.StringVar(&o.alter, "alter", "", "ALTER TABLE body")
	fs.StringVar(&o.mysqlVer, "mysql-version", "", "server version")
	fs.StringVar(&o.host, "h", "", "target host label")
	fs.StringVar(&o.host, "host", "", "target host label")
	fs.StringVar(&o.engine, "engine", "", "force engine")
	fs.BoolVar(&o.production, "production", false, "mark target as production")
	fs.BoolVar(&o.allowNativeRebuild, "allow-native-rebuild", false,
		"in production, let MySQL rebuild a small table in place (no throttle, no pause)")
	fs.BoolVar(&o.acceptUnknownMDL, "accept-unknown-mdl", false,
		"allow a production plan where the metadata lock queue cannot be observed (MySQL 5.6)")
	fs.Int64Var(&o.freeDisk, "free-disk", 0, "free disk bytes")
	fs.Int64Var(&o.diskTotal, "disk-total", 0, "total bytes of the data volume, to apply the 10 percent floor")
	fs.Int64Var(&o.rows, "rows", 0, "estimated row count (informs duration)")
	fs.Int64Var(&o.dataBytes, "data-bytes", 0, "table data size in bytes")
	fs.Int64Var(&o.indexBytes, "index-bytes", 0, "table index size in bytes")
	fs.BoolVar(&o.jsonOut, "json", false, "JSON output")
	fs.Bool("no-color", false, "never use colour (already off for a pipe, NO_COLOR, TERM=dumb)")

	if missing := missingLiveFlag(fs, args); explainMissingLive("plan", missing, args, stderr) {
		return ExitUsage
	}
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	// --- Load schema -------------------------------------------------------
	var (
		tbl     model.Table
		src     string
		ignored []string
		srv     model.Server
	)

	switch {
	case o.schemaPath != "":
		data, err := os.ReadFile(o.schemaPath)
		if err != nil {
			fmt.Fprintf(stderr, "saksama plan: %v\n", err)
			return ExitUsage
		}
		var payload schemaFile
		if err := json.Unmarshal(data, &payload); err != nil {
			fmt.Fprintf(stderr, "saksama plan: %s is not a valid schema file: %v\n", o.schemaPath, err)
			return ExitUsage
		}
		tbl = payload.Table
		srv = payload.Server
		src = "json"

	case o.ddlPath != "":
		ddl, err := readInput(o.ddlPath)
		if err != nil {
			fmt.Fprintf(stderr, "saksama plan: %v\n", err)
			return ExitUsage
		}
		parsed, ign, err := parser.ParseCreateTable(ddl)
		if err != nil {
			fmt.Fprintf(stderr, "saksama plan: cannot parse table definition: %v\n", err)
			return ExitUsage
		}
		tbl, ignored, src = parsed, ign, "ddl"

	default:
		fmt.Fprintf(stderr, "saksama plan: provide --ddl <file|-> or --schema <file>\n")
		return ExitUsage
	}

	if o.mysqlVer != "" {
		srv.Version = o.mysqlVer
	}
	if o.rows > 0 {
		tbl.Rows = o.rows
	}
	if o.dataBytes > 0 {
		tbl.DataLength = o.dataBytes
	}
	if o.indexBytes > 0 {
		tbl.IndexLength = o.indexBytes
	}
	if srv.Vendor == "" {
		srv.Vendor = "mysql"
	}

	if o.dumpSchema {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(schemaFile{Server: srv, Table: tbl}); err != nil {
			fmt.Fprintf(stderr, "saksama plan: %v\n", err)
			return ExitInternal
		}
		return ExitOK
	}

	// --- Parse the ALTER ---------------------------------------------------
	if strings.TrimSpace(o.alter) == "" {
		fmt.Fprintf(stderr, "saksama plan: --alter is required\n")
		return ExitUsage
	}
	alter, err := parser.ParseAlter("ALTER TABLE `" + tbl.Name + "` " + alterStatementBody(o.alter))
	if err == nil && alter.Table == "" {
		alter.Table = tbl.Name
	}
	if err == nil {
		alterSchema := tbl.Schema
		if alterSchema != "" {
			alter.Schema = alterSchema
		}
	}
	if err != nil {
		// Fail closed: an unparseable ALTER is reported through the plan rather
		// than crashing, so CI still receives a machine-readable blocker.
		// The synthetic unknown operation guarantees the plan can never be
		// reported as ready when nothing was actually understood.
		alter = parser.Alter{Ops: []model.Op{{Kind: model.OpUnknown, Raw: strings.TrimSpace(o.alter)}}}
		ignored = append(ignored, "ALTER statement could not be parsed: "+err.Error())
	}

	in := analyzer.Input{
		Server:             srv,
		Table:              tbl,
		Ops:                alter.Ops,
		RawAlter:           strings.TrimSpace(o.alter),
		Algorithm:          alter.Algorithm,
		Lock:               alter.Lock,
		Host:               o.host,
		Production:         o.production,
		AcceptUnknownMDL:   o.acceptUnknownMDL,
		AllowNativeRebuild: o.allowNativeRebuild,
		SchemaSource:       src,
		Ignored:            ignored,
		FreeDisk:           o.freeDisk,
		Disk:               analyzer.DiskFacts{Free: o.freeDisk, Total: o.diskTotal, Measured: o.freeDisk > 0, Source: "operator"},
	}
	// Engine precedence: flag, then the choice recorded for this table, then the
	// analyzer's recommendation.
	if o.engine != "" {
		e, err := parseEngine(o.engine)
		if err != nil {
			fmt.Fprintf(stderr, "saksama plan: %v\n", err)
			return ExitUsage
		}
		in.EngineHint = e
		in.EngineHintSource = "flag"
	} else if choice, err := loadEngineChoice(tbl.Schema, tbl.Name); err == nil && choice != nil {
		if e, err := parseEngine(choice.Engine); err == nil {
			in.EngineHint = e
			in.EngineHintSource = "recorded"
		}
	}

	plan := analyzer.Analyze(in)

	if o.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(plan); err != nil {
			fmt.Fprintf(stderr, "saksama plan: %v\n", err)
			return ExitInternal
		}
	} else {
		if err := render.PlanCard(stdout, plan); err != nil {
			fmt.Fprintf(stderr, "saksama plan: %v\n", err)
			return ExitInternal
		}
	}

	if plan.Recommendation.Blockers > 0 {
		return ExitBlocked
	}
	return ExitOK
}

// alterStatementBody accepts either a full ALTER TABLE statement or just the
// operation list, because both are natural to type.
func alterStatementBody(s string) string {
	l := strings.ToLower(strings.TrimSpace(s))
	if strings.HasPrefix(l, "alter table") {
		return strings.TrimSpace(s)[len("alter table"):]
	}
	return s
}

func readInput(path string) (string, error) {
	if path == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading stdin: %w", err)
		}
		return string(data), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

type schemaFile struct {
	Server model.Server `json:"server"`
	Table  model.Table  `json:"table"`
}

func cmdSchema(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("schema", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if _, err := io.WriteString(stdout, planJSONSchema); err != nil {
		return ExitInternal
	}
	return ExitOK
}

// hasFlag reports whether an exact flag token is present.
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

// removeFlag strips an exact bare flag token.
func removeFlag(args []string, name string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == name {
			continue
		}
		out = append(out, a)
	}
	return out
}

// classifyStoreError picks the exit code for an error that came out of the state
// directory: a session id the operator mistyped is a usage error, and anything
// else (a corrupt file, a full disk) is ours. The commands used to pick their
// own, so the same mistake exited 2 in one command and 3 in another.
func classifyStoreError(err error) int {
	if errors.Is(err, state.ErrSessionNotFound) || errors.Is(err, os.ErrNotExist) {
		return ExitUsage
	}
	return ExitInternal
}
