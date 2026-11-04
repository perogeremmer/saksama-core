// Package core orchestrates a migration session.
//
// It is the only place that knows the order of operations: observe, analyze,
// approve, lock, preflight, execute, verify, record. Every frontend (CLI, TUI,
// future web UI) drives this package rather than reimplementing the sequence.
package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/cred"
	"github.com/perogeremmer/saksama-core/internal/engine"
	"github.com/perogeremmer/saksama-core/internal/engine/copyeng"
	"github.com/perogeremmer/saksama-core/internal/engine/native"
	"github.com/perogeremmer/saksama-core/internal/events"
	"github.com/perogeremmer/saksama-core/internal/introspect"
	"github.com/perogeremmer/saksama-core/internal/model"
	"github.com/perogeremmer/saksama-core/internal/parser"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// ErrBlocked is returned when the analyzer refuses the change. It is a typed
// error so callers can map it to exit code 1 without inspecting strings.
var ErrBlocked = errors.New("migration blocked by safety rules")

// Request is one plan or apply invocation.
type Request struct {
	Conn     cred.Conn
	Database string
	Table    string
	Alter    string

	Production bool
	EngineHint analyzer.Engine
	// EngineHintSource is "flag" or "recorded"; it only affects reporting.
	EngineHintSource string
	Force            bool
	DryRun           bool

	LockWaitTimeoutSeconds int
	LongQueryThreshold     int64

	// Approve is called before any mutation. Returning an error aborts.
	Approve func(ApprovalRequest) (state.Approval, error)
	// Progress receives engine progress. May be nil.
	Progress func(engine.Progress)
	// EventSink, when non-nil, receives every event in addition to the log file.
	EventSink io.Writer
	// HoldCutover asks the engine to pause before swapping, when it supports it.
	HoldCutover bool
	// AllowArgvPassword permits an argv password for engines with no option file.
	AllowArgvPassword bool
	// Actor records who ran this, for the audit trail.
	Actor string
	// Binaries are the engine executables found locally, for the engine matrix.
	Binaries map[string]analyzer.EngineBinary
}

// ApprovalRequest is what the user is asked to approve.
type ApprovalRequest struct {
	Action          string
	Title           string
	RiskNote        string
	PlanFingerprint string
	Statements      []engine.Statement
	Production      bool
	Host            string
}

// Runner executes requests.
type Runner struct {
	DB    *sql.DB
	Store *state.Store
	Now   func() time.Time
}

// New creates a runner.
func New(db *sql.DB, store *state.Store) *Runner {
	return &Runner{DB: db, Store: store, Now: time.Now}
}

// now returns the clock, tolerating a Runner built as a struct literal. A nil
// clock must not panic in the middle of a migration setup.
func (r *Runner) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

// Prepared is the shared result of the observe-and-analyze phase.
type Prepared struct {
	Snapshot  introspect.Snapshot
	Plan      analyzer.Plan
	Alter     parser.Alter
	Session   *state.Session
	eventFile io.Closer
	emitter   *events.Writer
}

// Close releases the event log handle.
func (p *Prepared) Close() {
	if p.eventFile != nil {
		_ = p.eventFile.Close()
	}
}

// Prepare collects live state, parses the ALTER, and produces a plan.
//
// Nothing is mutated here. This is the function a CI gate uses.
func (r *Runner) Prepare(ctx context.Context, req Request) (*Prepared, error) {
	if r.DB == nil {
		return nil, errors.New("no database connection")
	}
	if strings.TrimSpace(req.Alter) == "" {
		return nil, errors.New("no ALTER statement given")
	}

	collector := introspect.NewCollector(r.DB, req.Database, req.Table)
	if req.LongQueryThreshold > 0 {
		collector.LongQueryThreshold = req.LongQueryThreshold
	}
	snap, err := collector.Collect(ctx)
	if err != nil {
		return nil, err
	}

	alter, alterErr := parser.ParseAlter("ALTER TABLE `" + escapeIdent(req.Table) + "` " + alterBody(req.Alter))

	// Duplicate probes run here because they depend on the planned indexes.
	duplicates := map[string]int64{}
	for _, op := range alter.Ops {
		if op.Kind != model.OpAddUniqueIndex {
			continue
		}
		name := op.IndexName
		cols := make([]string, 0, len(op.Parts))
		for _, p := range op.Parts {
			cols = append(cols, p.Column)
		}
		if name == "" && len(cols) > 0 {
			name = cols[0]
		}
		if n, err := collector.UniqueDuplicates(ctx, cols); err == nil {
			duplicates[name] = n
		}
	}

	var ignored []string
	ops := alter.Ops
	if alterErr != nil {
		ignored = append(ignored, "ALTER statement could not be parsed: "+alterErr.Error())
		ops = []model.Op{{Kind: model.OpUnknown, Raw: strings.TrimSpace(req.Alter)}}
	}

	input := analyzer.Input{
		Server:           snap.Server,
		Table:            snap.Table,
		Ops:              ops,
		RawAlter:         strings.TrimSpace(req.Alter),
		Algorithm:        alter.Algorithm,
		Lock:             alter.Lock,
		Host:             req.Conn.Describe(),
		DBUser:           req.Conn.User,
		DBPort:           req.Conn.Port,
		Production:       req.Production,
		SchemaSource:     "live",
		Ignored:          ignored,
		EngineHint:       req.EngineHint,
		EngineHintSource: req.EngineHintSource,
		Live:             liveStateFrom(snap, duplicates),
		Binaries:         req.Binaries,
	}

	plan := analyzer.Analyze(input)

	id, err := r.Store.NextID()
	if err != nil {
		return nil, err
	}

	sess := &state.Session{
		ID:                id,
		CreatedAt:         r.now().UTC().Format(time.RFC3339),
		State:             state.StatePlanned,
		Host:              req.Conn.Describe(),
		Database:          req.Database,
		Table:             req.Table,
		Production:        req.Production,
		Alter:             strings.TrimSpace(req.Alter),
		Engine:            string(plan.Recommendation.Engine),
		SchemaFingerprint: plan.SchemaFingerprint,
		ServerVersion:     snap.Server.Version,
		Blockers:          plan.Recommendation.Blockers,
		Warnings:          plan.Recommendation.Warnings,
		Reasons:           plan.Recommendation.Reasons,
		Actor:             req.Actor,
	}
	if raw, err := json.Marshal(plan); err == nil {
		sess.Plan = raw
	}
	if plan.Recommendation.Blocked {
		sess.State = state.StateBlocked
	}

	p := &Prepared{Snapshot: snap, Plan: plan, Alter: alter, Session: sess}

	// Open the event log and emit everything known so far.
	f, err := r.Store.OpenEventLog(id)
	if err != nil {
		return p, err
	}
	p.eventFile = f

	var sink io.Writer = f
	if req.EventSink != nil {
		sink = io.MultiWriter(f, req.EventSink)
	}
	p.emitter = events.NewWriter(sink, id)

	r.emit(p, events.TypeSessionCreated, map[string]any{
		"database": req.Database, "table": req.Table, "alter": sess.Alter,
		"host": sess.Host, "production": req.Production, "actor": req.Actor,
		"server_version": snap.Server.Version,
		"server_vendor":  snap.Server.Vendor,
	})
	r.emit(p, events.TypePlanComputed, map[string]any{
		"engine": plan.Recommendation.Engine, "fallbacks": plan.Recommendation.Fallbacks,
		"reasons": plan.Recommendation.Reasons, "blockers": plan.Recommendation.Blockers,
		"warnings": plan.Recommendation.Warnings,
		"duration": plan.Duration, "schema_fingerprint": plan.SchemaFingerprint,
	})
	for _, c := range plan.Checks {
		r.emit(p, events.TypeCheckFired, map[string]any{
			"id": c.ID, "severity": c.Severity, "title": c.Title,
			"detail": c.Detail, "remediation": c.Remediation, "mysql_error": c.MySQLError,
		})
	}
	p.Session.LastSeq = p.emitter.Seq()

	if err := r.Store.Save(sess); err != nil {
		return p, err
	}
	return p, nil
}

// Apply runs the full pipeline.
func (r *Runner) Apply(ctx context.Context, req Request) (*state.Session, error) {
	prep, err := r.Prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	defer prep.Close()

	sess := prep.Session
	plan := prep.Plan

	if plan.Recommendation.Blocked {
		r.emit(prep, events.TypePlanRejected, map[string]any{
			"blockers": plan.Recommendation.Blockers,
			"engine":   plan.Recommendation.Engine,
		})
		sess.LastSeq = prep.emitter.Seq()
		_ = r.Store.Save(sess)
		return sess, fmt.Errorf("%w: %d blocker(s); run 'saksama status %s' for details",
			ErrBlocked, plan.Recommendation.Blockers, sess.ID)
	}

	opts := engine.Options{
		Schema:                    req.Database,
		Table:                     req.Table,
		Alter:                     sess.Alter,
		Ops:                       prep.Alter.Ops,
		Plan:                      plan,
		TableModel:                prep.Snapshot.Table,
		LockWaitTimeoutSeconds:    req.LockWaitTimeoutSeconds,
		LongQueryThresholdSeconds: int(req.LongQueryThreshold),
		Force:                     req.Force,
		DryRun:                    req.DryRun,
		HoldCutover:               req.HoldCutover,
		AllowArgvPassword:         req.AllowArgvPassword,
		HasReplica:                prep.Snapshot.Runtime.ReplicaConfigured,
	}

	eng, err := r.buildEngine(plan.Recommendation.Engine, req, opts)
	if err != nil {
		sess.State = state.StateFailed
		sess.Error = err.Error()
		sess.LastSeq = prep.emitter.Seq()
		_ = r.Store.Save(sess)
		return sess, err
	}
	statements, err := eng.Statements()
	if err != nil {
		sess.State = state.StateFailed
		sess.Error = err.Error()
		sess.LastSeq = prep.emitter.Seq()
		_ = r.Store.Save(sess)
		return sess, err
	}
	for _, s := range statements {
		sess.Statements = append(sess.Statements, state.StatementRecord{
			Description: s.Description, SQL: s.SQL, Algorithm: s.Algorithm, Lock: s.Lock,
		})
	}

	// Approval happens before the table lock so a refused run leaves no trace.
	if req.Approve != nil {
		approval, err := req.Approve(ApprovalRequest{
			Action:          "apply",
			Title:           fmt.Sprintf("%s on %s.%s", time.Now().Format("15:04"), req.Database, req.Table),
			RiskNote:        strings.Join(plan.Recommendation.Reasons, "; "),
			PlanFingerprint: plan.SchemaFingerprint,
			Statements:      statements,
			Production:      req.Production,
			Host:            req.Conn.Describe(),
		})
		if err != nil {
			sess.State = state.StateCancelled
			sess.Error = "approval declined"
			_ = r.Store.Save(sess)
			return sess, err
		}
		approval.At = r.now().UTC().Format(time.RFC3339)
		approval.PlanFingerprint = plan.SchemaFingerprint
		sess.Approvals = append(sess.Approvals, approval)
		r.emit(prep, events.TypeApprovalGranted, map[string]any{
			"action": approval.Action, "actor": approval.Actor, "note": approval.Note,
		})
	}

	if req.DryRun {
		sess.State = state.StatePlanned
		sess.LastSeq = prep.emitter.Seq()
		_ = r.Store.Save(sess)
		return sess, nil
	}

	if err := r.Store.Lock(req.Database, req.Table, sess.ID, 12*time.Hour); err != nil {
		sess.State = state.StateFailed
		sess.Error = err.Error()
		sess.LastSeq = prep.emitter.Seq()
		_ = r.Store.Save(sess)
		return sess, err
	}
	defer func() { _ = r.Store.Unlock(req.Database, req.Table, sess.ID) }()

	sess.State = state.StateRunning
	sess.Phase = "preflight"
	sess.LastSeq = prep.emitter.Seq()
	_ = r.Store.Save(sess)
	r.emit(prep, events.TypePhaseStarted, map[string]any{"phase": "preflight"})

	if err := eng.Preflight(ctx); err != nil {
		sess.State = state.StateFailed
		sess.Error = err.Error()
		sess.LastSeq = prep.emitter.Seq()
		_ = r.Store.Save(sess)
		r.emit(prep, events.TypePhaseFailed, map[string]any{"phase": "preflight", "error": err.Error()})
		return sess, err
	}
	r.emit(prep, events.TypePhaseCompleted, map[string]any{"phase": "preflight"})

	sess.Phase = "ddl"
	r.emit(prep, events.TypePhaseStarted, map[string]any{"phase": "ddl"})

	exec, execErr := eng.Execute(ctx, func(p engine.Progress) {
		if req.Progress != nil {
			req.Progress(p)
		}
		r.emit(prep, events.TypePhaseProgress, map[string]any{
			"phase": p.Phase, "percent": p.Percent, "note": p.Note,
		})
	})

	for i, sr := range exec.Statements {
		if i < len(sess.Statements) {
			sess.Statements[i].DurationMS = sr.Duration.Milliseconds()
			sess.Statements[i].RowsAffected = sr.RowsAffected
			if sr.Err != nil {
				sess.Statements[i].Error = sr.Err.Error()
			}
		}
	}

	if execErr != nil {
		sess.State = state.StateFailed
		sess.Phase = "ddl"
		sess.Error = execErr.Error()
		sess.LastSeq = prep.emitter.Seq()
		_ = r.Store.Save(sess)
		r.emit(prep, events.TypePhaseFailed, map[string]any{"phase": "ddl", "error": execErr.Error()})
		return sess, execErr
	}
	r.emit(prep, events.TypePhaseCompleted, map[string]any{"phase": "ddl"})

	// Verification reads the schema back. The engine's exit code is not evidence.
	sess.State = state.StateVerifying
	sess.Phase = "verify"
	r.emit(prep, events.TypePhaseStarted, map[string]any{"phase": "verify"})

	results, verifyErr := eng.Verify(ctx)
	allOK := true
	for _, v := range results {
		sess.Verification = append(sess.Verification, state.VerifyRecord{
			Kind: v.Kind, Name: v.Name, OK: v.OK, Detail: v.Detail,
		})
		if !v.OK {
			allOK = false
		}
	}

	if verifyErr != nil || !allOK {
		sess.State = state.StateFailed
		sess.Phase = "verify"
		if verifyErr != nil {
			sess.Error = verifyErr.Error()
		} else {
			sess.Error = "post-execution verification did not confirm the intended schema change"
		}
		sess.LastSeq = prep.emitter.Seq()
		_ = r.Store.Save(sess)
		r.emit(prep, events.TypePhaseFailed, map[string]any{"phase": "verify", "error": sess.Error})
		return sess, errors.New(sess.Error)
	}
	r.emit(prep, events.TypePhaseCompleted, map[string]any{"phase": "verify"})

	sess.State = state.StateDone
	sess.Phase = "done"
	sess.Error = ""
	sess.LastSeq = prep.emitter.Seq()
	r.emit(prep, events.TypeSessionCompleted, map[string]any{
		"duration_ms": exec.FinishedAt.Sub(exec.StartedAt).Milliseconds(),
		"statements":  len(exec.Statements),
		"verified":    len(sess.Verification),
	})
	sess.LastSeq = prep.emitter.Seq()
	_ = r.Store.Save(sess)

	return sess, nil
}

// PlanOnly runs the analyze phase and returns the plan without mutating anything.
func (r *Runner) PlanOnly(ctx context.Context, req Request) (*Prepared, error) {
	return r.Prepare(ctx, req)
}

func (r *Runner) emit(p *Prepared, typ string, data map[string]any) {
	if p.emitter == nil {
		return
	}
	_, _ = p.emitter.Emit(typ, data)
}

func liveStateFrom(snap introspect.Snapshot, duplicates map[string]int64) *analyzer.LiveState {
	live := &analyzer.LiveState{
		ThreadsRunning:    snap.Runtime.ThreadsRunning,
		LongQueryCount:    len(snap.Runtime.LongQueries),
		LongTransactions:  len(snap.Runtime.Transactions),
		PendingMDLocks:    snap.Runtime.PendingMDLocks,
		ReplicaConfigured: snap.Runtime.ReplicaConfigured,
		ReplicaRunning:    snap.Runtime.ReplicaRunning,
		ReplicaLagSeconds: snap.Runtime.ReplicaLagSec,
		FreeDiskBytes:     snap.Runtime.FreeDiskBytes,
		Duplicates:        duplicates,
		Grants: analyzer.Grants{
			User:             snap.Runtime.Grants.User,
			Super:            snap.Runtime.Grants.Super,
			Process:          snap.Runtime.Grants.Process,
			Reload:           snap.Runtime.Grants.Reload,
			SystemVarsAdmin:  snap.Runtime.Grants.SystemVarsAdmin,
			SessionVarsAdmin: snap.Runtime.Grants.SessionVarsAdmin,
			AllPrivileges:    snap.Runtime.Grants.AllPrivileges,
			Known:            len(snap.Runtime.Grants.Raw) > 0,
		},
	}

	if len(snap.Runtime.LongQueries) > 0 {
		longest := snap.Runtime.LongQueries[0]
		for _, q := range snap.Runtime.LongQueries {
			if q.Seconds > longest.Seconds {
				longest = q
			}
		}
		live.LongQuerySeconds = longest.Seconds
		live.LongestQuerySQL = introspect.SanitizeSQL(longest.SQL, 200)
		live.LongestQueryID = longest.ID
	}

	for _, trx := range snap.Runtime.Transactions {
		if trx.AgeSeconds > live.OldestTrxSeconds {
			live.OldestTrxSeconds = trx.AgeSeconds
		}
	}

	for _, a := range snap.Runtime.Artifacts {
		live.LeftoverArtifacts = append(live.LeftoverArtifacts, a.Kind+" "+a.Name)
	}

	for _, fk := range snap.FKChecks {
		compat := analyzer.FKCompat{
			Constraint: fk.Constraint, Direction: fk.Direction,
			PeerTable: fk.PeerTable, Compatible: fk.Compatible,
		}
		if !fk.Compatible {
			var diffs []string
			for _, pair := range fk.Pairs {
				if pair.Compatible {
					continue
				}
				diffs = append(diffs, fmt.Sprintf("%s is %s/%s but %s is %s/%s",
					pair.LocalColumn, pair.LocalCharset, pair.LocalCollation,
					pair.PeerColumn, pair.PeerCharset, pair.PeerCollation))
			}
			compat.Description = strings.Join(diffs, "; ")
		}
		live.FKCompatibility = append(live.FKCompatibility, compat)
	}

	return live
}

func escapeIdent(s string) string { return strings.ReplaceAll(s, "`", "``") }

// alterBody accepts either a bare operation list or a full ALTER TABLE statement.
func alterBody(s string) string {
	t := strings.TrimSpace(s)
	if len(t) >= len("alter table") && strings.EqualFold(t[:len("alter table")], "alter table") {
		return strings.TrimSpace(t[len("alter table"):])
	}
	return t
}

// buildEngine returns the adapter for the chosen engine.
func (r *Runner) buildEngine(name analyzer.Engine, req Request, opts engine.Options) (engine.Engine, error) {
	conn := copyeng.Conn{
		Host: req.Conn.Host, Port: req.Conn.Port, User: req.Conn.User,
		Password: req.Conn.Password, Socket: req.Conn.Socket,
	}

	switch name {
	case analyzer.EngineNative:
		return native.New(r.DB, opts), nil
	case analyzer.EngineGhOst:
		return copyeng.New(copyeng.GhOst(), r.DB, conn, opts, nil), nil
	case analyzer.EnginePtOSC:
		return copyeng.New(copyeng.PtOSC(), r.DB, conn, opts, nil), nil
	}
	return nil, fmt.Errorf("%w: no adapter for engine %q", ErrBlocked, name)
}
