package wizard

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/app"
	"github.com/perogeremmer/saksama-core/internal/core"
	"github.com/perogeremmer/saksama-core/internal/cred"
	"github.com/perogeremmer/saksama-core/internal/engine"
	"github.com/perogeremmer/saksama-core/internal/events"
	"github.com/perogeremmer/saksama-core/internal/probe"
	"github.com/perogeremmer/saksama-core/internal/render"
	"github.com/perogeremmer/saksama-core/internal/state"
	"github.com/perogeremmer/saksama-core/internal/stream"
)

// Exit codes are the CLI's, because a wizard that invents its own is a second
// contract to maintain.
const (
	ExitOK    = 0
	ExitUsage = 2
)

// errDeclined aborts an execution without touching the database. It is not a
// failure: declining is the normal outcome of reading the statements first.
var errDeclined = errors.New("declined")

// Options are the flags the wizard accepts. Anything else belongs to the command
// the wizard is about to run, and the wizard prints that command.
type Options struct {
	Host     string
	Port     string
	User     string
	Database string
	StateDir string
	NoProbe  bool
	// FreeDisk and DiskTotal are the numbers core cannot measure from here. A
	// remote server always needs them, because its data directory is not on this
	// machine.
	FreeDisk  int64
	DiskTotal int64
}

// Run starts the interactive menu.
func Run(args []string, stateDir string, stdout, stderr io.Writer) int {
	var o Options
	fs := flag.NewFlagSet("wizard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.Host, "h", "", "MySQL host (also: --host)")
	fs.StringVar(&o.Host, "host", "", "MySQL host")
	fs.StringVar(&o.Port, "P", "", "MySQL port (also: --port)")
	fs.StringVar(&o.Port, "port", "", "MySQL port")
	fs.StringVar(&o.User, "u", "", "MySQL user (also: --user)")
	fs.StringVar(&o.User, "user", "", "MySQL user")
	fs.StringVar(&o.Database, "d", "", "database (also: --database)")
	fs.StringVar(&o.Database, "database", "", "database")
	fs.BoolVar(&o.NoProbe, "no-probe", false, "skip detecting installed engine binaries")
	fs.Int64Var(&o.FreeDisk, "free-disk", 0, "free bytes on the data volume, when it cannot be measured from here")
	fs.Int64Var(&o.DiskTotal, "disk-total", 0, "total bytes of the data volume, to apply the 10 percent floor")
	noColor := fs.Bool("no-color", false, "plain text even on a terminal")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	o.StateDir = stateDir

	// A prompt in a pipe is a hang. Say so, and name the commands that do work.
	if f, ok := stdout.(*os.File); !ok || !term.IsTerminal(int(f.Fd())) {
		fmt.Fprintln(stderr, "saksama wizard: this menu needs an interactive terminal.")
		fmt.Fprintln(stderr, "Non-interactive equivalents:")
		fmt.Fprintln(stderr, "  saksama advise --live -h <host> -d <db>")
		fmt.Fprintln(stderr, "  saksama plan --live -h <host> -d <db> -t <table> -a \"<ALTER>\"")
		fmt.Fprintln(stderr, "  saksama apply -h <host> -d <db> -t <table> -a \"<ALTER>\"")
		fmt.Fprintln(stderr, "  saksama attach <session>")
		return ExitUsage
	}

	// Presentation only: colour and the spinner are for the person at the keyboard.
	render.UseColor(shouldColor(stdout, *noColor))

	store, err := app.Store(o.StateDir)
	if err != nil {
		fmt.Fprintf(stderr, "saksama wizard: %v\n", err)
		return ExitUsage
	}

	var binaries map[string]analyzer.EngineBinary
	if !o.NoProbe {
		binaries = probe.Binaries()
	}

	backend := newLive(store, binaries)
	defer backend.Close()

	w := &Wizard{
		p:     NewTerminal(os.Stdin, stdout),
		b:     backend,
		out:   stdout,
		store: store,
		actor: currentActor(),
		conn: cred.Conn{
			Host: o.Host, Port: o.Port, User: o.User, Database: o.Database, TLS: "preferred",
		},
		database:  o.Database,
		freeDisk:  o.FreeDisk,
		diskTotal: o.DiskTotal,
		// The menu already refuses to start without a terminal; the spinner and the
		// colour follow the same rule, so a captured session stays readable.
		animate: true,
	}

	// A host on the command line is an invitation to go straight there, not a
	// promise that the connection works.
	if o.Host != "" {
		w.connectWith(w.conn)
	}
	return w.loop()
}

// Wizard is the menu state: what is connected, which database is in scope, and
// which schema was last read. It holds no policy.
type Wizard struct {
	p     Prompter
	b     Backend
	out   io.Writer
	store *state.Store
	actor string

	// animate is true only on a real terminal: the spinner writes carriage returns,
	// which in a captured log turn into a single unreadable line.
	animate   bool
	conn      cred.Conn
	connected bool
	database  string
	tables    []analyzer.TableAssessment
	freeDisk  int64
	diskTotal int64
}

// shouldColor decides whether this process writes colour: only on a terminal, and only
// when nothing says otherwise.
func shouldColor(out io.Writer, noColorFlag bool) bool {
	ok, _ := render.ShouldColor(out, noColorFlag)
	return ok
}

func (w *Wizard) loop() int {
	for {
		w.p.Printf("\n%s\n", render.Dim(strings.Repeat("-", 44)))
		w.p.Printf("%s  %s\n", render.Bold("Saksama"), render.Dim("examine before you alter"))
		// The first line of every screen answers "where am I", which is the thing a
		// menu with a persistent connection most needs to say.
		if w.connected {
			w.p.Printf("%s %s as %s, database %s\n",
				render.Good("connected:"), w.conn.Describe(), w.conn.User, orDash(w.database))
		} else {
			w.p.Printf("%s %s\n", render.Warn("not connected"), render.Dim("- choose 1 to begin"))
		}

		connectNote := ""
		if w.connected {
			connectNote = w.conn.Describe()
		}
		choice, err := w.p.Choose("What do you want to do?", []Item{
			{"1", "Connect to a server", connectNote},
			{"2", "Inspect the server", "settings, engine binaries, disk"},
			{"3", "Inspect tables and engines", "what a rebuild would cost, per table"},
			{"4", "Plan a change", "build an ALTER, read the plan"},
			{"5", "Run a planned change", "execute a recorded plan"},
			{"6", "Sessions", "follow, cancel, cutover, cleanup"},
			{"7", "Reconcile", "sessions that never finished"},
			{"8", "Engine choice", "record which engine to use, per table"},
			{"0", "Exit", ""},
		}, "0")
		if err != nil {
			// End of input: a pipe that ran dry, or ctrl-d. Leaving quietly is the
			// correct answer, not an error page.
			w.p.Printf("\n")
			return ExitOK
		}

		switch choice {
		case "1":
			w.connect()
		case "2":
			w.inspect()
		case "3":
			w.tablesScreen()
		case "4":
			w.plan()
		case "5":
			w.apply()
		case "6":
			w.sessions()
		case "7":
			w.reconcile()
		case "8":
			w.engineChoice()
		case "0":
			return ExitOK
		}
	}
}

// acceptUnknownMDL asks the one question a blocker can be answered with.
//
// The wizard holds no policy of its own, so it does not decide which blockers are
// acceptable: it asks only when the analyzer's own plan carries E5b as a BLOCKER,
// which is the check whose remedy is an explicit operator acknowledgement. A
// "no" leaves the plan refused, which is the correct outcome.
func (w *Wizard) acceptUnknownMDL(plan analyzer.Plan) bool {
	acceptable := false
	for _, c := range plan.Checks {
		if c.ID == "E5b" && c.Severity == analyzer.SeverityBlocker {
			acceptable = true
		}
	}
	if !acceptable {
		return false
	}
	ok, err := w.p.Confirm(
		"MySQL 5.6 cannot report its metadata lock queue. Accept that risk?", false)
	return err == nil && ok
}

// allowNativeRebuild asks whether MySQL may rebuild this table in place.
//
// Only asked when the analyzer's plan carries R13, which is the check that says
// native was passed over for exactly this reason. The question matters on a stock
// 5.6 or 5.7 server: with no binary log gh-ost cannot run, and pt-osc may not be
// installed, so the choice is between installing a copy engine and letting the
// server rebuild a small table itself. Neither answer is wrong; only the operator
// knows whether this table may be paused, and a "no" keeps the plan refused.
func (w *Wizard) allowNativeRebuild(plan analyzer.Plan) bool {
	offered := false
	for _, c := range plan.Checks {
		if c.ID == "R13" {
			offered = true
			break
		}
	}
	if !offered {
		return false
	}
	ok, err := w.p.Confirm(
		"MySQL can rebuild this table in place, with nothing to install. It cannot be paused or throttled. Allow it?", false)
	return err == nil && ok
}

// reanalyse re-plans with an acknowledgement applied and shows the result, so the
// screen never keeps a blocker it has just been told to drop.
func (w *Wizard) reanalyse(req core.Request, flag string) (*core.Prepared, error) {
	stop := w.busy("planning again")
	prep, err := w.b.Analyze(context.Background(), req)
	stop()
	if err != nil {
		return nil, err
	}
	w.p.Printf("\n%s\n", render.Accent("re-read with "+flag+":"))
	if err := render.PlanCard(w.out, prep.Plan); err != nil {
		return nil, err
	}
	return prep, nil
}

// tlsUnusable reports whether a connection error is the TLS negotiation failing,
// rather than a wrong password or a missing host.
func tlsUnusable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "tls: ") || strings.Contains(msg, "x509: ")
}

// ensureTables reads the schema once per session, so a table can be picked from a
// list rather than typed. Every screen that offers a table needs the same read.
func (w *Wizard) ensureTables() bool {
	if w.tables != nil {
		return true
	}
	started := time.Now()
	stop := w.busy("reading the schema of " + orDash(w.database))
	adv, err := w.b.Schema(context.Background(), w.database, "")
	stop()
	_ = started
	if err != nil {
		w.fail(err)
		return false
	}
	w.tables = adv.Tables
	return true
}

// engineChoice records which engine to use, because the verdict on the tables
// screen is only useful if the decision it implies can be written down without
// leaving the menu.
//
// It is a decision, not an action: nothing is installed, nothing downloads, and
// `engine clear` undoes it. Installing a binary stays outside the menu.
func (w *Wizard) engineChoice() {
	choices, err := w.b.EngineChoices(context.Background())
	if err != nil {
		w.fail(err)
		return
	}

	w.p.Printf("\n%s\n", render.Bold("ENGINE CHOICE"))
	if len(choices) == 0 {
		w.p.Printf("  nothing recorded yet: plan and apply follow the analyzer's recommendation\n")
	} else {
		for _, c := range choices {
			note := c.Note
			if note != "" {
				note = "  (" + shorten(note, 40) + ")"
			}
			w.p.Printf("  %-16s %-8s%s\n", c.Label(), c.Engine, note)
		}
	}
	w.p.Printf("\n  nothing installed here; `saksama engine doctor` shows what is missing\n")

	action, err := w.p.Choose("What do you want to do?", []Item{
		{"1", "Record a choice", "engines: native, gh-ost, pt-osc"},
		{"2", "Forget a choice", "back to the analyzer's recommendation"},
		{"0", "Back", ""},
	}, "0")
	if err != nil || action == "0" {
		return
	}

	scope, err := w.p.Choose("Which scope?", []Item{
		{"1", "One table", orDash(w.database)},
		{"2", "Every table in a database", orDash(w.database)},
		{"3", "Everywhere", "global"},
		{"0", "Back", ""},
	}, "0")
	if err != nil || scope == "0" {
		return
	}

	var database, table string
	global := scope == "3"
	switch scope {
	case "1":
		if !w.requireConnection() || !w.ensureTables() {
			return
		}
		table = w.chooseTable()
		if table == "" {
			return
		}
		database = w.database
	case "2":
		if !w.requireConnection() {
			return
		}
		database = w.database
	}

	if action == "2" {
		cleared, err := w.b.ClearEngine(database, table, global)
		if err != nil {
			w.fail(err)
			return
		}
		w.p.Printf("\nforgot the choice for %s\n", cleared.Label())
		w.p.Printf("equivalent: %s\n", core.EngineClearCommandLine(cleared))
		return
	}

	engine, err := w.p.Choose("Which engine?", []Item{
		{"1", "native", "MySQL does it in place, no copy"},
		{"2", "gh-ost", "rebuild through the binlog"},
		{"3", "pt-osc", "rebuild through triggers"},
		{"0", "Back", ""},
	}, "0")
	if err != nil || engine == "0" {
		return
	}
	names := map[string]string{"1": "native", "2": "gh-ost", "3": "pt-osc"}

	note, err := w.p.Ask("why? (recorded in the audit trail, optional)", "")
	if err != nil {
		return
	}

	choice, err := w.b.SetEngine(names[engine], database, table, global, note)
	if err != nil {
		w.fail(err)
		return
	}
	w.p.Printf("\nrecorded %s for %s\n", choice.Engine, choice.Label())
	w.p.Printf("equivalent: %s\n", core.EngineChoiceCommandLine(choice))
}

func (w *Wizard) fail(err error) {
	w.p.Printf("\n%v\n", err)
}

// requireConnection is the one guard every screen shares: no screen invents a
// connection, and none of them silently runs against the wrong server.
func (w *Wizard) requireConnection() bool {
	if w.connected {
		return true
	}
	w.p.Printf("\nconnect to a server first (option 1)\n")
	return false
}

// --- connect ----------------------------------------------------------------

func (w *Wizard) connect() {
	w.p.Printf("\n%s\n", render.Bold("CONNECT"))

	host, err := w.p.Ask("host", defaultOf(w.conn.Host, "127.0.0.1"))
	if err != nil {
		return
	}
	port, err := w.p.Ask("port", defaultOf(w.conn.Port, "3306"))
	if err != nil {
		return
	}
	dbUser, err := w.p.Ask("user", defaultOf(w.conn.User, "root"))
	if err != nil {
		return
	}
	database, err := w.p.Ask("database", w.database)
	if err != nil {
		return
	}

	// Keep whatever worked last time: a server without usable TLS should be asked
	// about once, not on every reconnect.
	tls := w.conn.TLS
	if tls == "" {
		tls = "preferred"
	}
	w.connectWith(cred.Conn{Host: host, Port: port, User: dbUser, Database: database, TLS: tls})
}

func (w *Wizard) connectWith(c cred.Conn) {
	// Option files and the environment are tried first. Asking for a password
	// that is already in ~/.my.cnf trains people to type secrets for no reason.
	resolved, err := cred.Resolve(cred.Options{
		Host: c.Host, Port: c.Port, User: c.User, Database: c.Database, TLS: c.TLS,
	})
	if err != nil {
		w.p.Printf("note: %v\n", err)
	} else {
		c.Password = resolved.Password
	}
	if c.Password == "" {
		secret, err := w.p.Secret("password (empty when the user has none)")
		if err != nil {
			return
		}
		c.Password = secret
	}

	started := time.Now()
	stop := w.busy("connecting to " + c.Describe())
	err = w.b.Connect(c)
	stop()
	if err != nil {
		w.p.Printf("\ncould not connect: %v\n", err)
		// The CLI has --tls; the menu had no way to say the same thing, so a server
		// whose TLS this client cannot negotiate was a dead end here. Ask, and only
		// when the error is actually about TLS: disabling it is a security decision
		// and the person at the keyboard is the one who knows the network path.
		if !tlsUnusable(err) || c.TLS == "false" {
			return
		}
		ok, cerr := w.p.Confirm(
			"this server's TLS handshake fails; connect without TLS instead? (only on a trusted link)", false)
		if cerr != nil || !ok {
			return
		}
		c.TLS = "false"
		stop := w.busy("connecting without TLS")
		retryErr := w.b.Connect(c)
		stop()
		if retryErr != nil {
			w.p.Printf("\ncould not connect without TLS either: %v\n", retryErr)
			return
		}
		w.p.Printf("\nnote: this session runs without TLS; the connection is not encrypted\n")
	}

	w.conn = c
	w.database = c.Database
	w.connected = true
	w.tables = nil

	w.note("connected", started)
	w.p.Printf("\nconnected to %s as %s\n", c.Describe(), c.User)
	w.p.Printf("equivalent: saksama advise --live -h %s -P %s -u %s -d %s",
		c.Host, c.Port, c.User, orDash(c.Database))
	if c.TLS == "false" {
		w.p.Printf(" --tls false")
	}
	w.p.Printf("\n")
}

// --- inspect ----------------------------------------------------------------

func (w *Wizard) inspect() {
	if !w.requireConnection() {
		return
	}
	lists, err := w.b.Doctor(context.Background())
	if err != nil {
		w.fail(err)
		return
	}
	_ = render.Doctor(w.out, lists, true)
	w.p.Printf("\nequivalent: saksama engine doctor -h %s -u %s -d %s\n",
		w.conn.Host, w.conn.User, orDash(w.database))
}

func (w *Wizard) tablesScreen() {
	if !w.requireConnection() {
		return
	}
	started := time.Now()
	stop := w.busy("reading the schema of " + orDash(w.database))
	adv, err := w.b.Schema(context.Background(), w.database, "")
	stop()
	_ = started
	if err != nil {
		w.fail(err)
		return
	}
	w.tables = adv.Tables

	if err := render.Environment(w.out, adv.SettingChecks(), true); err != nil {
		w.fail(err)
		return
	}
	w.p.Printf("\n")
	if err := render.SchemaReportStyled(w.out, adv.Database, adv.Tables, adv.Artifacts, adv.Server, true); err != nil {
		w.fail(err)
		return
	}
	w.p.Printf("\nequivalent: saksama advise --live -h %s -d %s\n", w.conn.Host, w.database)
}

// --- plan -------------------------------------------------------------------

func (w *Wizard) plan() {
	if !w.requireConnection() {
		return
	}
	if !w.ensureTables() {
		return
	}

	table := w.chooseTable()
	if table == "" {
		return
	}
	alter := w.buildAlter(table)
	if alter == "" {
		return
	}

	production, err := w.p.Confirm("Is this a production target?", false)
	if err != nil {
		return
	}

	req := core.Request{
		Conn:       w.conn,
		Database:   w.database,
		Table:      table,
		Alter:      alter,
		Production: production,
		Actor:      w.actor,
		Binaries:   w.b.Binaries(),
		FreeDisk:   w.freeDisk,
		DiskTotal:  w.diskTotal,
	}

	// Analyze first: it writes nothing at all, so reading a plan never leaves a
	// session behind.
	stop := w.busy("planning against " + req.Conn.Describe())
	prep, err := w.b.Analyze(context.Background(), req)
	stop()
	if err != nil {
		w.fail(err)
		return
	}
	if err := render.PlanCard(w.out, prep.Plan); err != nil {
		w.fail(err)
		return
	}

	// One blocker can be accepted rather than fixed, and only the operator may
	// decide that. The analyzer names it; the wizard does not go looking for a
	// server version, so this also covers a 5.7/8.0 user who cannot read the lock
	// queue for lack of a grant.
	if prep != nil && prep.Plan.Recommendation.Blockers > 0 && w.acceptUnknownMDL(prep.Plan) {
		req.AcceptUnknownMDL = true
		prep, err = w.reanalyse(req, "--accept-unknown-mdl")
		if err != nil {
			w.fail(err)
			return
		}
		if prep == nil {
			return
		}
	}

	// Asked on the second plan, not the first: on 5.6 in production both questions
	// arrive together, and the lock-queue answer changes what the rebuild one says.
	if prep != nil && prep.Plan.Recommendation.Blockers > 0 && w.allowNativeRebuild(prep.Plan) {
		req.AllowNativeRebuild = true
		prep, err = w.reanalyse(req, "--allow-native-rebuild")
		if err != nil {
			w.fail(err)
			return
		}
		if prep == nil {
			return
		}
	}

	if prep.Plan.Recommendation.Blockers > 0 {
		w.p.Printf("\nthis change would be refused; fix the BLOCKER lines above first\n")
		w.p.Printf("equivalent: %s\n", req.CommandLineFor("plan --live"))
		return
	}

	record, err := w.p.Confirm("Record this plan so it can be run later?", true)
	if err != nil || !record {
		w.p.Printf("\nnothing was recorded\n")
		w.p.Printf("equivalent: %s\n", req.CommandLineFor("plan --live"))
		return
	}

	// Recording re-reads the server. It is a handful of introspection queries and
	// it guarantees the recorded fingerprint matches the plan being shown.
	recorded, err := w.b.Prepare(context.Background(), req)
	if err != nil {
		w.fail(err)
		return
	}
	defer recorded.Close()
	w.p.Printf("\nSESSION    %s (recorded; nothing was changed)\n", recorded.Session.ID)
	w.p.Printf("run it with option 5, or: saksama apply --session %s\n", recorded.Session.ID)
}

func (w *Wizard) chooseTable() string {
	items := make([]Item, 0, len(w.tables)+2)
	for _, t := range w.tables {
		items = append(items, Item{
			Key:   t.Table,
			Label: t.Table,
			Note:  fmt.Sprintf("%s rows, rebuild: %s", humanCount(t.Rows), t.RebuildEngine),
		})
	}
	items = append(items, Item{Key: "-", Label: "Type a table name", Note: ""})
	items = append(items, Item{Key: "0", Label: "Back", Note: ""})

	choice, err := w.p.Choose("Which table?", items, "0")
	if err != nil || choice == "0" {
		return ""
	}
	if choice == "-" {
		name, err := w.p.Ask("table name", "")
		if err != nil {
			return ""
		}
		return strings.TrimSpace(name)
	}
	return choice
}

// buildAlter composes the ALTER body.
//
// It only assembles text: the analyzer is the thing that decides whether the
// result is supported, cheap, or refused. A wizard that pre-judged the SQL would
// be a second, quieter set of rules.
func (w *Wizard) buildAlter(table string) string {
	kind, err := w.p.Choose(fmt.Sprintf("Change to make on %s", table), []Item{
		{"1", "ADD INDEX", "a secondary index; often no copy at all"},
		{"2", "ADD UNIQUE INDEX", "refused when duplicates exist"},
		{"3", "ADD COLUMN", ""},
		{"4", "MODIFY COLUMN", "type or nullability change; often a copy"},
		{"5", "DROP INDEX", ""},
		{"6", "DROP COLUMN", "destructive"},
		{"7", "Type the ALTER myself", ""},
		{"0", "Back", ""},
	}, "0")
	if err != nil || kind == "0" {
		return ""
	}

	ask := func(label, def string) string {
		v, err := w.p.Ask(label, def)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(v)
	}

	switch kind {
	case "1", "2":
		kindName := "INDEX"
		if kind == "2" {
			kindName = "UNIQUE INDEX"
		}
		name := ask("index name", "")
		cols := ask("columns, comma separated", "")
		if name == "" || cols == "" {
			w.p.Printf("\nname and columns are both required\n")
			return ""
		}
		return fmt.Sprintf("ADD %s %s (%s)", kindName, name, cols)

	case "3":
		name := ask("column name", "")
		def := ask("column definition", "varchar(64) NOT NULL")
		if name == "" || def == "" {
			w.p.Printf("\nname and definition are both required\n")
			return ""
		}
		return fmt.Sprintf("ADD COLUMN %s %s", name, def)

	case "4":
		name := ask("column name", "")
		def := ask("new definition, for example varchar(64) NOT NULL", "")
		if name == "" || def == "" {
			w.p.Printf("\nname and definition are both required\n")
			return ""
		}
		return fmt.Sprintf("MODIFY COLUMN %s %s", name, def)

	case "5":
		name := ask("index name", "")
		if name == "" {
			return ""
		}
		return "DROP INDEX " + name

	case "6":
		name := ask("column name", "")
		if name == "" {
			return ""
		}
		ok, err := w.p.Confirm(fmt.Sprintf("Dropping %s.%s cannot be undone. Continue?", table, name), false)
		if err != nil || !ok {
			return ""
		}
		return "DROP COLUMN " + name

	case "7":
		body := ask("ALTER body, without the ALTER TABLE ... part", "")
		return body
	}
	return ""
}

// --- apply ------------------------------------------------------------------

func (w *Wizard) apply() {
	if !w.requireConnection() {
		return
	}
	list, err := w.store.List()
	if err != nil {
		w.fail(err)
		return
	}

	items := []Item{}
	for _, s := range list {
		if s.State != state.StatePlanned {
			continue
		}
		items = append(items, Item{
			Key:   s.ID,
			Label: s.Database + "." + s.Table,
			Note:  fmt.Sprintf("%-8s %s", s.Engine, shorten(s.Alter, 44)),
		})
	}
	if len(items) == 0 {
		w.p.Printf("\nno planned session is waiting to run; plan a change first (option 4)\n")
		return
	}
	items = append(items, Item{Key: "0", Label: "Back"})

	id, err := w.p.Choose("Run which plan?", items, "0")
	if err != nil || id == "0" {
		return
	}

	sess, err := w.store.Load(id)
	if err != nil {
		w.fail(err)
		return
	}
	_ = render.Session(w.out, sess)

	if sess.Production {
		typed, err := w.p.Ask(fmt.Sprintf("production target: type %q to confirm", w.conn.Describe()), "")
		if err != nil || typed != w.conn.Describe() {
			w.p.Printf("\nnot confirmed; nothing was executed\n")
			return
		}
	}

	req := core.Request{
		Conn:                   w.conn,
		Database:               sess.Database,
		Table:                  sess.Table,
		Alter:                  sess.Alter,
		Production:             sess.Production,
		Actor:                  w.actor,
		Binaries:               w.b.Binaries(),
		ForExecution:           true,
		LockWaitTimeoutSeconds: 10,
		ResumedFrom:            id,
		FreeDisk:               w.freeDisk,
		DiskTotal:              w.diskTotal,
		Approve:                w.approve,
		Progress: func(p engine.Progress) {
			w.p.Printf("  [%5.1f%%] %s\n", p.Percent, p.Note)
		},
	}

	if w.freeDisk == 0 {
		w.p.Printf("\nnote: free disk was not supplied, so a run may be refused for an\n")
		w.p.Printf("      unmeasurable volume. Restart with --free-disk and --disk-total.\n")
	}

	// Re-reading before executing answers one question the session record cannot:
	// does this run need the operator to accept a risk that only exists on some
	// servers? It writes nothing, so asking here costs a handful of queries.
	if preview, err := w.b.Analyze(context.Background(), req); err == nil && preview != nil &&
		preview.Plan.Recommendation.Blockers > 0 {
		if w.acceptUnknownMDL(preview.Plan) {
			req.AcceptUnknownMDL = true
			preview, err = w.b.Analyze(context.Background(), req)
			if err != nil || preview == nil {
				preview = &core.Prepared{Plan: analyzer.Plan{Recommendation: analyzer.Recommendation{Blockers: 1}}}
			}
		}
		if preview.Plan.Recommendation.Blockers > 0 && w.allowNativeRebuild(preview.Plan) {
			req.AllowNativeRebuild = true
		}
	}
	w.p.Printf("\nequivalent: %s\n", req.CommandLineFor("apply"))

	res, err := w.b.ApplyFromSession(context.Background(), id, req)
	if res != nil {
		_ = render.Session(w.out, res)
	}
	if err != nil {
		w.p.Printf("\nstopped: %v\n", err)
	}
}

// approve is the human gate. It shows the statements that will run, which is the
// only thing worth approving: the plan card is a summary, and a summary is not
// what executes.
func (w *Wizard) approve(ar core.ApprovalRequest) (state.Approval, error) {
	w.p.Printf("\n%s\n", render.Bold("STATEMENTS"))
	for i, st := range ar.Statements {
		w.p.Printf("  %d. %s\n", i+1, st.SQL)
	}
	if ar.RiskNote != "" {
		w.p.Printf("\nwhy: %s\n", ar.RiskNote)
	}
	w.p.Printf("plan %s on %s\n", ar.PlanFingerprint, ar.Host)

	ok, err := w.p.Confirm("Execute these statements now?", false)
	if err != nil {
		return state.Approval{}, err
	}
	if !ok {
		return state.Approval{}, errDeclined
	}
	return state.Approval{
		Action:          ar.Action,
		Actor:           w.actor,
		Decision:        "granted",
		Note:            "approved in the terminal wizard",
		PlanFingerprint: ar.PlanFingerprint,
	}, nil
}

// --- sessions ---------------------------------------------------------------

func (w *Wizard) sessions() {
	list, err := w.store.List()
	if err != nil {
		w.fail(err)
		return
	}
	if len(list) == 0 {
		w.p.Printf("\nno sessions recorded in %s\n", w.store.Dir())
		return
	}

	items := make([]Item, 0, len(list)+1)
	for _, s := range list {
		note := string(s.State)
		if s.Engine != "" && s.Engine != "none" {
			note += " / " + s.Engine
		}
		items = append(items, Item{
			Key:   s.ID,
			Label: s.Database + "." + s.Table,
			Note:  note,
		})
	}
	items = append(items, Item{Key: "0", Label: "Back"})

	id, err := w.p.Choose("Which session?", items, "0")
	if err != nil || id == "0" {
		return
	}
	sess, err := w.store.Load(id)
	if err != nil {
		w.fail(err)
		return
	}
	_ = render.Session(w.out, sess)

	action, err := w.p.Choose(fmt.Sprintf("Session %s", id), []Item{
		{"1", "Follow the event log", "ctrl-c returns to the menu"},
		{"2", "Cancel the engine", "only stops a running copy"},
		{"3", "Release a held cutover", "allows the swap"},
		{"4", "Cleanup, dry run", "show what would be dropped"},
		{"5", "Cleanup", "drop only what this session owns"},
		{"0", "Back", ""},
	}, "0")
	if err != nil {
		return
	}

	switch action {
	case "1":
		w.follow(id)
	case "2":
		// SIGKILL is offered as a second step, not as the default: an engine that
		// is killed outright cannot drop its own shadow table and triggers.
		force, err := w.p.Confirm(
			"If it ignores SIGTERM, kill it (SIGKILL)? The engine will leave debris behind.", false)
		if err != nil {
			return
		}
		res, err := w.b.Cancel(context.Background(), id, force)
		if err != nil {
			w.fail(err)
			return
		}
		w.p.Printf("%s\n", res.Note)
	case "3":
		if _, err := w.b.ReleaseCutover(context.Background(), id); err != nil {
			w.fail(err)
			return
		}
		w.p.Printf("cutover released; the engine will swap now. There is no data rollback after this point.\n")
	case "4":
		res, err := w.b.Cleanup(context.Background(), id, true)
		if err != nil {
			w.fail(err)
			return
		}
		w.printCleanup(res)
	case "5":
		res, err := w.b.Cleanup(context.Background(), id, false)
		if err != nil {
			w.fail(err)
			return
		}
		w.printCleanup(res)
	}
}

func (w *Wizard) printCleanup(res core.CleanupResult) {
	if res.Refused != "" {
		w.p.Printf("%s\nnothing was dropped\n", res.Refused)
		return
	}
	for _, a := range res.Items {
		switch {
		case a.Err != "":
			w.p.Printf("  FAILED  %-8s %s: %s\n", a.Kind, a.Name, a.Err)
		case res.DryRun:
			w.p.Printf("  would drop %-8s %s\n", a.Kind, a.Name)
		default:
			w.p.Printf("  dropped %-8s %s\n", a.Kind, a.Name)
		}
	}
}

func (w *Wizard) follow(id string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	w.p.Printf("\nfollowing %s; ctrl-c returns to the menu\n\n", id)
	s := stream.Follow(ctx, w.store.EventLogPath(id), 0)
	for ev := range s.Events {
		w.printEvent(ev)
	}
	w.p.Printf("\n(stopped following; the migration itself is unaffected)\n")
}

func (w *Wizard) printEvent(ev events.Event) {
	if line := render.EventLine(ev); line != "" {
		w.p.Printf("%s\n", line)
	}
}

// --- reconcile --------------------------------------------------------------

func (w *Wizard) reconcile() {
	if !w.requireConnection() {
		return
	}
	findings, err := w.b.Reconcile(context.Background())
	if err != nil {
		w.fail(err)
		return
	}
	if len(findings) == 0 {
		w.p.Printf("\nno session is in an unfinished state\n")
		return
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
	_ = table.Render(w.out)

	w.p.Printf("\nNothing was changed. An unfinished session has to be observed before\n")
	w.p.Printf("anyone touches it: it is neither obviously failed nor obviously running.\n")
}

// --- small helpers ----------------------------------------------------------

func currentActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return "unknown"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func defaultOf(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "~"
}

func humanCount(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	}
	return fmt.Sprint(n)
}
