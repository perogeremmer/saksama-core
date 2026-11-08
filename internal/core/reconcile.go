package core

import (
	"context"
	"fmt"
	"sort"

	"github.com/perogeremmer/saksama-core/internal/engine/process"
	"github.com/perogeremmer/saksama-core/internal/introspect"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// Finding is one observation about a session that did not end cleanly.
type Finding struct {
	Session string      `json:"session"`
	Table   string      `json:"table"`
	State   state.State `json:"state"`
	// EngineAlive reports whether the recorded engine process is still running.
	EngineAlive bool `json:"engine_alive"`
	// Observed lists what the database shows for the artifacts this session owns.
	Observed []string `json:"observed,omitempty"`
	// Verdict is what a person should do next.
	Verdict string `json:"verdict"`
	// Actionable is true when `cleanup` can be used as the next step.
	Actionable bool `json:"actionable"`
}

// Reconcile compares recorded sessions against reality.
//
// The point is honesty after a crash or a killed client: a session that never
// reached a terminal state is not obviously failed, and it is not obviously
// running. It has to be observed before anyone touches it.
func (r *Runner) Reconcile(ctx context.Context) ([]Finding, error) {
	sessions, err := r.Store.List()
	if err != nil {
		return nil, err
	}
	return r.reconcileSessions(ctx, sessions, r.objectExists(ctx)), nil
}

// objectExists reports whether one recorded artifact is still in the database.
// It is a function so the state machine can be tested without a server.
func (r *Runner) objectExists(ctx context.Context) func(*state.Session, state.ArtifactRecord) bool {
	if r.DB == nil {
		return func(*state.Session, state.ArtifactRecord) bool { return false }
	}
	return func(sess *state.Session, a state.ArtifactRecord) bool {
		switch a.Kind {
		case "table":
			return introspect.NewCollector(r.DB, sess.Database, sess.Table).TableExists(ctx, sess.Database, a.Name)
		case "trigger":
			return introspect.NewCollector(r.DB, sess.Database, sess.Table).TriggerExists(ctx, sess.Database, a.Name)
		}
		return false
	}
}

// reconcileSessions is the observation loop, with the database lookup injected.
func (r *Runner) reconcileSessions(ctx context.Context, sessions []*state.Session, exists func(*state.Session, state.ArtifactRecord) bool) []Finding {
	var out []Finding
	for _, sess := range sessions {
		// A finished session is not the end of the story. An engine that was killed
		// mid-copy is recorded as failed, so it is "terminal" while its shadow table
		// and changelog are still on the primary: pt-osc's triggers keep firing on
		// every write, and gh-ost's changelog keeps taking space. Reporting only
		// unfinished sessions would let reconcile say "nothing to do" while the
		// database is still carrying the debris of the last incident.
		terminal := sess.State.Terminal()

		f := Finding{
			Session: sess.ID,
			Table:   sess.Database + "." + sess.Table,
			State:   sess.State,
			// Alive means the number is in use; ours means it still answers to the
			// command line that was recorded. A reused pid is not the engine.
			EngineAlive: process.Alive(sess.EnginePID) && process.IsEngine(sess.EnginePID, sess.EngineIdentity),
		}

		// What does the database actually show for the objects this session owns?
		// A gate is a local file, not a database object, so it is not reported here.
		for _, a := range sess.Artifacts {
			if a.Kind == "gate" {
				continue
			}
			if exists(sess, a) {
				f.Observed = append(f.Observed, a.Kind+" "+a.Name)
			}
		}
		sort.Strings(f.Observed)

		switch {
		case terminal && len(f.Observed) == 0:
			// Finished, and nothing remains: the normal case, and the reason
			// reconcile stays quiet after a clean run.
			continue
		case terminal:
			f.Verdict = fmt.Sprintf(
				"the migration ended as %q, but %d object(s) from it are still here; cleanup drops what it owns",
				sess.State, len(f.Observed))
			f.Actionable = true
		case f.EngineAlive:
			f.Verdict = fmt.Sprintf("engine pid %d is still running; attach to it, or cancel it", sess.EnginePID)
		case len(f.Observed) > 0:
			f.Verdict = "the engine is gone but left objects behind; check the table, then clean up"
			f.Actionable = true
		default:
			f.Verdict = "the engine is gone and nothing is left behind; mark it failed and re-plan"
			f.Actionable = true
		}

		out = append(out, f)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Session < out[j].Session })
	return out
}
