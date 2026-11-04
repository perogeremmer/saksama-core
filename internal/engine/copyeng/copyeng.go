// Package copyeng drives the copy-based engines (gh-ost and
// pt-online-schema-change).
//
// Both are treated the same way here because the wrapper's job is identical:
// build an argv, start the process, poll the database for real progress, and
// verify the result against information_schema. Anything that differs between
// them is a field in Spec, not a separate code path.
package copyeng

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/engine"
	"github.com/perogeremmer/saksama-core/internal/engine/process"
	"github.com/perogeremmer/saksama-core/internal/introspect"
	"github.com/perogeremmer/saksama-core/internal/model"
)

// Kind identifies which engine this is.
type Kind string

const (
	KindGhOst Kind = "gh-ost"
	KindPtOSC Kind = "pt-osc"
)

// Conn carries what the engines need to reach MySQL.
type Conn struct {
	Host     string
	Port     string
	User     string
	Password string
	Socket   string
	// ServerPort is @@port as the server reports it. gh-ost refuses to start when
	// it differs from the connection port, which happens behind any port mapping
	// or proxy, so the difference has to be declared to it explicitly.
	ServerPort int
}

// Spec is everything that differs between the two engines.
type Spec struct {
	Kind Kind
	// Binary is the executable name on PATH.
	Binary string
	// Table is the ghost/shadow table prefix template.
	ShadowPrefix string
	// ShadowSuffix is appended to build the copy table name.
	ShadowSuffix string
}

// tailBuffer keeps the last few lines of engine output.
//
// When an engine fails, its own words are the most useful diagnostic there is;
// the wrapper's job is to surface them, not to summarise them into "exit 1".
type tailBuffer struct {
	lines []string
	max   int
}

func (t *tailBuffer) add(prefix, text string) {
	if t.max == 0 {
		t.max = 40
	}
	t.lines = append(t.lines, prefix+": "+text)
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
}

func (t *tailBuffer) String() string {
	return strings.Join(t.lines, "\n")
}

// Engine implements engine.Engine for a copy-based tool.
type Engine struct {
	spec  Spec
	opts  engine.Options
	conn  Conn
	db    introspect.Querier
	now   func() time.Time
	lines func(process.Line)

	detach         bool
	onProcessStart func(pid int)

	// defaultsFile is a temporary 0600 option file used instead of argv
	// credentials. It is removed when the run finishes.
	defaultsFile string
}

// New creates a copy-engine wrapper.
func New(spec Spec, db introspect.Querier, conn Conn, opts engine.Options, onLine func(process.Line)) *Engine {
	return &Engine{spec: spec, opts: opts, conn: conn, db: db, now: time.Now, lines: onLine}
}

// Name implements engine.Engine.
func (e *Engine) Name() engine.Name {
	if e.spec.Kind == KindGhOst {
		return engine.GhOst
	}
	return engine.PtOSC
}

// binary is the executable to run: the one the plan verified, or a PATH lookup.
//
// Using the plan's path matters, because the plan told the operator which version
// was found. Looking the name up again could silently run a different one.
func (e *Engine) binary() string {
	if e.opts.BinaryPath != "" {
		return e.opts.BinaryPath
	}
	return e.spec.Binary
}

func (e *Engine) port() string {
	if e.conn.Port != "" {
		return e.conn.Port
	}
	return "3306"
}

func (e *Engine) user() string {
	if e.conn.User != "" {
		return e.conn.User
	}
	return "root"
}

// Statements builds the exact argv that will be executed. It is pure.
func (e *Engine) Statements() ([]engine.Statement, error) {
	argv, cleanup, err := e.argv()
	if err != nil {
		return nil, err
	}
	e.defaultsFile = cleanup

	return []engine.Statement{{
		Description: fmt.Sprintf("%s copy of %s.%s", e.Name(), e.opts.Schema, e.opts.Table),
		Argv:        argv,
		SQL:         renderArgv(argv),
	}}, nil
}

func (e *Engine) argv() (argv []string, cleanupFile string, err error) {
	alter := strings.Join(opRaw(e.opts.Ops), ", ")
	if strings.TrimSpace(alter) == "" {
		alter = e.opts.Alter
	}

	switch e.spec.Kind {
	case KindGhOst:
		argv = []string{
			e.binary(),
			"--host=" + e.conn.Host,
			"--port=" + e.port(),
			"--user=" + e.user(),
			"--database=" + e.opts.Schema,
			"--table=" + e.opts.Table,
			"--alter=" + alter,
			fmt.Sprintf("--chunk-size=%d", chunkSize(e.opts)),
			"--max-load=Threads_running=50",
			"--critical-load=Threads_running=100",
			"--assume-rbr",
			"--initially-drop-ghost-table",
			"--initially-drop-old-table",
		}
		if e.opts.HoldCutover && e.opts.CutoverGate != "" {
			// gh-ost waits for this file to disappear before swapping. The flag is
			// only as reliable as the installed version; a bad flag makes gh-ost
			// exit immediately with an argument error rather than doing something
			// surprising, which is the behaviour we want.
			argv = append(argv, "--postpone-cut-over-flag-file="+e.opts.CutoverGate)
		}
		if !e.opts.HasReplica {
			argv = append(argv, "--allow-on-master")
		}
		if e.conn.ServerPort > 0 && e.port() != fmt.Sprint(e.conn.ServerPort) {
			// Connecting through a proxy or a mapped port is normal; gh-ost just
			// needs to be told that the mismatch is expected.
			argv = append(argv, "--skip-port-validation")
		}
		// gh-ost has no option-file support. A password would have to go in argv,
		// which puts it in the process list, so this is opt-in and loud.
		if e.conn.Password != "" {
			if !e.opts.AllowArgvPassword {
				return nil, "", fmt.Errorf(
					"gh-ost cannot read an option file, so its password would be visible in the process list; " +
						"pass --allow-argv-password to accept that, or use pt-osc which supports --defaults-file")
			}
			argv = append(argv, "--password="+e.conn.Password)
		}
		// An empty password means "connect without one"; --ask-pass would block
		// waiting on a terminal, which is the last thing a supervisor wants.
		argv = append(argv, "--verbose", "--execute")
		return argv, "", nil

	case KindPtOSC:
		// pt-osc reads an option file, so the password never reaches argv.
		if e.conn.Password != "" {
			path, err := writeDefaultsFile(e.conn)
			if err != nil {
				return nil, "", err
			}
			cleanupFile = path
			argv = append(argv, e.binary(), "--defaults-file="+path)
		} else {
			argv = append(argv, e.binary())
		}
		argv = append(argv,
			"--alter", alter,
			"--chunk-size", fmt.Sprint(chunkSize(e.opts)),
			"--max-load", "Threads_running=50",
			"--critical-load", "Threads_running=100",
			// pt-osc throttles on replica lag with --max-lag, in seconds.
			"--max-lag", "5",
			// Never let the tool reach out to check for a newer version: a CI run
			// must not depend on percona.com being reachable.
			"--no-version-check",
		)
		if len(e.opts.TableModel.ReferencedBy) > 0 {
			argv = append(argv, "--alter-foreign-keys-method=auto")
		}
		// --print only describes the work; --execute is what performs it.
		if e.opts.DryRun {
			argv = append(argv, "--print")
		} else {
			argv = append(argv, "--execute")
		}
		dsn := fmt.Sprintf("D=%s,t=%s,h=%s,P=%s,u=%s",
			e.opts.Schema, e.opts.Table, e.conn.Host, e.port(), e.user())
		argv = append(argv, dsn)
		return argv, cleanupFile, nil
	}

	return nil, "", fmt.Errorf("unknown copy engine %q", e.spec.Kind)
}

func (e *Engine) installHint() string {
	if e.spec.Kind == KindGhOst {
		return "install it from https://github.com/github/gh-ost/releases"
	}
	return "install it with: apt install percona-toolkit"
}

// CutoverGate implements engine.HoldsCutover. gh-ost waits until this file is
// removed; pt-osc has no equivalent and swaps as soon as the copy finishes.
func (e *Engine) CutoverGate() string {
	if e.spec.Kind != KindGhOst {
		return ""
	}
	return e.cutoverFile()
}

func (e *Engine) cutoverFile() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("saksama-%s-%s.cutover", e.opts.Schema, e.opts.Table))
}

func chunkSize(o engine.Options) int {
	if o.ChunkSize > 0 {
		return o.ChunkSize
	}
	return 1000
}

func writeDefaultsFile(c Conn) (string, error) {
	f, err := os.CreateTemp("", "saksama-my-*.cnf")
	if err != nil {
		return "", err
	}
	path := f.Name()
	content := fmt.Sprintf("[client]\nhost=%s\nport=%s\nuser=%s\npassword=%s\n",
		c.Host, c.Port, c.User, c.Password)
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	// The file exists only for the duration of one run.
	if err := os.Chmod(path, 0o600); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// Preflight rechecks the hazards that can appear between planning and running.
func (e *Engine) Preflight(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	// The engine binary must exist before anything else: failing here gives a
	// usable message instead of an exec error hours into a migration plan.
	if _, err := exec.LookPath(e.binary()); err != nil {
		return fmt.Errorf("%s is not installed at %s; %s", e.spec.Binary, e.binary(), e.installHint())
	}

	var pending int
	_ = e.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM performance_schema.metadata_locks WHERE LOCK_STATUS = 'PENDING'
	`).Scan(&pending)
	if pending > 0 && !e.opts.Force {
		return fmt.Errorf("%d metadata lock request(s) are pending; the engine would queue behind them", pending)
	}
	return nil
}

// Execute starts the engine and reports real progress from the database.
func (e *Engine) Execute(ctx context.Context, onProgress func(engine.Progress)) (engine.Execution, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ex := engine.Execution{Engine: e.Name(), StartedAt: e.now()}

	stmts, err := e.Statements()
	if err != nil {
		ex.Err = err
		ex.FinishedAt = e.now()
		return ex, err
	}
	if e.defaultsFile != "" {
		defer os.Remove(e.defaultsFile)
	}

	if e.opts.DryRun {
		for _, s := range stmts {
			ex.Statements = append(ex.Statements, engine.StatementResult{Statement: s})
		}
		ex.FinishedAt = e.now()
		return ex, nil
	}

	stop := make(chan struct{})
	if onProgress != nil {
		go e.pollProgress(ctx, onProgress, stop)
	}

	tail := &tailBuffer{}
	collect := func(l process.Line) {
		// Never keep the password line if one was passed in argv.
		text := l.Text
		if e.opts.AllowArgvPassword && e.conn.Password != "" {
			text = strings.ReplaceAll(text, e.conn.Password, "[REDACTED]")
		}
		tail.add(l.Stream, text)
		if e.lines != nil {
			e.lines(l)
		}
	}

	start := e.now()
	res, runErr := process.Run(ctx, process.Spec{
		Argv:   stmts[0].Argv,
		Detach: e.detach,
		OnStart: func(pid int) {
			if e.onProcessStart != nil {
				e.onProcessStart(pid)
			}
		},
	}, collect)
	close(stop)

	ex.Statements = append(ex.Statements, engine.StatementResult{
		Statement: stmts[0],
		Duration:  res.Finished.Sub(start),
		Err:       runErr,
	})
	ex.FinishedAt = e.now()
	if runErr != nil {
		// Quote the engine's own last words: that is the diagnostic the operator
		// needs, and it is not recoverable from an exit code.
		ex.Err = fmt.Errorf("%s failed (exit %d): %w\nlast output:\n%s",
			e.Name(), res.ExitCode, runErr, tail.String())
		return ex, ex.Err
	}
	if onProgress != nil {
		onProgress(engine.Progress{Phase: "copy", Percent: 100, Note: "engine finished"})
	}
	return ex, nil
}

// pollProgress reads the copy table from information_schema. Row counts there
// are estimates on InnoDB, which is fine for a progress bar and far more stable
// than parsing engine output.
func (e *Engine) pollProgress(ctx context.Context, onProgress func(engine.Progress), stop <-chan struct{}) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	source := e.opts.TableModel.Rows
	if source <= 0 {
		source = e.tableRows(ctx, e.opts.Table)
	}

	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		copyRows := int64(0)
		for _, name := range e.copyTableNames() {
			if n := e.tableRows(ctx, name); n > 0 {
				copyRows = n
				break
			}
		}

		p := engine.Progress{Phase: "copy", RowsCopied: copyRows, RowsTotal: source}
		if source > 0 {
			p.Percent = float64(copyRows) / float64(source) * 100
			if p.Percent > 99 {
				p.Percent = 99
			}
		}
		var threads int
		_ = e.db.QueryRowContext(ctx, `SHOW GLOBAL STATUS LIKE 'Threads_running'`).Scan(new(string), new(string))
		_ = threads
		onProgress(p)
	}
}

func (e *Engine) copyTableNames() []string {
	if e.spec.Kind == KindGhOst {
		return []string{"_" + e.opts.Table + "_gho"}
	}
	return []string{"_" + e.opts.Table + "_new"}
}

func (e *Engine) tableRows(ctx context.Context, table string) int64 {
	var n int64
	err := e.db.QueryRowContext(ctx, `
		SELECT TABLE_ROWS FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, e.opts.Schema, table).Scan(&n)
	if err != nil {
		return 0
	}
	return n
}

// SetDetach implements engine.Detachable.
func (e *Engine) SetDetach(detach bool, onStart func(pid int)) {
	e.detach = detach
	e.onProcessStart = onStart
}

// Verify checks the result through information_schema.
func (e *Engine) Verify(ctx context.Context) ([]engine.VerifyResult, error) {
	return engine.VerifyAgainstSchema(ctx, e.db, e.opts.Schema, e.opts.Table, e.opts.Plan.Verification)
}

// GhOst and PtOSC return the two configured specs.
func GhOst() Spec { return Spec{Kind: KindGhOst, Binary: "gh-ost", ShadowSuffix: "_gho"} }

func PtOSC() Spec {
	return Spec{Kind: KindPtOSC, Binary: "pt-online-schema-change", ShadowSuffix: "_new"}
}

func renderArgv(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(argv[0])
	for _, a := range argv[1:] {
		if strings.HasPrefix(a, "-") {
			b.WriteString(" \\\n  " + a)
		} else {
			b.WriteString(" " + a)
		}
	}
	return b.String()
}

func opRaw(ops []model.Op) []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, strings.TrimSpace(op.Raw))
	}
	return out
}

var _ = analyzer.EngineNone
