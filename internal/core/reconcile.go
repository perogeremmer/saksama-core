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

	var out []Finding
	for _, sess := range sessions {
		if sess.State.Terminal() {
			continue
		}

		f := Finding{
			Session:     sess.ID,
			Table:       sess.Database + "." + sess.Table,
			State:       sess.State,
			EngineAlive: process.Alive(sess.EnginePID),
		}

		// What does the database actually show for the objects this session owns?
		if len(sess.Artifacts) > 0 {
			collector := introspect.NewCollector(r.DB, sess.Database, sess.Table)
			for _, a := range sess.Artifacts {
				exists := false
				switch a.Kind {
				case "table":
					exists = collector.TableExists(ctx, sess.Database, a.Name)
				case "trigger":
					exists = collector.TriggerExists(ctx, sess.Database, a.Name)
				case "gate":
					exists = true // a gate file is a local file, not a database object
				}
				if exists && a.Kind != "gate" {
					f.Observed = append(f.Observed, a.Kind+" "+a.Name)
				}
			}
		}
		sort.Strings(f.Observed)

		switch {
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
	return out, nil
}
