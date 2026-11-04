//go:build integration

package integration

import (
	"context"
	"database/sql"
	"io"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/engine"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// dropArtifactsForTest mirrors the cleanup command's delete path so the integration
// suite can assert it against a real server.
func dropArtifactsForTest(t *testing.T, db *sql.DB, database string, sess *state.Session) ([]string, int) {
	t.Helper()
	var dropped []string
	var failed int
	for _, a := range sess.DeletableArtifacts() {
		var stmt string
		switch a.Kind {
		case "table":
			stmt = "DROP TABLE IF EXISTS " + engine.QuoteIdent(database) + "." + engine.QuoteIdent(a.Name)
		case "trigger":
			stmt = "DROP TRIGGER IF EXISTS " + engine.QuoteIdent(database) + "." + engine.QuoteIdent(a.Name)
		default:
			failed++
			continue
		}
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			failed++
			continue
		}
		dropped = append(dropped, a.Name)
	}
	_ = io.Discard
	return dropped, failed
}
