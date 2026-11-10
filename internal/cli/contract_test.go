package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/state"
)

// The exit codes are the interface a script sees, and until now they were pinned
// one command at a time, in whatever test happened to need them. That is how
// `cutover` came to answer a precondition with 3 while every other command answered
// 2, and how `cleanup` came to accept a misspelled --dry-run and go on to delete.
//
// This table is the contract in one place: every command, every failure it can
// answer, and the code a script can rely on. It is also asserted to be complete -
// a command added to Run without an entry here fails the test.
//
// 0  success
// 1  the plan exists and has blockers: the CI gate
// 2  the operator can fix it: usage, a missing session, a precondition
// 3  our failure: a connection broke, the state directory is unreadable

type contractCase struct {
	name string
	args []string
	// wantExit is the code the command must return.
	wantExit int
	// wantStderr is a substring the operator needs to see, so a refusal cannot be
	// correct in code and silent in words.
	wantStderr string
	// wantStdout is a substring of the success output.
	wantStdout string
	// setup seeds the state directory this case runs against.
	setup func(t *testing.T, dir string)
}

// contractSeed writes one finished session, which is enough for the commands that
// read a session without connecting to anything.
func contractSeed(t *testing.T, dir string) {
	t.Helper()
	store, err := state.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&state.Session{
		ID: "s1", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:01:00Z",
		State: state.StateDone, Database: "shop", Table: "orders",
		Engine: "native", Alter: "ADD INDEX ix (id)", Host: "127.0.0.1",
	}); err != nil {
		t.Fatal(err)
	}
}

func contractCases(t *testing.T, ddl string) []contractCase {
	t.Helper()
	return []contractCase{
		// --- plan ------------------------------------------------------------
		{name: "plan, nothing to plan from", args: []string{"plan"},
			wantExit: ExitUsage, wantStderr: "provide --ddl"},
		{name: "plan, ddl file does not exist", args: []string{"plan", "--ddl", "/nonexistent/schema.sql", "--alter", "ADD INDEX a (b)", "--mysql-version", "8.0.36"},
			wantExit: ExitUsage, wantStderr: "no such file"},
		{name: "plan, stray positional", args: []string{"plan", "orders", "--ddl", ddl},
			wantExit: ExitUsage, wantStderr: "unexpected argument \"orders\""},
		{name: "plan, unknown flag", args: []string{"plan", "--ddl", ddl, "--bogus"},
			wantExit: ExitUsage, wantStderr: "not defined"},
		{name: "plan, help", args: []string{"plan", "--help"},
			wantExit: ExitOK, wantStderr: "Usage of plan"},
		{name: "plan, ready", args: []string{"plan", "--ddl", ddl, "--alter", "ADD INDEX ix_created (created_at)", "--mysql-version", "8.0.36"},
			wantExit: ExitOK, wantStdout: "RESULT      ready"},
		{name: "plan, blocked exits 1 for a CI gate", args: []string{"plan", "--ddl", writeTemp(t, "nokey.sql", contractNoKeyDDL()), "--alter", "MODIFY COLUMN status varchar(64) NOT NULL", "--mysql-version", "8.0.36"},
			wantExit: ExitBlocked, wantStdout: "none (blocked)"},

		// --- plan --live ------------------------------------------------------
		{name: "plan --live, no target", args: []string{"plan", "--live"},
			wantExit: ExitUsage, wantStderr: "--database and --table are required"},
		{name: "plan --live, no alter", args: []string{"plan", "--live", "-d", "shop", "-t", "orders"},
			wantExit: ExitUsage, wantStderr: "--alter is required"},

		// --- advise ----------------------------------------------------------
		{name: "advise, no source", args: []string{"advise"},
			wantExit: ExitUsage, wantStderr: "give --ddl/--schema, or use --live"},
		{name: "advise, stray positional", args: []string{"advise", "orders", "--ddl", ddl},
			wantExit: ExitUsage, wantStderr: "unexpected argument"},

		// --- apply -----------------------------------------------------------
		{name: "apply, nothing named", args: []string{"apply"},
			wantExit: ExitUsage, wantStderr: "--database, --table, and --alter are required"},
		{name: "apply, session passed positionally", args: []string{"apply", "s1"},
			wantExit: ExitUsage, wantStderr: "unexpected argument \"s1\""},
		{name: "apply, session does not exist", args: []string{"apply", "--session", "s9"},
			wantExit: ExitUsage, wantStderr: "session not found"},

		// --- status ----------------------------------------------------------
		{name: "status, empty", args: []string{"status"},
			wantExit: ExitOK, wantStdout: "no sessions recorded"},
		{name: "status, one session", args: []string{"status", "s1"}, setup: contractSeed,
			wantExit: ExitOK, wantStdout: "shop.orders"},
		{name: "status, missing session", args: []string{"status", "s9"},
			wantExit: ExitUsage, wantStderr: "session not found"},
		{name: "status, unknown flag after the id", args: []string{"status", "s1", "--bogus"},
			wantExit: ExitUsage, wantStderr: "not defined"},

		// --- events / export / attach / cleanup -------------------------------
		{name: "events, no id", args: []string{"events"},
			wantExit: ExitUsage, wantStderr: "session id required"},
		{name: "events, missing session", args: []string{"events", "s9"},
			wantExit: ExitUsage, wantStderr: "session not found"},
		{name: "export, missing session", args: []string{"export", "s9"},
			wantExit: ExitUsage, wantStderr: "session not found"},
		{name: "attach, missing session", args: []string{"attach", "s9"},
			wantExit: ExitUsage, wantStderr: "no event log"},
		{name: "cleanup, no id", args: []string{"cleanup"},
			wantExit: ExitUsage, wantStderr: "session id required"},
		{name: "cleanup, missing session", args: []string{"cleanup", "s9"},
			wantExit: ExitUsage, wantStderr: "session not found"},
		{name: "cleanup, misspelled dry run", args: []string{"cleanup", "s1", "--dryrun"}, setup: contractSeed,
			wantExit: ExitUsage, wantStderr: "not defined"},

		// --- cancel / cutover --------------------------------------------------
		{name: "cancel, missing session", args: []string{"cancel", "s9"},
			wantExit: ExitUsage, wantStderr: "session not found"},
		{name: "cutover, missing session", args: []string{"cutover", "s9"},
			wantExit: ExitUsage, wantStderr: "session not found"},
		{name: "cutover, no gate to release", args: []string{"cutover", "s1"}, setup: contractSeed,
			wantExit: ExitUsage, wantStderr: "no held cutover"},

		// --- reconcile --------------------------------------------------------
		{name: "reconcile, stray positional", args: []string{"reconcile", "everything"},
			wantExit: ExitUsage, wantStderr: "unexpected argument"},
		{name: "reconcile, retire without an id", args: []string{"reconcile", "--retire"},
			wantExit: ExitUsage, wantStderr: "flag needs an argument"},
		// Retiring a session that already ended is a documented no-op, not an
		// error: the operator asked for a state, and the state already holds.
		{name: "reconcile, retire a session that already ended", args: []string{"reconcile", "--retire", "s1"}, setup: contractSeed,
			wantExit: ExitOK, wantStdout: "nothing to retire"},
		{name: "reconcile, retire an unknown session", args: []string{"reconcile", "--retire", "s9"},
			wantExit: ExitUsage, wantStderr: "session not found"},

		// --- engine -----------------------------------------------------------
		{name: "engine, no subcommand", args: []string{"engine"},
			wantExit: ExitUsage, wantStderr: "saksama engine"},
		{name: "engine, unknown subcommand", args: []string{"engine", "bogus"},
			wantExit: ExitUsage, wantStderr: "unknown subcommand"},
		// No target means the global choice, which is a legitimate thing to read.
		{name: "engine show, global choice", args: []string{"engine", "show"},
			wantExit: ExitOK, wantStdout: "global"},
		{name: "engine show, unknown flag", args: []string{"engine", "show", "--bogus"},
			wantExit: ExitUsage, wantStderr: "not defined"},
		{name: "engine list, stray positional", args: []string{"engine", "list", "gh-ost"},
			wantExit: ExitUsage, wantStderr: "unexpected argument"},

		// --- read-only output --------------------------------------------------
		{name: "compat", args: []string{"compat"}, wantExit: ExitOK, wantStdout: "gh-ost"},
		{name: "compat, stray positional", args: []string{"compat", "table"},
			wantExit: ExitUsage, wantStderr: "unexpected argument"},
		{name: "schema", args: []string{"schema"}, wantExit: ExitOK, wantStdout: "$schema"},
		{name: "schema, stray positional", args: []string{"schema", "orders"},
			wantExit: ExitUsage, wantStderr: "unexpected argument"},
		{name: "version", args: []string{"version"}, wantExit: ExitOK, wantStdout: "contract v"},
		{name: "version, extra argument", args: []string{"version", "--json"},
			wantExit: ExitUsage, wantStderr: "unexpected argument"},
		{name: "help", args: []string{"help"}, wantExit: ExitOK, wantStdout: "Usage:"},
		{name: "help, long flag", args: []string{"--help"}, wantExit: ExitOK, wantStdout: "Usage:"},
		{name: "help, single dash", args: []string{"-help"}, wantExit: ExitOK, wantStdout: "Usage:"},
		{name: "version, long flag", args: []string{"--version"}, wantExit: ExitOK, wantStdout: "contract v"},
		{name: "version, short flag", args: []string{"-V"}, wantExit: ExitOK, wantStdout: "contract v"},
		{name: "bare invocation in a pipe", args: nil,
			wantExit: ExitUsage, wantStderr: "Usage"},

		// --- not built, and not a command -------------------------------------
		{name: "daemon is not in this build", args: []string{"daemon"},
			wantExit: ExitUsage, wantStderr: "not implemented in this build"},
		{name: "unknown command", args: []string{"frobnicate"},
			wantExit: ExitUsage, wantStderr: "unknown command"},
		{name: "recover is not in this build", args: []string{"recover"},
			wantExit: ExitUsage, wantStderr: "not implemented in this build"},

		// --- the wizard needs a terminal ---------------------------------------
		{name: "wizard without a terminal", args: []string{"wizard"},
			wantExit: ExitUsage, wantStderr: "interactive terminal"},
		{name: "interactive alias without a terminal", args: []string{"interactive"},
			wantExit: ExitUsage, wantStderr: "interactive terminal"},
	}
}

// TestExitCodeContract is the table itself.
func TestExitCodeContract(t *testing.T) {
	ddl := writeTemp(t, "orders.sql", testDDL)
	for _, c := range contractCases(t, ddl) {
		t.Run(c.name, func(t *testing.T) {
			// Every case gets its own state directory: a seeded session must never
			// leak into the next case, or a passing test proves nothing about the
			// one after it.
			dir := t.TempDir()
			t.Setenv("SAKSAMA_STATE_DIR", dir)
			if c.setup != nil {
				c.setup(t, dir)
			}

			code, out, errOut := run(t, c.args...)
			if code != c.wantExit {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, c.wantExit, out, errOut)
			}
			if c.wantStderr != "" && !strings.Contains(errOut+out, c.wantStderr) {
				t.Fatalf("the output does not say %q\nstdout: %s\nstderr: %s", c.wantStderr, out, errOut)
			}
			if c.wantStdout != "" && !strings.Contains(out, c.wantStdout) {
				t.Fatalf("stdout does not contain %q\nstdout: %s\nstderr: %s", c.wantStdout, out, errOut)
			}
		})
	}
}

// TestEveryCommandHasContractCases is the anti-drift half: a command wired into Run
// without a case here would otherwise be a command whose exit codes nobody checked.
// It reads the dispatch, the same way the analyzer reads its own catalogue.
func TestEveryCommandHasContractCases(t *testing.T) {
	ddl := writeTemp(t, "orders.sql", testDDL)

	covered := map[string]bool{}
	for _, c := range contractCases(t, ddl) {
		if len(c.args) == 0 {
			continue
		}
		name := c.args[0]
		covered[name] = true
		// `plan --live` is a second surface behind the same word.
		if name == "plan" && len(c.args) > 1 && c.args[1] == "--live" {
			covered["plan --live"] = true
		}
	}

	dispatched := dispatchedCommands(t)
	var missing []string
	for _, name := range dispatched {
		key := name
		if name == "--live" {
			continue
		}
		if !covered[key] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("commands with no contract case: %s\nadd a row to contractCases: every command answers with a documented exit code", strings.Join(missing, ", "))
	}
	if !covered["plan --live"] {
		t.Fatal("plan --live has no contract case")
	}
}

// TestNoCommandSilentlyIgnoresAnArgument walks every command that takes no
// positional argument, with a stray word at the end. The standard parser stops at
// the first non-flag token, so anything after it - a flag that turns a guard off,
// most dangerously - was previously discarded in silence.
//
// Every dispatched command has to be classified here. A new command that is neither
// a pure-flag command, nor takes a positional subject, nor owns a subcommand
// namespace fails the test rather than escaping it.
func TestNoCommandSilentlyIgnoresAnArgument(t *testing.T) {
	// The argument is the subject the command acts on.
	takesPositional := map[string]bool{
		"status": true, "events": true, "export": true, "attach": true,
		"cleanup": true, "cancel": true, "cutover": true,
	}
	// The first argument is a noun from the command's own namespace.
	takesSubcommand := map[string]bool{"engine": true}
	// `help` exists to list what a reader may have meant, so it never rejects a
	// word it does not recognise. The wizard parses its own arguments.
	neverRefuses := map[string]bool{
		"help": true, "--help": true, "-help": true,
		"wizard": true, "interactive": true,
	}
	// Commands that are not in this build refuse before they read anything, so the
	// word after them is irrelevant - they are already saying the useful thing.
	notImplemented := map[string]bool{"daemon": true, "recover": true}
	// plan --live is a second surface behind `plan`, probed through plan itself.
	skip := map[string]bool{"--live": true}

	var probed int
	for _, name := range dispatchedCommands(t) {
		if skip[name] {
			continue
		}
		if takesPositional[name] || takesSubcommand[name] || neverRefuses[name] {
			continue
		}
		probed++
		code, out, errOut := run(t, name, "STRAGGLER")
		if code != ExitUsage {
			t.Errorf("`%s STRAGGLER` exited %d, want %d (nothing typed may be ignored)\nstdout: %s\nstderr: %s",
				name, code, ExitUsage, out, errOut)
			continue
		}
		if notImplemented[name] {
			continue
		}
		if !strings.Contains(errOut, "unexpected argument") {
			t.Errorf("`%s STRAGGLER` refused without naming the argument: %q", name, errOut)
			continue
		}
		// The other half of the rule: the refusal has to come before the command
		// does any work, so the probe above uses no flags at all.
	}
	if probed < 8 {
		t.Fatalf("only %d commands were probed; the classification above has drifted", probed)
	}
}

// TestHelpIsNotAFailure pins the other half of the same contract: asking a command
// what it does is not a usage error, and it must stop the command rather than print
// the flags and then complain that they were missing.
func TestHelpIsNotAFailure(t *testing.T) {
	for _, name := range []string{"plan", "advise", "apply", "status", "events", "export", "cleanup", "compat", "schema", "cancel", "cutover", "reconcile"} {
		code, out, errOut := run(t, name, "--help")
		if code != ExitOK {
			t.Errorf("`%s --help` exited %d, want %d\nstdout: %s\nstderr: %s", name, code, ExitOK, out, errOut)
		}
		if strings.Contains(out, "is required") || strings.Contains(errOut, "is required") {
			t.Errorf("`%s --help` went on to validate its arguments\nstdout: %s\nstderr: %s", name, out, errOut)
		}
	}
}

// dispatchedCommands reads the command names out of Run's switch, so a new command
// cannot quietly escape the two tests above.
func dispatchedCommands(t *testing.T) []string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cli.go", nil, 0)
	if err != nil {
		t.Fatalf("parse cli.go: %v", err)
	}

	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		// The dispatch is `switch args[0]`; any other switch in the package is not
		// a command list.
		idx, ok := sw.Tag.(*ast.IndexExpr)
		if !ok {
			return true
		}
		ident, ok := idx.X.(*ast.Ident)
		if !ok || ident.Name != "args" {
			return true
		}
		for _, stmt := range sw.Body.List {
			clause, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			if len(clause.List) == 0 {
				out = append(out, "help") // bare `saksama`
				continue
			}
			for _, expr := range clause.List {
				if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					out = append(out, strings.Trim(lit.Value, `"`))
				}
			}
		}
		return true
	})

	if len(out) == 0 {
		t.Fatal("no commands were found in the dispatch switch")
	}
	return out
}

// contractNoKeyDDL is a table without a primary key: the shape every copy engine
// refuses, which is how a plan reaches RESULT blocked without a live server.
func contractNoKeyDDL() string {
	return `CREATE TABLE ` + "`audit`" + ` (
  ` + "`id`" + ` int NOT NULL,
  ` + "`status`" + ` varchar(32) NOT NULL DEFAULT 'pending'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`
}
