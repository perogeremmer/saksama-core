package core

import (
	"encoding/json"
	"strings"

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

	// The session's LastSeq is only as fresh as its last save. A cancel issued
	// while the copy is running would otherwise number its event the same as one
	// the running process has emitted since, and both a follower's watermark and
	// a reconnecting viewer would then skip or duplicate an event. The log is the
	// authority; the session is a hint.
	start := sess.LastSeq
	if fromLog := lastSeqFromLog(r.Store, sess.ID); fromLog > start {
		start = fromLog
	}

	w := events.NewWriter(f, sess.ID).WithSeq(start)
	if _, err := w.Emit(typ, data); err != nil {
		return
	}
	sess.LastSeq = w.Seq()
}

// lastSeqFromLog reads the largest sequence number the log already carries.
// It is only consulted on the out-of-band path, where one append is worth a
// scan; the in-band writer counts in memory.
func lastSeqFromLog(store *state.Store, id string) int64 {
	data, err := store.ReadEvents(id)
	if err != nil {
		return 0
	}
	var last int64
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev events.Event
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Seq > last {
			last = ev.Seq
		}
	}
	return last
}
