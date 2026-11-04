package core

import (
	"github.com/perogeremmer/saksama-core/internal/events"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// recordEvent appends one event to a session's log, continuing its sequence.
//
// Some actions happen outside the process that started the migration: `cancel`
// from another ssh session, a reconciliation after a crash, an operator releasing
// a cutover. They still belong in the same audit trail, so the writer picks up the
// last sequence number rather than starting at one.
//
// Failures are deliberately swallowed: an action that succeeded must not be
// reported as failed because the log could not be appended to.
func (r *Runner) recordEvent(sess *state.Session, typ string, data map[string]any) {
	if r.Store == nil || sess == nil {
		return
	}
	f, err := r.Store.OpenEventLog(sess.ID)
	if err != nil {
		return
	}
	defer f.Close()

	w := events.NewWriter(f, sess.ID).WithSeq(sess.LastSeq)
	if _, err := w.Emit(typ, data); err != nil {
		return
	}
	sess.LastSeq = w.Seq()
}
