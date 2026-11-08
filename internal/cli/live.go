package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/app"
	"github.com/perogeremmer/saksama-core/internal/core"
	"github.com/perogeremmer/saksama-core/internal/cred"
	"github.com/perogeremmer/saksama-core/internal/engine"
	"github.com/perogeremmer/saksama-core/internal/events"
	"github.com/perogeremmer/saksama-core/internal/probe"
	"github.com/perogeremmer/saksama-core/internal/render"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// connFlags are the shared connection inputs.
type connFlags struct {
	host         string
	port         string
	user         string
	database     string
	tls          string
	defaultsFile string
	askPass      bool
	noDefaults   bool
}

func addConnFlags(fs *flag.FlagSet, c *connFlags) {
	fs.StringVar(&c.host, "h", "", "MySQL host (also: --host)")
	fs.StringVar(&c.host, "host", "", "MySQL host")
	fs.StringVar(&c.port, "P", "", "MySQL port (also: --port)")
	fs.StringVar(&c.port, "port", "", "MySQL port")
	fs.StringVar(&c.user, "u", "", "MySQL user (also: --user)")
	fs.StringVar(&c.user, "user", "", "MySQL user")
	fs.StringVar(&c.database, "d", "", "database (also: --database)")
	fs.StringVar(&c.database, "database", "", "database")
	fs.StringVar(&c.tls, "tls", "preferred", "TLS mode: preferred|true|false|skip-verify")
	fs.StringVar(&c.defaultsFile, "defaults-extra-file", "", "read credentials from this option file")
	fs.BoolVar(&c.askPass, "ask-pass", false, "prompt for the password when none is found")
	fs.BoolVar(&c.noDefaults, "no-defaults", false, "do not read ~/.my.cnf")
}

func (c *connFlags) resolve() (cred.Conn, error) {
	return cred.Resolve(cred.Options{
		Host: c.host, Port: c.port, User: c.user, TLS: c.tls, Database: c.database,
		DefaultsFile: c.defaultsFile, AskPass: c.askPass, NoDefaults: c.noDefaults,
		Stdin: os.Stdin,
	})
}

// openDB is the CLI's spelling of app.Connect. It stays a function so every
// command keeps one call site, and so the wizard and an agent can use the same
// connection logic without going through the CLI.
func openDB(c cred.Conn) (*sql.DB, error) {
	return app.Connect(c)
}

func currentActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return "unknown"
}

// newStore honours the global --state-dir flag, and otherwise the default
// location. Every command resolves it the same way, so a session recorded by one
// command is the session the next command reads.
func newStore() (*state.Store, error) {
	return app.Store(stateDir)
}

// --- plan (live) ------------------------------------------------------------

type livePlanOpts struct {
	connFlags
	table              string
	alter              string
	production         bool
	acceptUnknownMDL   bool
	allowNativeRebuild bool
	engine             string
	force              bool
	jsonOut            bool
	freeDisk           int64
	diskTotal          int64
}

func cmdPlanLive(args []string, stdout, stderr io.Writer) int {
	var o livePlanOpts
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addConnFlags(fs, &o.connFlags)
	fs.StringVar(&o.table, "t", "", "table (also: --table)")
	fs.StringVar(&o.table, "table", "", "table")
	fs.StringVar(&o.alter, "a", "", "ALTER statement (also: --alter)")
	fs.StringVar(&o.alter, "alter", "", "ALTER statement")
	fs.BoolVar(&o.production, "production", false, "mark the target as production")
	fs.BoolVar(&o.allowNativeRebuild, "allow-native-rebuild", false,
		"in production, let MySQL rebuild a small table in place (no throttle, no pause)")
	fs.BoolVar(&o.acceptUnknownMDL, "accept-unknown-mdl", false,
		"proceed even though the metadata lock queue cannot be observed (MySQL 5.6)")
	fs.StringVar(&o.engine, "engine", "", "force native|gh-ost|pt-osc")
	fs.BoolVar(&o.force, "force", false, "downgrade selected preflight aborts (never capability blockers)")
	fs.BoolVar(&o.jsonOut, "json", false, "JSON output")
	fs.Int64Var(&o.freeDisk, "free-disk", 0, "free bytes on the data volume, when it cannot be measured from here")
	fs.Int64Var(&o.diskTotal, "disk-total", 0, "total bytes of the data volume, to apply the 10 percent floor")
	noProbe := fs.Bool("no-probe", false, "skip detecting installed engines")
	fs.Bool("no-color", false, "accepted for compatibility")

	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if o.table == "" || o.database == "" {
		fmt.Fprintln(stderr, "saksama plan --live: --database and --table are required")
		return ExitUsage
	}
	if strings.TrimSpace(o.alter) == "" {
		fmt.Fprintln(stderr, "saksama plan --live: --alter is required")
		return ExitUsage
	}

	conn, err := o.resolve()
	if err != nil {
		fmt.Fprintf(stderr, "saksama plan: %v\n", err)
		return ExitUsage
	}

	db, err := openDB(conn)
	if err != nil {
		fmt.Fprintf(stderr, "saksama plan: %v\n", err)
		return ExitInternal
	}
	defer db.Close()

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama plan: %v\n", err)
		return ExitInternal
	}

	runner := core.New(db, store)
	req := core.Request{
		Conn:               conn,
		Database:           o.database,
		Table:              o.table,
		Alter:              o.alter,
		Production:         o.production,
		AcceptUnknownMDL:   o.acceptUnknownMDL,
		AllowNativeRebuild: o.allowNativeRebuild,
		Force:              o.force,
		FreeDisk:           o.freeDisk,
		DiskTotal:          o.diskTotal,
		Actor:              currentActor(),
	}
	if !*noProbe {
		req.Binaries = probe.Binaries()
	}
	if code := resolveEngineChoice(&req, o.engine, o.database, o.table, "saksama plan", stderr); code != 0 {
		return code
	}

	ctx := context.Background()
	prep, err := runner.Prepare(ctx, req)
	if err != nil {
		fmt.Fprintf(stderr, "saksama plan: %v\n", err)
		return ExitInternal
	}
	defer prep.Close()

	if o.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(prep.Plan); err != nil {
			fmt.Fprintf(stderr, "saksama plan: %v\n", err)
			return ExitInternal
		}
	} else {
		if err := render.PlanCard(stdout, prep.Plan); err != nil {
			fmt.Fprintf(stderr, "saksama plan: %v\n", err)
			return ExitInternal
		}
		fmt.Fprintf(stdout, "\nSESSION    %s (recorded; nothing was changed)\n", prep.Session.ID)
	}

	if prep.Plan.Recommendation.Blockers > 0 {
		return ExitBlocked
	}
	return ExitOK
}

func parseEngine(s string) (analyzer.Engine, error) {
	switch strings.ToLower(s) {
	case "native":
		return analyzer.EngineNative, nil
	case "gh-ost", "ghost":
		return analyzer.EngineGhOst, nil
	case "pt-osc", "ptosc":
		return analyzer.EnginePtOSC, nil
	}
	return "", fmt.Errorf("unknown engine %q (want native, gh-ost, or pt-osc)", s)
}

// --- apply ------------------------------------------------------------------

type applyOpts struct {
	connFlags
	table              string
	alter              string
	production         bool
	acceptUnknownMDL   bool
	allowNativeRebuild bool
	engine             string
	force              bool
	jsonOut            bool
	confirm            string
	dryRun             bool
	lockWait           int
	chunkSize          int
	holdCutover        bool
	allowArgvPassword  bool
	freeDisk           int64
	diskTotal          int64
	session            string
	detach             bool
}

func cmdApply(args []string, stdout, stderr io.Writer) int {
	var o applyOpts
	o.lockWait = 10

	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addConnFlags(fs, &o.connFlags)
	fs.StringVar(&o.table, "t", "", "table (also: --table)")
	fs.StringVar(&o.table, "table", "", "table")
	fs.StringVar(&o.alter, "a", "", "ALTER statement (also: --alter)")
	fs.StringVar(&o.alter, "alter", "", "ALTER statement")
	fs.BoolVar(&o.production, "production", false, "mark the target as production, requiring --confirm")
	fs.BoolVar(&o.allowNativeRebuild, "allow-native-rebuild", false,
		"in production, let MySQL rebuild a small table in place (no throttle, no pause)")
	fs.BoolVar(&o.acceptUnknownMDL, "accept-unknown-mdl", false,
		"proceed although the metadata lock queue cannot be observed on this server")
	fs.StringVar(&o.engine, "engine", "", "force native|gh-ost|pt-osc")
	fs.BoolVar(&o.force, "force", false, "downgrade selected preflight aborts")
	fs.Int64Var(&o.freeDisk, "free-disk", 0, "free bytes on the data volume, when it cannot be measured from here")
	fs.Int64Var(&o.diskTotal, "disk-total", 0, "total bytes of the data volume, to apply the 10 percent floor")
	fs.StringVar(&o.session, "session", "", "execute the plan stored in this session instead of planning again")
	fs.BoolVar(&o.detach, "detach", false, "run the engine in its own process group, so closing this terminal does not stop it")
	fs.BoolVar(&o.jsonOut, "json", false, "JSON output")
	fs.StringVar(&o.confirm, "confirm", "", "must equal the host name when --production is set")
	fs.BoolVar(&o.dryRun, "dry-run", false, "build statements and approvals without executing")
	fs.IntVar(&o.lockWait, "lock-wait-timeout", 10, "session lock_wait_timeout in seconds")
	fs.IntVar(&o.chunkSize, "chunk-size", 0, "rows per copy chunk (0 keeps the engine default of 1000)")
	fs.BoolVar(&o.holdCutover, "hold-cutover", false, "pause before the swap so you can verify first (engine permitting)")
	fs.BoolVar(&o.allowArgvPassword, "allow-argv-password", false, "permit engines with no option file to receive the password in argv (visible in ps)")
	noProbe := fs.Bool("no-probe", false, "skip detecting installed engines")

	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if o.session == "" && (o.table == "" || o.database == "" || strings.TrimSpace(o.alter) == "") {
		fmt.Fprintln(stderr, "saksama apply: --database, --table, and --alter are required, or pass --session <id>")
		return ExitUsage
	}

	conn, err := o.resolve()
	if err != nil {
		fmt.Fprintf(stderr, "saksama apply: %v\n", err)
		return ExitUsage
	}

	if o.production && o.confirm != conn.Describe() {
		fmt.Fprintf(stderr,
			"refusing to mutate a production target without confirmation.\n"+
				"target is marked production: %s\n"+
				"re-run with: --confirm %s\n", conn.Describe(), conn.Describe())
		return ExitUsage
	}

	db, err := openDB(conn)
	if err != nil {
		fmt.Fprintf(stderr, "saksama apply: %v\n", err)
		return ExitInternal
	}
	defer db.Close()

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama apply: %v\n", err)
		return ExitInternal
	}

	// The run can go quiet for minutes: before the copy starts, while a cutover
	// lock is held, while the session is polled. The indicator covers those
	// silences with the one-line marker the wizard uses on connect and plan, and
	// the progress lines keep their own lines through it. Only a terminal gets the
	// moving frame; a captured run gets the lines it always got.
	busy := render.NewBusy(stderr, !o.jsonOut && render.IsTerminal(stderr))
	runner := core.New(db, store)
	req := core.Request{
		Conn:                   conn,
		Database:               o.database,
		Table:                  o.table,
		Alter:                  o.alter,
		Production:             o.production,
		AcceptUnknownMDL:       o.acceptUnknownMDL,
		AllowNativeRebuild:     o.allowNativeRebuild,
		Force:                  o.force,
		DryRun:                 o.dryRun,
		LockWaitTimeoutSeconds: o.lockWait,
		ChunkSize:              o.chunkSize,
		HoldCutover:            o.holdCutover,
		AllowArgvPassword:      o.allowArgvPassword,
		FreeDisk:               o.freeDisk,
		DiskTotal:              o.diskTotal,
		ForExecution:           true,
		Detach:                 o.detach,
		Actor:                  currentActor(),
		Binaries:               binariesOrNil(*noProbe),
		Progress: func(p engine.Progress) {
			if !o.jsonOut && p.Note != "" {
				busy.Line("  [%5.1f%%] %s", p.Percent, p.Note)
			}
		},
	}
	if code := resolveEngineChoice(&req, o.engine, o.database, o.table, "saksama apply", stderr); code != 0 {
		return code
	}
	req.Approve = func(ar core.ApprovalRequest) (state.Approval, error) {
		return state.Approval{
			Action: ar.Action, Actor: currentActor(), Decision: "granted",
			Note: fmt.Sprintf("plan %s; %d statement(s)", ar.PlanFingerprint, len(ar.Statements)),
		}, nil
	}

	label := "applying " + o.database + "." + o.table
	if o.session != "" {
		label = "running session " + o.session
	}
	stopBusy := busy.Start(label)
	var sess *state.Session
	if o.session != "" {
		// A stored plan already names its engine; --engine still wins.
		sess, err = runner.ApplyFromSession(context.Background(), o.session, req)
	} else {
		sess, err = runner.Apply(context.Background(), req)
	}
	stopBusy()
	if sess != nil {
		if o.jsonOut {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(sess)
		} else {
			_ = render.Session(stdout, sess)
		}
	}
	if err != nil {
		if errors.Is(err, core.ErrBlocked) {
			return ExitBlocked
		}
		fmt.Fprintf(stderr, "saksama apply: %v\n", err)
		// A mistyped --session id is fixable by the operator; anything else is not.
		if errors.Is(err, state.ErrSessionNotFound) {
			return ExitUsage
		}
		return ExitInternal
	}
	return ExitOK
}

// --- status -----------------------------------------------------------------

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	// Users naturally type "status s2 --json"; strip known flags first so the
	// standard library flag parser does not stop at the positional argument.
	jsonRequested := hasFlag(args, "--json")
	plainRequested := hasFlag(args, "--plain")
	args = removeFlag(args, "--json")
	args = removeFlag(args, "--plain")

	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Bool("json", false, "JSON output (accepted in any position)")
	fs.Bool("plain", false, "borderless output (accepted in any position)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	jsonOut := &jsonRequested

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama status: %v\n", err)
		return ExitInternal
	}

	if fs.NArg() == 0 {
		sessions, err := store.List()
		if err != nil {
			fmt.Fprintf(stderr, "saksama status: %v\n", err)
			return ExitInternal
		}
		if *jsonOut {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(sessions)
			return ExitOK
		}
		if len(sessions) == 0 && unreadableNote(store, stdout) {
			return ExitOK
		}
		if len(sessions) == 0 {
			fmt.Fprintln(stdout, "no sessions recorded")
			return ExitOK
		}
		table := render.Table{
			Border:   !plainRequested,
			MaxWidth: 40,
			Columns: []render.Column{
				{Header: "ID"}, {Header: "STATE"}, {Header: "TARGET"},
				{Header: "ENGINE"}, {Header: "ALTER"}, {Header: "CREATED"},
			},
		}
		for _, s := range sessions {
			table.Add(s.ID, describeState(s), s.Database+"."+s.Table, s.Engine, s.Alter, s.CreatedAt)
		}
		_ = table.Render(stdout)
		// A session that exists but cannot be read is the record of a migration
		// that may have left things behind. Hiding it would make this list lie.
		unreadableNote(store, stdout)
		return ExitOK
	}

	sess, err := store.Load(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "saksama status: %v\n", err)
		return classifyStoreError(err)
	}
	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(sess)
		return ExitOK
	}
	_ = render.Session(stdout, sess)
	if sess.State == state.StateBlocked {
		return ExitBlocked
	}
	return ExitOK
}

// unreadableNote prints one line for each session file that cannot be parsed,
// and reports whether anything was printed.
func unreadableNote(store *state.Store, stdout io.Writer) bool {
	bad, err := store.Unreadable()
	if err != nil || len(bad) == 0 {
		return false
	}
	fmt.Fprintf(stdout, "%d session file(s) cannot be read: %s\n", len(bad), strings.Join(bad, ", "))
	fmt.Fprintf(stdout, "their migrations may have left things behind; inspect %s directly\n", store.Dir())
	return true
}

func describeState(s *state.Session) string {
	if s.State == state.StateFailed && s.Error != "" {
		return "failed"
	}
	return string(s.State)
}

// --- events -----------------------------------------------------------------

func cmdEvents(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Bool("json", true, "JSONL output (always on)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "saksama events: session id required")
		return ExitUsage
	}
	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama events: %v\n", err)
		return ExitInternal
	}
	data, err := store.ReadEvents(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "saksama events: %v\n", err)
		return classifyStoreError(err)
	}
	if _, err := stdout.Write(data); err != nil {
		return ExitInternal
	}
	return ExitOK
}

// --- export -----------------------------------------------------------------

type exportDoc struct {
	ContractVersion int               `json:"contract_version"`
	ExportedAt      string            `json:"exported_at"`
	Session         *state.Session    `json:"session"`
	Events          []json.RawMessage `json:"events"`
	Completeness    string            `json:"completeness"`
}

func cmdExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.String("format", "json", "json")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "saksama export: session id required")
		return ExitUsage
	}
	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama export: %v\n", err)
		return ExitInternal
	}
	id := fs.Arg(0)
	sess, err := store.Load(id)
	if err != nil {
		fmt.Fprintf(stderr, "saksama export: %v\n", err)
		return classifyStoreError(err)
	}
	raw, err := store.ReadEvents(id)
	if err != nil {
		fmt.Fprintf(stderr, "saksama export: %v\n", err)
		return classifyStoreError(err)
	}

	doc := exportDoc{
		ContractVersion: events.ContractVersion,
		ExportedAt:      time.Now().UTC().Format(time.RFC3339),
		Session:         sess,
		Completeness:    "complete",
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !json.Valid([]byte(line)) {
			doc.Completeness = "incomplete: event log contains an unparseable line"
			continue
		}
		doc.Events = append(doc.Events, json.RawMessage(line))
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		fmt.Fprintf(stderr, "saksama export: %v\n", err)
		return ExitInternal
	}
	return ExitOK
}

// --- cleanup ----------------------------------------------------------------

func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "~"
}

// resolveEngineChoice applies the documented precedence:
//
//  1. --engine on the command line
//  2. the choice recorded for this table
//  3. the recorded default choice
//  4. the analyzer's recommendation
func resolveEngineChoice(req *core.Request, flagValue, database, table, cmd string, stderr io.Writer) int {
	if flagValue != "" {
		e, err := parseEngine(flagValue)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", cmd, err)
			return ExitUsage
		}
		req.EngineHint = e
		req.EngineHintSource = "flag"
		return 0
	}

	choice, err := loadEngineChoice(database, table)
	if err != nil || choice == nil {
		return 0
	}
	e, err := parseEngine(choice.Engine)
	if err != nil {
		fmt.Fprintf(stderr, "%s: recorded engine %q is not valid: %v\n", cmd, choice.Engine, err)
		return ExitUsage
	}
	req.EngineHint = e
	req.EngineHintSource = "recorded"
	return 0
}

// binariesOrNil probes installed engines unless the operator opted out.
func binariesOrNil(noProbe bool) map[string]analyzer.EngineBinary {
	if noProbe {
		return nil
	}
	return probe.Binaries()
}
