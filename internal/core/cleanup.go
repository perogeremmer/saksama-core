package core

import (
	"context"
	"fmt"

	"github.com/perogeremmer/saksama-core/internal/engine"
	"github.com/perogeremmer/saksama-core/internal/events"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// ArtifactOutcome is one recorded object and what happened to it.
type ArtifactOutcome struct {
	state.ArtifactRecord
	Dropped bool   `json:"dropped"`
	Err     string `json:"error,omitempty"`
}

// CleanupResult is what cleanup did, or what it would do.
type CleanupResult struct {
	ID     string            `json:"id"`
	DryRun bool              `json:"dry_run"`
	Items  []ArtifactOutcome `json:"items,omitempty"`
	// Refused explains why nothing was attempted. It is not an error: refusing to
	// drop objects a recovery still needs is the correct behaviour.
	Refused string `json:"refused,omitempty"`
}

// Cleanup drops the objects a session recorded as its own.
//
// Ownership comes from the session, never from a name pattern: a table called
// `_orders_old` might be this migration's debris, or something a person created on
// purpose, and the name cannot tell you which.
func (r *Runner) Cleanup(ctx context.Context, sessionID string, dryRun bool) (CleanupResult, error) {
	if r.Store == nil {
		return CleanupResult{}, fmt.Errorf("no state directory")
	}
	sess, err := r.Store.Load(sessionID)
	if err != nil {
		return CleanupResult{}, err
	}

	res := CleanupResult{ID: sess.ID, DryRun: dryRun}

	if len(sess.Artifacts) == 0 {
		res.Refused = fmt.Sprintf(
			"session %s recorded no artifacts (engine %q creates none of its own); nothing to clean up",
			sess.ID, sess.Engine)
		return res, nil
	}

	deletable := sess.DeletableArtifacts()
	if len(deletable) == 0 {
		res.Refused = fmt.Sprintf(
			"session %s is still %s, so its artifacts may be needed for recovery; nothing was dropped",
			sess.ID, sess.State)
		return res, nil
	}

	for _, a := range deletable {
		res.Items = append(res.Items, ArtifactOutcome{ArtifactRecord: a})
	}

	if dryRun {
		for i := range res.Items {
			res.Items[i].Dropped = true
		}
		return res, nil
	}
	if r.DB == nil {
		return res, fmt.Errorf("no database connection")
	}

	dropped := 0
	for i := range res.Items {
		a := res.Items[i].ArtifactRecord
		stmt, err := dropStatement(sess.Database, a)
		if err != nil {
			res.Items[i].Err = err.Error()
			continue
		}
		if _, err := r.DB.ExecContext(ctx, stmt); err != nil {
			res.Items[i].Err = err.Error()
			continue
		}
		res.Items[i].Dropped = true
		dropped++
		sess.ForgetsArtifact(a.Kind, a.Name)
	}

	r.recordEvent(sess, events.TypeSessionState, map[string]any{
		"state": sess.State,
		"note":  fmt.Sprintf("cleanup dropped %d of %d artifact(s)", dropped, len(res.Items)),
		"actor": actorOrUnknown(sess),
	})
	if err := r.Store.Save(sess); err != nil {
		return res, err
	}
	return res, nil
}

// dropStatement builds the statement for one recorded object. An unknown kind is a
// refusal, not a guess: dropping the wrong thing cannot be undone.
func dropStatement(database string, a state.ArtifactRecord) (string, error) {
	switch a.Kind {
	case "table":
		return "DROP TABLE IF EXISTS " + engine.QuoteIdent(database) + "." + engine.QuoteIdent(a.Name), nil
	case "trigger":
		return "DROP TRIGGER IF EXISTS " + engine.QuoteIdent(database) + "." + engine.QuoteIdent(a.Name), nil
	}
	return "", fmt.Errorf("unknown artifact kind %q; refusing to drop %s", a.Kind, a.Name)
}
