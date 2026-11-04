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
	// ForExecution is set by `apply`. A plan tolerates an unmeasured disk; an
	// execution does not.
	ForExecution bool
	// ResumedFrom names the session whose plan this execution follows.
	ResumedFrom string

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
	// FreeDisk and DiskTotal let an operator supply the numbers when the data
	// directory is not on this machine, which is the normal case for a managed
	// service.
	FreeDisk  int64
	DiskTotal int64
	// ServerPort is @@port, used to tell gh-ost that a port mismatch is expected.
	ServerPort int
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
	// Settings come first: the data directory is needed before Collect can try to
	// measure the volume it lives on.
	settings, _ := collector.Settings(ctx)
	if dir := settings["datadir"]; dir != "" {
		collector.DataDir = dir
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
		Settings:         analyzer.SettingsFromMap(settings),
		Disk:             diskFacts(snap, req),
		Force:            req.Force,
		ForExecution:     req.ForExecution,
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
		ResumedFrom:       req.ResumedFrom,
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
		// Name the blockers in the session itself, so `apply` and `status` say why
		// without the operator having to open the stored plan.
		var reasons []string
		for _, c := range plan.Checks {
			if c.Severity == analyzer.SeverityBlocker {
				reasons = append(reasons, fmt.Sprintf("%s %s", c.ID, c.Title))
			}
		}
		sess.Error = "blocked by " + strings.Join(reasons, "; ")
		r.emit(prep, events.TypePlanRejected, map[string]any{
			"blockers": plan.Recommendation.Blockers,
			"engine":   plan.Recommendation.Engine,
			"reasons":  reasons,
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

	eng, err := r.buildEngine(plan.Recommendation.Engine, req, opts, prep.Snapshot.Server.Port)
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

	// The schema must still be the one that was planned and approved. Between the
	// plan and this moment a deploy, another migration, or a colleague's ALTER can
	// change it, and an ALTER written for the old shape is the wrong ALTER.
	if err := r.revalidateSchema(ctx, prep, sess, req); err != nil {
		sess.State = state.StateFailed
		sess.Error = err.Error()
		sess.LastSeq = prep.emitter.Seq()
		_ = r.Store.Save(sess)
		r.emit(prep, events.TypePhaseFailed, map[string]any{"phase": "preflight", "error": err.Error()})
		return sess, err
	}

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
func (r *Runner) buildEngine(name analyzer.Engine, req Request, opts engine.Options, serverPort int) (engine.Engine, error) {
	conn := copyeng.Conn{
		Host: req.Conn.Host, Port: req.Conn.Port, User: req.Conn.User,
		Password: req.Conn.Password, Socket: req.Conn.Socket,
		ServerPort: serverPort,
	}

	switch name {
	case analyzer.EngineNative:
		return native.New(r.DB, opts), nil
	case analyzer.EngineGhOst:
		opts.BinaryPath = binaryPath(req.Binaries, "gh-ost")
		return copyeng.New(copyeng.GhOst(), r.DB, conn, opts, nil), nil
	case analyzer.EnginePtOSC:
		opts.BinaryPath = binaryPath(req.Binaries, "pt-online-schema-change")
		return copyeng.New(copyeng.PtOSC(), r.DB, conn, opts, nil), nil
	}
	return nil, fmt.Errorf("%w: no adapter for engine %q", ErrBlocked, name)
}

// diskFacts prefers a measurement from this machine, then whatever the operator
// supplied, and otherwise says why it could not be measured.
func diskFacts(snap introspect.Snapshot, req Request) analyzer.DiskFacts {
	if snap.Runtime.Disk.Measured {
		return analyzer.DiskFacts{
			Free:     snap.Runtime.Disk.Free,
			Total:    snap.Runtime.Disk.Total,
			Measured: true,
			Source:   "filesystem",
		}
	}
	if req.FreeDisk > 0 {
		return analyzer.DiskFacts{
			Free:     req.FreeDisk,
			Total:    req.DiskTotal,
			Measured: true,
			Source:   "operator",
		}
	}
	return analyzer.DiskFacts{Reason: snap.Runtime.Disk.Reason}
}

// binaryPath is the executable the plan verified, when it verified one.
func binaryPath(bins map[string]analyzer.EngineBinary, name string) string {
	if bins == nil {
		return ""
	}
	b, ok := bins[name]
	if !ok || !b.Available {
		return ""
	}
	return b.Path
}

// revalidateSchema compares the schema now against the schema the plan was built
// from, and refuses on any difference the fingerprint tracks: columns, indexes,
// foreign keys, engine, row format, charset.
//
// Volatile facts (row counts, sizes) are deliberately excluded, so a growing table
// does not trip this.
func (r *Runner) revalidateSchema(ctx context.Context, prep *Prepared, sess *state.Session, req Request) error {
	current, err := introspect.NewCollector(r.DB, req.Database, req.Table).Table(ctx)
	if err != nil {
		return fmt.Errorf("cannot re-read the schema before executing: %w", err)
	}

	now := analyzer.Fingerprint(current)
	if now == prep.Plan.SchemaFingerprint {
		return nil
	}

	sess.SchemaFingerprint = now
	return fmt.Errorf(
		"the schema changed after this plan was made (planned %s, now %s); "+
			"someone altered %s.%s in between, so re-plan before executing",
		shortFingerprint(prep.Plan.SchemaFingerprint), shortFingerprint(now),
		req.Database, req.Table)
}

func shortFingerprint(f string) string {
	if len(f) > 19 {
		return f[:19]
	}
	return f
}

// ApplyFromSession executes a plan that was produced and reviewed earlier.
//
// This is the workflow the tool claims to support: plan, read it, decide, execute.
// The schema must still be the one that plan describes, otherwise the ALTER was
// written for a table that has since changed.
func (r *Runner) ApplyFromSession(ctx context.Context, sessionID string, req Request) (*state.Session, error) {
	stored, err := r.Store.Load(sessionID)
	if err != nil {
		return nil, err
	}
	if stored.State.Terminal() {
		return nil, fmt.Errorf("session %s already ended as %q", sessionID, stored.State)
	}
	if stored.Blockers > 0 {
		return nil, fmt.Errorf("%w: session %s was planned with %d blocker(s)", ErrBlocked, sessionID, stored.Blockers)
	}

	// Fill what the caller did not repeat.
	if req.Database == "" {
		req.Database = stored.Database
	}
	if req.Table == "" {
		req.Table = stored.Table
	}
	if strings.TrimSpace(req.Alter) == "" {
		req.Alter = stored.Alter
	}
	if req.Production != stored.Production && !req.Production {
		req.Production = stored.Production
	}

	// Compare the stored plan against the schema as it is now, before doing
	// anything. A plan is a statement about a shape, and shapes move.
	current, err := introspect.NewCollector(r.DB, req.Database, req.Table).Table(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot read the table before executing session %s: %w", sessionID, err)
	}
	if now := analyzer.Fingerprint(current); now != stored.SchemaFingerprint {
		return nil, fmt.Errorf(
			"%w: the schema of %s.%s changed since session %s was planned (planned %s, now %s); re-plan first",
			ErrBlocked, req.Database, req.Table, sessionID,
			shortFingerprint(stored.SchemaFingerprint), shortFingerprint(now))
	}

	if req.EngineHint == "" && stored.Engine != "" && stored.Engine != "none" {
		req.EngineHint = analyzer.Engine(stored.Engine)
		req.EngineHintSource = "recorded"
	}
	req.ResumedFrom = sessionID

	return r.Apply(ctx, req)
}
