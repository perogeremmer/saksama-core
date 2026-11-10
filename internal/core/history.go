package core

import (
	"bytes"
	"encoding/json"
	"sort"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/events"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// maxObservations caps what is read from a long-lived state directory. A machine
// that has run a hundred migrations should not spend the tenth of a second to
// re-read all of them to estimate a duration, and the estimator only needs a
// spread - the most recent runs describe the machine as it is now, not as it was
// a year ago.
const maxObservations = 20

// Observations reduces what this state directory has already measured to the two
// numbers a duration estimate needs: how much data, and how long it took.
//
// Nothing new is recorded here. Every session already stores the plan it was
// decided with (the plan carries the table size the migration actually faced), and
// every run already writes an append-only event log whose entries carry
// timestamps. So a state directory that has been in use for months starts
// answering immediately, and the answers get narrower with use.
//
// Only completed runs count. A cancelled or killed migration measured the copy up
// to the point it stopped, which is a different number wearing the same name - and
// feeding it into an estimate would make the tool quietly worse at the moment an
// operator most needs it to be honest.
func Observations(store *state.Store) []analyzer.Observation {
	if store == nil {
		return nil
	}
	sessions, err := store.List()
	if err != nil {
		return nil
	}

	// List returns newest first; sort explicitly so this does not depend on that.
	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].CreatedAt > sessions[j].CreatedAt
	})

	out := make([]analyzer.Observation, 0, maxObservations)
	for _, sess := range sessions {
		if len(out) >= maxObservations {
			break
		}
		if sess.State != state.StateDone {
			continue
		}
		engine := analyzer.Engine(sess.Engine)
		switch engine {
		case analyzer.EngineNative, analyzer.EngineGhOst, analyzer.EnginePtOSC:
		default:
			continue
		}
		size := plannedTableBytes(sess.Plan)
		if size <= 0 {
			continue
		}
		seconds := executionSeconds(store, sess.ID)
		if seconds <= 0 {
			continue
		}
		out = append(out, analyzer.Observation{
			Engine:    engine,
			DataBytes: size,
			Seconds:   seconds,
			At:        sess.CreatedAt,
		})
	}
	return out
}

// plannedTableBytes reads the size the plan was made for. A session whose plan is
// missing or was written by an older format yields zero, and is skipped: guessing
// the size would defeat the point of measuring.
func plannedTableBytes(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var stored struct {
		Schema analyzer.SchemaInfo `json:"schema"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return 0
	}
	// Data plus indexes: the copy engines move both, and an index build rebuilds
	// the secondary indexes as it goes.
	return stored.Schema.DataBytes + stored.Schema.IndexBytes
}

// executionSeconds is how long the change itself took, from the event log: the
// ddl phase opens immediately before the server or the engine is handed the work
// and closes when that work is over. Verification, preflight, and the time spent
// waiting for an operator at a cutover gate are outside it, which is what makes
// the throughput usable for the next copy.
//
// A log that does not contain the pair (a run from an older format, a truncated
// log, a crashed run) yields zero and is skipped rather than guessed at.
func executionSeconds(store *state.Store, id string) int {
	data, err := store.ReadEvents(id)
	if err != nil {
		return 0
	}
	var start, end float64
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var ev struct {
			TS   float64 `json:"ts"`
			Type string  `json:"type"`
			Data struct {
				Phase string `json:"phase"`
			} `json:"data"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Data.Phase != "ddl" {
			continue
		}
		switch ev.Type {
		case events.TypePhaseStarted:
			if start == 0 {
				start = ev.TS
			}
		case events.TypePhaseCompleted, events.TypePhaseFailed:
			end = ev.TS
		}
	}
	if start <= 0 || end <= start {
		return 0
	}
	// Round to the nearest second: the estimate that consumes this is expressed in
	// seconds, and a sub-second migration has no throughput worth measuring.
	sec := int(end - start + 0.5)
	if sec < 1 {
		sec = 1
	}
	return sec
}
