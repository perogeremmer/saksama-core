package core

import (
	"context"
	"fmt"
	"time"

	"github.com/perogeremmer/saksama-core/internal/engine/process"
	"github.com/perogeremmer/saksama-core/internal/events"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// StopTimeout is how long a copy engine is given to exit after the stop signal.
// gh-ost and pt-osc both clean up their triggers and shadow table on a normal
// exit, and killing them harder than this leaves more debris than it saves.
const StopTimeout = 20 * time.Second

// CancelOutcome is what cancel found, which matters more than what it did: three
// of the four outcomes are "there was nothing to stop".
type CancelOutcome string

const (
	CancelStopped      CancelOutcome = "stopped"
	CancelAlreadyEnded CancelOutcome = "already-ended"
	CancelNoEngine     CancelOutcome = "no-engine"
	CancelNotRunning   CancelOutcome = "not-running"
)

// CancelResult reports the outcome without pretending it was uniform.
type CancelResult struct {
	ID      string        `json:"id"`
	PID     int           `json:"pid,omitempty"`
	Outcome CancelOutcome `json:"outcome"`
	// ForceKilled reports that SIGTERM was not enough and SIGKILL was used. The
	// consequence belongs in the note, not in a footnote: a killed engine cannot
	// drop its own shadow table and triggers.
	ForceKilled bool `json:"force_killed,omitempty"`
	// State is the session state after the attempt.
	State state.State `json:"state"`
	Note  string      `json:"note"`
}

// Cancel stops the copy engine recorded for a session.
//
// It talks to the state directory and the process table, not to a daemon, so it
// works from another terminal, another ssh session, or after a reconnect. The
// production table is untouched: a stop before cutover means the swap never
// happened, and that is exactly what the note says.
func (r *Runner) Cancel(ctx context.Context, sessionID string, force bool) (CancelResult, error) {
	if r.Store == nil {
		return CancelResult{}, fmt.Errorf("no state directory")
	}
	sess, err := r.Store.Load(sessionID)
	if err != nil {
		return CancelResult{}, err
	}

	res := CancelResult{ID: sess.ID, PID: sess.EnginePID, State: sess.State}

	if sess.State.Terminal() {
		res.Outcome = CancelAlreadyEnded
		res.Note = fmt.Sprintf("session %s already ended as %q, nothing to cancel", sess.ID, sess.State)
		return res, nil
	}
	if sess.EnginePID == 0 {
		res.Outcome = CancelNoEngine
		res.Note = fmt.Sprintf(
			"session %s has no recorded engine process; it may be a native DDL the server owns",
			sess.ID)
		return res, nil
	}
	if !process.Alive(sess.EnginePID) {
		// Not a success and not a failure: the session is in exactly the state
		// `reconcile` exists for. Guessing here would hide that.
		res.Outcome = CancelNotRunning
		res.Note = fmt.Sprintf("engine pid %d is no longer running; run `saksama reconcile` to see what is left", sess.EnginePID)
		return res, nil
	}

	var forceKilled bool
	if force {
		var err error
		forceKilled, err = process.StopForce(sess.EnginePID, StopTimeout)
		if err != nil {
			res.Note = fmt.Sprintf(
				"engine pid %d did not stop: %v. The session is still marked running, because it is.", sess.EnginePID, err)
			return res, err
		}
	} else if err := process.Stop(sess.EnginePID, StopTimeout); err != nil {
		res.Note = fmt.Sprintf(
			"engine pid %d ignored SIGTERM for %s: %v. It is still running, and the session still says so. "+
				"Re-run with --force-kill to send SIGKILL, and expect the engine to leave its shadow table and triggers behind.",
			sess.EnginePID, StopTimeout, err)
		return res, err
	}

	sess.State = state.StateCancelled
	sess.Phase = "cancelled"
	sess.Error = fmt.Sprintf("cancelled by %s at %s", actorOrUnknown(sess), time.Now().UTC().Format(time.RFC3339))
	sess.MarkArtifactsSafe("leftover from a cancelled migration; the production table was not swapped")
	r.recordEvent(sess, events.TypeSessionCancelled, map[string]any{
		"pid":    sess.EnginePID,
		"actor":  actorOrUnknown(sess),
		"reason": "stopped by operator",
	})
	if err := r.Store.Save(sess); err != nil {
		return res, err
	}

	res.Outcome = CancelStopped
	res.State = sess.State
	res.ForceKilled = forceKilled
	if forceKilled {
		res.Note = fmt.Sprintf(
			"killed engine pid %d after SIGTERM was ignored. The production table was not swapped. "+
				"SIGKILL gives the engine no chance to drop its shadow table or triggers, so run "+
				"`saksama reconcile` and then `saksama cleanup %s` to remove what it left.",
			sess.EnginePID, sess.ID)
		return res, nil
	}
	res.Note = fmt.Sprintf(
		"stopped engine pid %d. The production table was not swapped, because the engine had not cut over.",
		sess.EnginePID)
	return res, nil
}
