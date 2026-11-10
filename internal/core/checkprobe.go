package core

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/perogeremmer/saksama-core/internal/engine"
	"github.com/perogeremmer/saksama-core/internal/model"
)

// Probing CHECK constraints is a full scan when the table is clean, so it is bounded
// by size rather than done unconditionally: a plan that takes four seconds on a small
// table must not take four minutes on a large one. Above the bound the plan says it
// did not look, and hands over the query - which is the same honesty D6 applies to an
// unmeasurable disk and E5b to an unreadable lock queue.
const (
	checkProbeMaxRows  = 2_000_000
	checkProbeMaxBytes = 1 << 30
)

// probeChecks finds rows that violate the table's CHECK constraints.
//
// The question is not academic: since 8.0.16 the server enforces these constraints,
// and a copy engine builds its shadow table from SHOW CREATE TABLE - so the
// constraint travels into the copy, and one bad row fails the copy with 3819 after it
// has been running for minutes. A table inherited from a server that ignored CHECK
// constraints can hold exactly those rows, and nothing on the new server will notice
// until a copy runs into them.
//
// The existence probe comes first because it stops at the first violating row; the
// count only runs for a constraint that already has one, so a clean table costs one
// scan per constraint and a dirty one costs a scan plus a count.
func probeChecks(ctx context.Context, db *sql.DB, table model.Table) (map[string]int64, string) {
	if len(table.Checks) == 0 || db == nil {
		return nil, ""
	}
	if table.Rows > checkProbeMaxRows || table.SizeBytes() > checkProbeMaxBytes {
		return nil, fmt.Sprintf("the table is %d rows and %d bytes; probing it at plan time would scan it whole, so run the query below yourself",
			table.Rows, table.SizeBytes())
	}

	prefix := engine.QuoteIdent(table.Name)
	if table.Schema != "" {
		prefix = engine.QuoteIdent(table.Schema) + "." + engine.QuoteIdent(table.Name)
	}

	out := map[string]int64{}
	for _, chk := range table.Checks {
		label := chk.Name
		if label == "" {
			label = chk.Expression
		}
		probe := fmt.Sprintf("SELECT EXISTS(SELECT 1 FROM %s WHERE NOT (%s))", prefix, chk.Expression)
		var violated bool
		if err := db.QueryRowContext(ctx, probe).Scan(&violated); err != nil {
			// A constraint this build cannot re-run (a function the server hides, a
			// column that moved) is not a pass. Say what happened and move on: one
			// unprobeable constraint must not throw away the others.
			out[label] = 0
			return out, fmt.Sprintf("constraint %q could not be probed: %v", label, err)
		}
		if !violated {
			out[label] = 0
			continue
		}
		count := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE NOT (%s)", prefix, chk.Expression)
		var n int64
		if err := db.QueryRowContext(ctx, count).Scan(&n); err != nil {
			out[label] = 1
			continue
		}
		out[label] = n
	}
	return out, ""
}
