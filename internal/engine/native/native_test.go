package native

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/engine"
	"github.com/perogeremmer/saksama-core/internal/model"
)

func planWith(ops []analyzer.OpPlan) analyzer.Plan {
	return analyzer.Plan{Operations: ops}
}

func TestStatementsCarryExplicitAlgorithmAndLock(t *testing.T) {
	op := model.Op{Kind: model.OpAddIndex, Raw: "ADD INDEX ix_created (created_at)"}
	e := New(nil, engine.Options{
		Schema: "shop", Table: "orders",
		Ops: []model.Op{op},
		Plan: planWith([]analyzer.OpPlan{{
			Kind: model.OpAddIndex, Raw: op.Raw, Method: analyzer.MethodInplace, Lock: "NONE",
		}}),
	})

	stmts, err := e.Statements()
	if err != nil {
		t.Fatalf("statements: %v", err)
	}
	if len(stmts) != 1 {
		t.Fatalf("got %d statements", len(stmts))
	}
	sql := stmts[0].SQL
	if !strings.Contains(sql, "ALTER TABLE `shop`.`orders`") {
		t.Fatalf("bad target: %s", sql)
	}
	if !strings.Contains(sql, "ALGORITHM=INPLACE, LOCK=NONE") {
		t.Fatalf("missing explicit algorithm/lock: %s", sql)
	}
}

// MySQL rejects ALGORITHM=INSTANT together with a LOCK clause (error 1221), so
// the engine must never emit one.
func TestInstantStatementsOmitLockClause(t *testing.T) {
	op := model.Op{Kind: model.OpAddColumn, Raw: "ADD COLUMN note varchar(64)"}
	e := New(nil, engine.Options{
		Schema: "shop", Table: "orders",
		Ops: []model.Op{op},
		Plan: planWith([]analyzer.OpPlan{{
			Kind: model.OpAddColumn, Raw: op.Raw, Method: analyzer.MethodInstant,
		}}),
	})

	stmts, err := e.Statements()
	if err != nil {
		t.Fatalf("statements: %v", err)
	}
	sql := stmts[0].SQL
	if !strings.Contains(sql, "ALGORITHM=INSTANT") {
		t.Fatalf("missing INSTANT: %s", sql)
	}
	if strings.Contains(sql, "LOCK=") {
		t.Fatalf("INSTANT must not carry a LOCK clause: %s", sql)
	}
}

func TestStatementsGroupByAlgorithm(t *testing.T) {
	ops := []model.Op{
		{Kind: model.OpAddColumn, Raw: "ADD COLUMN a int NOT NULL DEFAULT 0"},
		{Kind: model.OpAddIndex, Raw: "ADD INDEX ix_a (a)"},
		{Kind: model.OpAddColumn, Raw: "ADD COLUMN b int NOT NULL DEFAULT 0"},
	}
	e := New(nil, engine.Options{
		Schema: "shop", Table: "t", Ops: ops,
		Plan: planWith([]analyzer.OpPlan{
			{Kind: model.OpAddColumn, Raw: ops[0].Raw, Method: analyzer.MethodInstant},
			{Kind: model.OpAddIndex, Raw: ops[1].Raw, Method: analyzer.MethodInplace, Lock: "NONE"},
			{Kind: model.OpAddColumn, Raw: ops[2].Raw, Method: analyzer.MethodInstant},
		}),
	})

	stmts, err := e.Statements()
	if err != nil {
		t.Fatalf("statements: %v", err)
	}
	if len(stmts) != 2 {
		t.Fatalf("got %d statements, want 2 (one INSTANT group, one INPLACE group): %+v", len(stmts), stmts)
	}
	// Both INSTANT operations must be batched together.
	if stmts[0].Algorithm != "INSTANT" || len(stmts[0].Ops) != 2 {
		t.Fatalf("first group = %+v", stmts[0])
	}
	if stmts[1].Algorithm != "INPLACE" || len(stmts[1].Ops) != 1 {
		t.Fatalf("second group = %+v", stmts[1])
	}
}

func TestStatementsRefuseNonNativeOperation(t *testing.T) {
	op := model.Op{Kind: model.OpModifyColumn, Raw: "MODIFY COLUMN status varchar(64)"}
	e := New(nil, engine.Options{
		Schema: "shop", Table: "t", Ops: []model.Op{op},
		Plan: planWith([]analyzer.OpPlan{{
			Kind: model.OpModifyColumn, Raw: op.Raw, Method: analyzer.MethodCopy, RuleID: "R-MODIFY-COLUMN-COPY",
		}}),
	})

	_, err := e.Statements()
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "not executable natively") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestStatementsRefuseOpWithoutVerdict(t *testing.T) {
	e := New(nil, engine.Options{
		Schema: "shop", Table: "t",
		Ops:  []model.Op{{Kind: model.OpAddIndex, Raw: "ADD INDEX ix (a)"}},
		Plan: planWith(nil),
	})
	_, err := e.Statements()
	if err == nil {
		t.Fatal("expected a refusal when the analyzer gave no verdict")
	}
}

func TestTargetWithoutSchemaIsUnqualified(t *testing.T) {
	op := model.Op{Kind: model.OpDropIndex, Raw: "DROP INDEX ix"}
	e := New(nil, engine.Options{
		Table: "t", Ops: []model.Op{op},
		Plan: planWith([]analyzer.OpPlan{{
			Kind: model.OpDropIndex, Raw: op.Raw, Method: analyzer.MethodInplace, Lock: "NONE",
		}}),
	})
	stmts, err := e.Statements()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stmts[0].SQL, "ALTER TABLE `t`") {
		t.Fatalf("unexpected target: %s", stmts[0].SQL)
	}
}

func TestQuoteIdentEscapesBackticks(t *testing.T) {
	if got := quoteIdent("we`ird"); got != "`we``ird`" {
		t.Fatalf("quoteIdent = %s", got)
	}
}

func TestDryRunProducesStatementsWithoutExecuting(t *testing.T) {
	op := model.Op{Kind: model.OpAddIndex, Raw: "ADD INDEX ix (a)"}
	e := New(nil, engine.Options{
		Schema: "shop", Table: "t", Ops: []model.Op{op},
		Plan:   planWith([]analyzer.OpPlan{{Kind: model.OpAddIndex, Raw: op.Raw, Method: analyzer.MethodInplace, Lock: "NONE"}}),
		DryRun: true,
	})
	exec, err := e.Execute(nil, nil)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(exec.Statements) != 1 {
		t.Fatalf("got %d statements", len(exec.Statements))
	}
	if exec.Err != nil {
		t.Fatalf("unexpected error: %v", exec.Err)
	}
}

// A stub driver that hands out a fresh session for every Open and records what
// ran on each one, so a test can see which connection served which query.
type recordingDriver struct {
	mu    sync.Mutex
	conns []*recordingConn
}

type recordingConn struct {
	mu      sync.Mutex
	queries []string
	closed  bool
}

func (d *recordingDriver) Open(name string) (driver.Conn, error) {
	c := &recordingConn{}
	d.mu.Lock()
	d.conns = append(d.conns, c)
	d.mu.Unlock()
	return c, nil
}

func (c *recordingConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not part of this stub")
}

func (c *recordingConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *recordingConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not part of this stub")
}

func (c *recordingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.mu.Lock()
	c.queries = append(c.queries, query)
	c.mu.Unlock()
	return driver.RowsAffected(0), nil
}

func (c *recordingConn) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.queries...)
}

// The guard and the DDL it protects must share a session. A SET SESSION through
// a pool lands on whichever connection serves it; the ALTER may draw another,
// and then the short lock_wait_timeout never protected anything.
func TestGuardsAndDDLShareOneSession(t *testing.T) {
	d := &recordingDriver{}
	const name = "saksama-native-pintest"
	sql.Register(name, d)
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The pool discards idle connections, so two Execs through the pool would
	// land on two different sessions - exactly the bug pinning removes.
	db.SetMaxIdleConns(0)

	op := model.Op{Kind: model.OpAddIndex, Raw: "ADD INDEX ix_a (a)"}
	e := New(db, engine.Options{
		Schema: "shop", Table: "orders", Ops: []model.Op{op},
		Plan: planWith([]analyzer.OpPlan{{
			Kind: model.OpAddIndex, Raw: op.Raw, Method: analyzer.MethodInplace, Lock: "NONE",
		}}),
		LockWaitTimeoutSeconds: 3,
	})

	if _, err := e.Execute(context.Background(), nil); err != nil {
		t.Fatalf("execute: %v", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.conns) != 1 {
		t.Fatalf("guard and DDL must share one session; the driver opened %d", len(d.conns))
	}
	queries := d.conns[0].snapshot()
	if len(queries) != 2 {
		t.Fatalf("expected the guard and one ALTER on that session, got %d: %v", len(queries), queries)
	}
	if !strings.Contains(queries[0], "SET SESSION lock_wait_timeout = 3") {
		t.Fatalf("the guard must run before the DDL on the same session: %v", queries)
	}
	if !strings.Contains(queries[1], "ALTER TABLE") {
		t.Fatalf("the DDL must run on the guarded session: %v", queries)
	}
}
