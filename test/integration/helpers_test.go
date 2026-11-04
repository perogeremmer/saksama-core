//go:build integration

package integration

import (
	"context"
	"database/sql"
	"io"
	"strings"
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

// requireBinlog skips a test whose subject cannot happen on a server that
// gh-ost cannot start on. Binary logging is a server configuration, not a
// Saksama setting, and a 5.6 box with it off is a real and common production
// state: the honest answer there is pt-osc, not a failed assertion.
func requireBinlog(t *testing.T, db *sql.DB) {
	t.Helper()
	var name, value string
	if err := db.QueryRow("SHOW GLOBAL VARIABLES LIKE 'log_bin'").Scan(&name, &value); err != nil {
		t.Skipf("cannot read log_bin: %v", err)
	}
	if !strings.EqualFold(value, "ON") {
		t.Skip("log_bin is off on this server, so gh-ost cannot run here")
	}
}
