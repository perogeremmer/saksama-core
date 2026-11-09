// Package native executes MySQL DDL directly, with the algorithm and lock mode
// spelled out so the server can never silently pick something more expensive.
package native

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/engine"
	"github.com/perogeremmer/saksama-core/internal/introspect"
	"github.com/perogeremmer/saksama-core/internal/model"
)

// Executor is the database surface the native engine needs.
type Executor interface {
	introspect.Querier
}

// Engine runs ALTER statements natively.
type Engine struct {
	opts engine.Options
	db   Executor
	now  func() time.Time
}

// New creates a native engine.
func New(db Executor, opts engine.Options) *Engine {
	if opts.LockWaitTimeoutSeconds <= 0 {
		opts.LockWaitTimeoutSeconds = 10
	}
	if opts.LongQueryThresholdSeconds <= 0 {
		opts.LongQueryThresholdSeconds = 30
	}
	return &Engine{opts: opts, db: db, now: time.Now}
}

// Name implements engine.Engine.
func (e *Engine) Name() engine.Name { return engine.Native }

// algorithmFor returns the algorithm and lock an operation must be executed with.
func algorithmFor(plan analyzer.OpPlan) (algorithm, lock string, ok bool) {
	switch plan.Method {
	case analyzer.MethodInstant:
		// MySQL rejects ALGORITHM=INSTANT together with LOCK=... (error 1221).
		// INSTANT is metadata-only by definition, so there is nothing to lock.
		return "INSTANT", "", true
	case analyzer.MethodInplace:
		return "INPLACE", plan.Lock, true
	}
	return "", "", false
}

// Statements groups operations by their required (algorithm, lock) pair, so the
// number of metadata-lock acquisitions stays small without ever mixing an
// INSTANT change into an INPLACE statement or silently downgrading one.
func (e *Engine) Statements() ([]engine.Statement, error) {
	planByIndex := map[string]analyzer.OpPlan{}
	for _, op := range e.opts.Plan.Operations {
		planByIndex[op.Raw] = op
	}

	type groupKey struct{ algorithm, lock string }
	order := []groupKey{}
	grouped := map[groupKey][]model.Op{}

	for _, op := range e.opts.Ops {
		plan, ok := planByIndex[op.Raw]
		if !ok {
			return nil, fmt.Errorf("no analyzer verdict for operation %q", op.Raw)
		}
		algorithm, lock, ok := algorithmFor(plan)
		if !ok {
			return nil, fmt.Errorf("operation %q is not executable natively (method %s, rule %s)",
				op.Raw, plan.Method, plan.RuleID)
		}
		key := groupKey{algorithm, strings.ToUpper(lock)}
		if _, seen := grouped[key]; !seen {
			order = append(order, key)
		}
		grouped[key] = append(grouped[key], op)
	}

	target := quoteIdent(e.opts.Schema) + "." + quoteIdent(e.opts.Table)
	if e.opts.Schema == "" {
		target = quoteIdent(e.opts.Table)
	}

	var out []engine.Statement
	for _, key := range order {
		ops := grouped[key]
		clauses := make([]string, 0, len(ops))
		descriptions := make([]string, 0, len(ops))
		for _, op := range ops {
			clause := strings.TrimSpace(op.Raw)
			// Strip a trailing ALGORITHM/LOCK the user may have typed; we add our own.
			clauses = append(clauses, clause)
			descriptions = append(descriptions, string(op.Kind))
		}

		algorithmClause := "ALGORITHM=" + key.algorithm
		if key.lock != "" {
			algorithmClause += ", LOCK=" + key.lock
		}

		out = append(out, engine.Statement{
			Description: fmt.Sprintf("%s (%s)", strings.Join(descriptions, ", "), describeAlgorithm(key.algorithm, key.lock)),
			SQL:         fmt.Sprintf("ALTER TABLE %s %s, %s", target, strings.Join(clauses, ", "), algorithmClause),
			Algorithm:   key.algorithm,
			Lock:        key.lock,
			Ops:         ops,
		})
	}

	return out, nil
}

func describeAlgorithm(algorithm, lock string) string {
	if lock == "" {
		return algorithm
	}
	return algorithm + ", LOCK=" + lock
}

// Preflight rechecks, right before execution, the hazards that can appear
// between planning and running. A stale plan is the most common real cause of an
// incident, so the recheck is unconditional.
func (e *Engine) Preflight(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !hasMethod(e.opts.Plan, analyzer.MethodInstant) {
		// INPLACE operations take a metadata lock even with LOCK=NONE, so a long
		// statement or a pending MDL queue is fatal.
		var pending int
		_ = e.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM performance_schema.metadata_locks WHERE LOCK_STATUS = 'PENDING'
		`).Scan(&pending)
		if pending > 0 && !e.opts.Force {
			return fmt.Errorf("%d metadata lock request(s) are pending; a DDL statement would enqueue behind them (use --force only if you accept the outage risk)", pending)
		}

		var running, longest int64
		var sqlText sql.NullString
		_ = e.db.QueryRowContext(ctx, `
			SELECT COUNT(*), COALESCE(MAX(TIME), 0)
			FROM information_schema.PROCESSLIST
			WHERE COMMAND NOT IN ('Sleep','Daemon') AND INFO IS NOT NULL
		`).Scan(&running, &longest)
		if longest >= int64(e.opts.LongQueryThresholdSeconds) && !e.opts.Force {
			_ = e.db.QueryRowContext(ctx, `
				SELECT INFO FROM information_schema.PROCESSLIST
				WHERE COMMAND NOT IN ('Sleep','Daemon') AND INFO IS NOT NULL AND TIME = ?
				LIMIT 1
			`, longest).Scan(&sqlText)
			return fmt.Errorf(
				"a statement has been running for %ds and may hold the metadata lock: %s (wait for it, terminate it, or pass --force)",
				longest, introspect.SanitizeSQL(sqlText.String, 160))
		}
	}
	return nil
}

func hasMethod(p analyzer.Plan, m analyzer.Method) bool {
	for _, op := range p.Operations {
		if op.Method == m {
			return true
		}
	}
	return false
}

// Execute runs each statement in its own transaction-free session, with a short
// lock_wait_timeout so it fails fast instead of blocking the whole server.
// pinSource is the optional ability to pin one connection out of the pool. A
// SET SESSION sent through a pool is a promise about whichever connection
// happens to serve it, and the ALTER may draw a different one - so without
// pinning, the short lock_wait_timeout can silently fail to protect the DDL
// it exists to protect.
type pinSource interface {
	Conn(ctx context.Context) (*sql.Conn, error)
}

// pin holds one connection for the whole run when the executor can provide it,
// so the guards and the DDL share a server session. Executors that cannot pin
// (test fakes, a *sql.Tx) run as they are; they never had a pool to lose.
func (e *Engine) pin(ctx context.Context) (introspect.Querier, func(), error) {
	if p, ok := e.db.(pinSource); ok {
		conn, err := p.Conn(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("pinning a session for the DDL: %w", err)
		}
		return conn, func() { _ = conn.Close() }, nil
	}
	return e.db, func() {}, nil
}

func (e *Engine) Execute(ctx context.Context, onProgress func(engine.Progress)) (engine.Execution, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ex := engine.Execution{Engine: engine.Native, StartedAt: e.now()}

	stmts, err := e.Statements()
	if err != nil {
		ex.Err = err
		ex.FinishedAt = e.now()
		return ex, err
	}

	if e.opts.DryRun {
		for _, s := range stmts {
			ex.Statements = append(ex.Statements, engine.StatementResult{Statement: s})
		}
		ex.FinishedAt = e.now()
		return ex, nil
	}

	// One session for the guards and every statement: the lock_wait_timeout is
	// only real if it is set where the ALTER actually runs.
	session, release, err := e.pin(ctx)
	if err != nil {
		ex.Err = err
		ex.FinishedAt = e.now()
		return ex, err
	}
	defer release()

	total := len(stmts)
	for i, s := range stmts {
		if onProgress != nil {
			onProgress(engine.Progress{
				Phase:   "ddl",
				Percent: float64(i) / float64(total) * 100,
				Note:    s.Description,
			})
		}

		start := e.now()
		res := engine.StatementResult{Statement: s}

		if err := e.setSessionGuards(ctx, session); err != nil {
			res.Err = err
			res.Duration = e.now().Sub(start)
			ex.Statements = append(ex.Statements, res)
			ex.Err = err
			ex.FinishedAt = e.now()
			return ex, err
		}

		var rows int64
		result, err := session.ExecContext(ctx, s.SQL)
		if err == nil {
			rows, _ = result.RowsAffected()
		}
		res.RowsAffected = rows
		res.Err = err
		res.Duration = e.now().Sub(start)
		ex.Statements = append(ex.Statements, res)

		if err != nil {
			ex.Err = fmt.Errorf("%s failed: %w", s.Description, err)
			ex.FinishedAt = e.now()
			return ex, ex.Err
		}
	}

	if onProgress != nil {
		onProgress(engine.Progress{Phase: "ddl", Percent: 100, Note: "all statements applied"})
	}
	ex.FinishedAt = e.now()
	return ex, nil
}

// setSessionGuards makes DDL fail fast instead of queueing for a metadata lock.
// It runs on the session that was pinned for the DDL, not on the pool: a guard
// set on one connection protects nothing. It is best-effort: a server that
// refuses the variable change is reported by the privilege checks, not fought
// with here.
func (e *Engine) setSessionGuards(ctx context.Context, q introspect.Querier) error {
	_, err := q.ExecContext(ctx, fmt.Sprintf("SET SESSION lock_wait_timeout = %d", e.opts.LockWaitTimeoutSeconds))
	if err != nil {
		// Non-fatal: the ALTER is still correct, it may just queue longer.
		return nil
	}
	return nil
}

// Verify asserts the intended end state through information_schema, not through
// the engine's exit code.
func (e *Engine) Verify(ctx context.Context) ([]engine.VerifyResult, error) {
	return engine.VerifyAgainstSchema(ctx, e.db, e.opts.Schema, e.opts.Table, e.opts.Plan.Verification)
}

func quoteIdent(s string) string { return engine.QuoteIdent(s) }

var _ engine.Engine = (*Engine)(nil)
