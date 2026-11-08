// Package core orchestrates a migration session.
//
// It is the only place that knows the order of operations: observe, analyze,
// approve, lock, preflight, execute, verify, record. Every frontend (CLI, TUI,
// future web UI) drives this package rather than reimplementing the sequence.
package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	// AcceptUnknownMDL says the operator knows this server cannot report its
	// metadata lock queue, and accepts the risk. Only MySQL 5.6 needs it.
	AcceptUnknownMDL bool
	// AllowNativeRebuild lets MySQL rebuild a table in place in production.
	// Outside production a small table needs no such decision.
	AllowNativeRebuild bool
	EngineHint         analyzer.Engine
	// EngineHintSource is "flag" or "recorded"; it only affects reporting.
	EngineHintSource string
	Force            bool
	DryRun           bool
	// ForExecution is set by `apply`. A plan tolerates an unmeasured disk; an
	// execution does not.
	ForExecution bool
	// ResumedFrom names the session whose plan this execution follows.
	ResumedFrom string
	// Detach runs the copy engine in its own process group, so a dropped SSH
	// session does not kill it. `saksama cancel` stops it by recorded PID.
	Detach bool

	LockWaitTimeoutSeconds int
	LongQueryThreshold     int64
	// ChunkSize is how many rows the copy engine takes per chunk. Zero keeps the
	// engine default (1000). It is the dial that decides whether a very large
	// table is copied gently or quickly, and the one a recovery test uses to keep
	// a copy alive long enough to kill.
	ChunkSize int

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

// Analyze observes the server and decides what a change costs. It writes
// nothing: no session, no event log, nothing in the state directory.
//
// This is what a preview, a wizard, a CI gate, or a web request that is only
// asking a question must call. Prepare builds on it when a durable record is
// actually wanted.
func (r *Runner) Analyze(ctx context.Context, req Request) (*Prepared, error) {
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
		Server:             snap.Server,
		Table:              snap.Table,
		Ops:                ops,
		RawAlter:           strings.TrimSpace(req.Alter),
		Algorithm:          alter.Algorithm,
		Lock:               alter.Lock,
		Host:               req.Conn.Describe(),
		DBUser:             req.Conn.User,
		DBPort:             req.Conn.Port,
		Production:         req.Production,
		AcceptUnknownMDL:   req.AcceptUnknownMDL,
		AllowNativeRebuild: req.AllowNativeRebuild,
		SchemaSource:       "live",
		Ignored:            ignored,
		EngineHint:         req.EngineHint,
		EngineHintSource:   req.EngineHintSource,
		Live:               liveStateFrom(snap, duplicates),
		Binaries:           req.Binaries,
		Settings:           analyzer.SettingsFromMap(settings),
		Disk:               diskFacts(snap, req),
		Force:              req.Force,
		ForExecution:       req.ForExecution,
	}

	return &Prepared{Snapshot: snap, Plan: analyzer.Analyze(input), Alter: alter}, nil
}

// Prepare is Analyze plus the durable record: a session, its append-only event
// log, and the plan exactly as it was decided.
//
// Everything that can refuse a change has already refused it by the time this
// returns. A session is a record of a decision, not a claim that the decision
// will be executed.
func (r *Runner) Prepare(ctx context.Context, req Request) (*Prepared, error) {
	if r.Store == nil {
		return nil, errors.New("no state directory")
	}

	p, err := r.Analyze(ctx, req)
	if err != nil {
		return nil, err
	}
	snap, plan := p.Snapshot, p.Plan

	// The id is claimed by creating its file, so two concurrent prepares cannot
	// both be told s1 is free and then interleave their audit trails.
	id, err := r.Store.ClaimID()
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

	p.Session = sess

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
	sess.LastSeq = p.emitter.Seq()

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
		// Point at the detail: the one line here names the checks, and the fix for
		// each one (a flag, a command, a query) is in the plan that `status` prints.
		sess.Error = fmt.Sprintf("blocked by %s (the plan detail and what to do about each is in 'saksama status %s')",
			strings.Join(reasons, "; "), sess.ID)
		r.emit(prep, events.TypePlanRejected, map[string]any{
			"blockers": plan.Recommendation.Blockers,
			"engine":   plan.Recommendation.Engine,
			"reasons":  reasons,
		})
		sess.LastSeq = prep.emitter.Seq()
		_ = r.Store.Save(sess)
		return sess, fmt.Errorf("%w: %d blocker(s); the plan detail, including what to do about each one, is in 'saksama status %s'",
			ErrBlocked, plan.Recommendation.Blockers, sess.ID)
	}

	// A held cutover only means something if the gate exists before the engine
	// starts: the engine waits for the file to disappear, so the file must be there.
	cutoverGate := ""
	if req.HoldCutover {
		cutoverGate = cutoverGatePath(r.Store.Dir(), sess.ID)
		if err := os.WriteFile(cutoverGate, []byte("held by session "+sess.ID+"\n"), 0o600); err != nil {
			sess.State = state.StateFailed
			sess.Error = fmt.Sprintf("cannot create the cutover gate: %v", err)
			_ = r.Store.Save(sess)
			return sess, errors.New(sess.Error)
		}
		sess.CutoverFlagFile = cutoverGate
		sess.RecordArtifact("gate", cutoverGate, string(plan.Recommendation.Engine),
			"cutover gate file; removing it releases the swap")
		// The gate must outlive the engine - it is what the engine waits on - but
		// not the run. This catches every early return below; the paths that save
		// a session also clean the gate before saving, so the record matches /tmp.
		defer cleanupGate(sess)
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
		CutoverGate:               cutoverGate,
		AllowArgvPassword:         req.AllowArgvPassword,
		ChunkSize:                 req.ChunkSize,
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
	// Whatever happens from here - approval declined, dry run, a failed start -
	// nothing the engine staged for this run may outlive it.
	defer cleanupStaged(eng)
	statements, err := eng.Statements()
	if err != nil {
		sess.State = state.StateFailed
		sess.Error = err.Error()
		sess.LastSeq = prep.emitter.Seq()
		_ = r.Store.Save(sess)
		return sess, err
	}
	// Record the artifacts the chosen engine is about to create, before it runs, so
	// ownership exists even if this process dies halfway.
	recordExpectedArtifacts(sess, plan.Recommendation.Engine, req)

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
			// No engine was started, so the gate it never waited on must go now,
			// before the session is saved without it.
			cleanupGate(sess)
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
		// Nothing will wait on the gate, so a dry run must not leave one behind:
		// the next `cutover` would look meaningful and mean nothing.
		cleanupGate(sess)
		sess.State = state.StatePlanned
		sess.LastSeq = prep.emitter.Seq()
		_ = r.Store.Save(sess)
		return sess, nil
	}

	if err := r.Store.Lock(req.Database, req.Table, sess.ID, 12*time.Hour); err != nil {
		cleanupGate(sess)
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

	// Detach is opt-in through the request, because a user watching a short
	// migration in a terminal expects ctrl-c to reach it.
	// The pid is recorded whether or not the run is detached: `status` should show
	// it, and `cancel` from another terminal should find it. Detaching only changes
	// who owns the process group, not whether the process has an identity.
	if d, ok := eng.(engine.Detachable); ok {
		d.SetDetach(req.Detach, func(pid int, identity string) {
			sess.EnginePID = pid
			sess.EngineIdentity = identity
			sess.LastSeq = prep.emitter.Seq()
			_ = r.Store.Save(sess)
			note := "closing this terminal stops the engine"
			if req.Detach {
				note = "detached: closing this terminal will not stop the migration"
			}
			r.emit(prep, events.TypeEngineStarted, map[string]any{
				"engine": sess.Engine, "pid": pid, "note": note,
			})
		})
	}

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
		// A failed run leaves the most debris, and the operator is most likely to
		// want it gone. Still only what this session recorded.
		cleanupGate(sess)
		sess.MarkArtifactsSafe("leftover from a failed migration; the production table was not swapped")
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
		// The engine has exited, so the gate has served its purpose whatever the
		// outcome. Leaving it in /tmp would make the next `cutover` look meaningful.
		cleanupGate(sess)
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

	cleanupGate(sess)
	sess.State = state.StateDone
	sess.Phase = "done"
	sess.Error = ""
	sess.MarkArtifactsSafe("leftover from a finished migration; safe once you have checked the table")
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
		MDLObservable:     snap.Runtime.MDLObservable,
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
			PeerTable: fk.PeerTable, Compatible: fk.Compatible, Verified: fk.Verified,
		}
		if compat.Description == "" && fk.Note != "" {
			// The warn branch needs a reason; the unreadable peer or the skipped
			// pairs are the reason.
			compat.Description = fk.Note
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
	return analyzer.DiskFacts{
		Reason:     snap.Runtime.Disk.Reason,
		Path:       snap.Runtime.Disk.Path,
		ServerHost: snap.Runtime.Disk.ServerHost,
	}
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

// cutoverGatePath is where gh-ost waits before swapping.
//
// The session id alone is not unique: two state directories both start at s1, and
// two runs sharing /tmp/saksama-s1.cutover would let one session release the
// other's cut-over. The directory is folded into the name so the file belongs to
// exactly one session record.
func cutoverGatePath(stateDir, sessionID string) string {
	sum := sha256.Sum256([]byte(stateDir))
	return filepath.Join(os.TempDir(), fmt.Sprintf("saksama-%x-%s.cutover", sum[:4], sessionID))
}

// recordExpectedArtifacts names what the engine will create, so cleanup has
// ownership rather than a naming convention.
func recordExpectedArtifacts(sess *state.Session, eng analyzer.Engine, req Request) {
	t := sess.Table
	switch eng {
	case analyzer.EngineGhOst:
		// gh-ost is triggerless by design: it reads the binary log instead of
		// adding triggers, and that is why the ladder prefers it. Recording trigger
		// names here would have cleanup report dropping three objects that never
		// existed, and the changelog table - which gh-ost really does leave behind -
		// would stay unowned and undeleted. Verified against gh-ost 1.1.11: a live
		// copy creates _t_gho and _t_ghc and no triggers at all.
		sess.RecordArtifact("table", "_"+t+"_gho", "gh-ost", "ghost table, present only while copying")
		sess.RecordArtifact("table", "_"+t+"_ghc", "gh-ost", "gh-ost changelog table, kept after a crash")
		sess.RecordArtifact("table", "_"+t+"_del", "gh-ost", "pre-migration table, present only after a cut-over")
	case analyzer.EnginePtOSC:
		sess.RecordArtifact("table", "_"+t+"_new", "pt-osc", "shadow table, present only while copying")
		sess.RecordArtifact("table", "_"+t+"_old", "pt-osc", "pre-migration table during the swap")
		for _, suffix := range []string{"_ins", "_upd", "_del"} {
			sess.RecordArtifact("trigger", "pt_osc_"+req.Database+"_"+t+suffix, "pt-osc", "copy trigger")
		}
	}
}

// cleanupStaged removes what the engine staged for this run, on every exit
// path. Engines that stage nothing simply do not implement the interface.
func cleanupStaged(eng engine.Engine) {
	if c, ok := eng.(engine.Cleaner); ok {
		c.Cleanup()
	}
}

// cleanupGate removes the cutover gate file once the engine has exited, so a
// finished session leaves no stray file in /tmp.
func cleanupGate(sess *state.Session) {
	if sess.CutoverFlagFile == "" {
		return
	}
	_ = os.Remove(sess.CutoverFlagFile)
	sess.ForgetsArtifact("gate", sess.CutoverFlagFile)
	sess.CutoverFlagFile = ""
}

// ReleaseCutover removes the gate so the engine may swap tables.
//
// This is the verification gate the product is built around: the copy is finished,
// the operator has looked at the shadow table, and only then is the swap allowed.
func (r *Runner) ReleaseCutover(ctx context.Context, sessionID string) (*state.Session, error) {
	sess, err := r.Store.Load(sessionID)
	if err != nil {
		return nil, err
	}
	if sess.CutoverFlagFile == "" {
		return nil, fmt.Errorf("session %s has no held cutover (it was not started with --hold-cutover, or the gate is already released)", sessionID)
	}
	if err := os.Remove(sess.CutoverFlagFile); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("cannot release the cutover gate: %w", err)
	}

	// The engine notices the file is gone and swaps. Its process is the one that
	// reports the outcome, so this command only records the decision.
	sess.CutoverFlagFile = ""
	sess.RecordArtifact("cutover", "released", sess.Engine, "cutover approved by "+actorOrUnknown(sess))
	sess.ForgetsArtifact("gate", "")
	sess.ForgetsArtifact("cutover", "")
	return sess, r.Store.Save(sess)
}

func actorOrUnknown(sess *state.Session) string {
	if sess.Actor != "" {
		return sess.Actor
	}
	return "an operator"
}
