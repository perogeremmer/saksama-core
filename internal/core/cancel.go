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
func (r *Runner) Cancel(ctx context.Context, sessionID string) (CancelResult, error) {
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

	if err := process.Stop(sess.EnginePID, StopTimeout); err != nil {
		return res, fmt.Errorf("stopping engine pid %d: %w", sess.EnginePID, err)
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
	res.Note = fmt.Sprintf(
		"stopped engine pid %d. The production table was not swapped, because the engine had not cut over.",
		sess.EnginePID)
	return res, nil
}
