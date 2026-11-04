// Package engine defines the contract every execution path implements.
//
// Engines are deliberately dumb workers: they build argv or SQL, run it, and
// report. All judgement lives in the analyzer, and all supervision lives in the
// core. That is what keeps gh-ost, pt-online-schema-change, and native DDL
// interchangeable without three copies of the same policy.
package engine

import (
	"context"
	"time"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/model"
)

// Name identifies an execution path.
type Name string

const (
	Native Name = "native"
	GhOst  Name = "gh-ost"
	PtOSC  Name = "pt-osc"
)

// Statement is one unit of work an engine will run.
type Statement struct {
	// Description is what this statement does, in plain words, for the audit log.
	Description string
	// SQL is the exact statement, with ALGORITHM and LOCK spelled out.
	SQL string
	// Algorithm and Lock are the executability guarantees requested.
	Algorithm string
	Lock      string
	// Ops are the ALTER operations this statement covers.
	Ops []model.Op
}

// VerifyResult is the outcome of one post-execution assertion.
type VerifyResult struct {
	Kind   string
	Name   string
	OK     bool
	Detail string
}

// Progress is a point-in-time observation an engine can report.
type Progress struct {
	Phase          string
	RowsCopied     int64
	RowsTotal      int64
	Percent        float64
	ETASeconds     int64
	Throttled      bool
	ThreadsRunning int
	ReplicaLagSec  float64
	Note           string
}

// Execution is the result of running an engine.
type Execution struct {
	Engine     Name
	StartedAt  time.Time
	FinishedAt time.Time
	Statements []StatementResult
	Err        error
}

// StatementResult is the outcome of one statement.
type StatementResult struct {
	Statement    Statement
	Duration     time.Duration
	Err          error
	RowsAffected int64
}

// Engine is the interface the core drives.
type Engine interface {
	// Name reports which path this is.
	Name() Name
	// Statements is the exact work that will be executed. It is pure: calling it
	// twice yields the same result and touches nothing.
	Statements() ([]Statement, error)
	// Preflight rechecks, immediately before execution, the hazards that can
	// appear between planning and running.
	Preflight(ctx context.Context) error
	// Execute runs the statements. onProgress may be nil.
	Execute(ctx context.Context, onProgress func(Progress)) (Execution, error)
	// Verify asserts the intended schema change actually happened.
	Verify(ctx context.Context) ([]VerifyResult, error)
}

// Options carries what every engine needs.
type Options struct {
	Schema     string
	Table      string
	Alter      string
	Ops        []model.Op
	Plan       analyzer.Plan
	TableModel model.Table
	// LockWaitTimeoutSeconds bounds how long DDL waits for a metadata lock. It is
	// intentionally small: failing fast is better than queueing every query
	// behind an ALTER that cannot start.
	LockWaitTimeoutSeconds int
	// LongQueryThresholdSeconds is the age at which a running statement is
	// treated as a metadata-lock hazard during preflight. 0 means 30.
	LongQueryThresholdSeconds int
	// Force downgrades selected preflight aborts to warnings. It never bypasses
	// capability or FK blockers, which are enforced by the analyzer.
	Force bool
	// DryRun builds statements and reports them without executing.
	DryRun bool
}
