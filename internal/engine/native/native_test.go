package native

import (
	"strings"
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
