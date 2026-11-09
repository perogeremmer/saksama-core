package core

import (
	"context"
	"fmt"
	"time"

	"github.com/perogeremmer/saksama-core/internal/engine/process"
	"github.com/perogeremmer/saksama-core/internal/events"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// RetireOutcome mirrors CancelOutcome: three of the four possibilities are
// "there was nothing to retire".
type RetireOutcome string

const (
	RetireRetired      RetireOutcome = "retired"
	RetireAlreadyEnded RetireOutcome = "already-ended"
	RetireEngineAlive  RetireOutcome = "engine-alive"
)

// RetireResult is what retiring a session did.
type RetireResult struct {
	ID      string        `json:"id"`
	Outcome RetireOutcome `json:"outcome"`
	State   state.State   `json:"state"`
	// LockReleased reports whether the target table became available again.
	LockReleased bool   `json:"lock_released"`
	Note         string `json:"note"`
}

// Retire marks a non-terminal session as failed after the engine behind it is
// confirmed gone, and releases the table lock it held.
//
// This is the verb `reconcile` keeps prescribing - "mark it failed and re-plan"
// - and it exists because without it that advice is a dead end: a crashed client
// leaves the session saying "running" forever, `apply` refuses a non-terminal
// session, and the lock stays until its stale window passes. Retire does not
// guess whether the migration happened; it records that the operator looked, saw
// the engine gone, and decided this attempt is over. The verification on the next
// plan is what guards the schema itself.
func (r *Runner) Retire(ctx context.Context, sessionID string) (RetireResult, error) {
	if r.Store == nil {
		return RetireResult{}, fmt.Errorf("no state directory")
	}
	sess, err := r.Store.Load(sessionID)
	if err != nil {
		return RetireResult{}, err
	}

	res := RetireResult{ID: sess.ID, State: sess.State}

	if sess.State.Terminal() {
		res.Outcome = RetireAlreadyEnded
		res.Note = fmt.Sprintf("session %s already ended as %q; nothing to retire", sess.ID, sess.State)
		return res, nil
	}
	if sess.EnginePID > 0 && process.Alive(sess.EnginePID) && process.IsEngine(sess.EnginePID, sess.EngineIdentity) {
		// Not an error: cancel is the verb for a live engine, and the note says so.
		res.Outcome = RetireEngineAlive
		res.Note = fmt.Sprintf("engine pid %d is still running; cancel it instead - retire is for an engine that is gone", sess.EnginePID)
		return res, nil
	}

	actor := actorOrUnknown(sess)
	reason := fmt.Sprintf("retired by %s at %s: engine pid %d confirmed gone",
		actor, time.Now().UTC().Format(time.RFC3339), sess.EnginePID)

	// The running apply saves at every phase boundary; the update goes through
	// the lock so it cannot erase a save that landed in between.
	err = r.Store.Update(sessionID, func(sess *state.Session) error {
		sess.State = state.StateFailed
		sess.Phase = "retired"
		sess.Error = reason
		// An engine that died mid-copy usually left something. The objects stay
		// owned by this session, and marking the session terminal is what makes
		// them cleanable - the dry run is how the operator confirms each one.
		sess.MarkArtifactsSafe("leftover from an engine process that died; check with 'saksama cleanup " + sess.ID + " --dry-run' before dropping")
		r.recordEvent(sess, events.TypeSessionFailed, map[string]any{
			"reason": "retired", "actor": actor,
			"engine_pid": sess.EnginePID,
		})
		return nil
	})
	if err != nil {
		return res, err
	}

	if err := r.Store.Unlock(sess.Database, sess.Table, sess.ID); err == nil {
		res.LockReleased = true
	}

	res.Outcome = RetireRetired
	res.State = state.StateFailed
	res.Note = fmt.Sprintf(
		"session %s is now failed and its lock on %s.%s is released. Verify what the engine left "+
			"(saksama cleanup %s --dry-run), then re-plan; the schema fingerprint will tell whether "+
			"the change actually happened.", sess.ID, sess.Database, sess.Table, sess.ID)
	return res, nil
}
