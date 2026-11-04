package analyzer

import (
	"fmt"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// LiveState is the observation collected from a real server. A nil LiveState
// means the plan is offline, and every live-only check reports that it could not
// be evaluated instead of silently passing.
type LiveState struct {
	ThreadsRunning    int
	LongQuerySeconds  int64 // age of the oldest non-sleeping statement
	LongQueryCount    int
	LongestQuerySQL   string
	LongestQueryID    int64
	LongTransactions  int
	OldestTrxSeconds  int64
	PendingMDLocks    int
	ReplicaConfigured bool
	ReplicaRunning    bool
	ReplicaLagSeconds float64

	Grants Grants

	// FreeDiskBytes is the measured free space on the data volume. 0 means "not
	// measured", which is different from "no space".
	FreeDiskBytes int64

	LeftoverArtifacts []string

	// FKCompatibility reports whether both sides of each foreign key agree on
	// charset and collation. Empty when it could not be verified.
	FKCompatibility []FKCompat

	// Duplicates maps an index name to the number of duplicate groups found.
	// Absent keys mean "not checked".
	Duplicates map[string]int64
}

// Grants is the privilege picture that matters for DDL and copy engines.
type Grants struct {
	User             string
	Super            bool
	Process          bool
	Reload           bool
	SystemVarsAdmin  bool
	SessionVarsAdmin bool
	AllPrivileges    bool
	Known            bool // false when SHOW GRANTS could not be read
}

// FKCompat is a per-constraint charset verdict.
type FKCompat struct {
	Constraint  string
	Direction   string
	PeerTable   string
	Compatible  bool
	Description string
}

// liveChecks emits the checks that require a database connection.
func liveChecks(in Input, projected model.Table) []Check {
	var out []Check

	if in.Live == nil {
		out = append(out, Check{
			ID: "L0", Severity: SeverityInfo,
			Title:       "planning offline",
			Detail:      "no live server state was collected, so load, lock, privilege, and charset-compatibility checks could not run",
			Remediation: "run against the server (--live) before applying; an offline plan is a CI gate, not an execution approval",
		})
		return out
	}

	L := in.Live

	// L1: metadata lock hazard from a long-running statement.
	if L.LongQueryCount > 0 {
		sev := SeverityWarn
		detail := fmt.Sprintf("%d statement(s) running longer than the threshold; the oldest is %ds", L.LongQueryCount, L.LongQuerySeconds)
		if L.LongQuerySeconds >= 300 {
			sev = SeverityBlocker
			detail += " and will almost certainly hold the metadata lock"
		}
		out = append(out, Check{
			ID: "D1", Severity: sev,
			Title:       "long-running statement may hold the metadata lock",
			Detail:      detail + sqlSuffix(L.LongestQuerySQL),
			Remediation: "wait for it to finish, or terminate it deliberately; DDL queues behind an open metadata lock and blocks every other query",
			MySQLError:  1205,
		})
	}

	// L2: pending metadata locks.
	if L.PendingMDLocks > 0 {
		out = append(out, Check{
			ID: "D1b", Severity: SeverityBlocker,
			Title:       "metadata locks are already pending",
			Detail:      fmt.Sprintf("%d metadata lock request(s) are waiting; a DDL statement would enqueue behind them", L.PendingMDLocks),
			Remediation: "resolve the existing lock queue first; adding this migration now would extend the outage",
			MySQLError:  1205,
		})
	}

	// L3: open long transactions.
	if L.LongTransactions > 0 {
		out = append(out, Check{
			ID: "D3", Severity: SeverityWarn,
			Title:       "long-running transaction is open",
			Detail:      fmt.Sprintf("%d transaction(s) older than the threshold; the oldest is %ds", L.LongTransactions, L.OldestTrxSeconds),
			Remediation: "an open transaction keeps row locks and old row versions; commit or roll back before a copy-based migration",
		})
	}

	// L4: current server load.
	switch {
	case L.ThreadsRunning >= 100:
		out = append(out, Check{
			ID: "D4", Severity: SeverityWarn,
			Title:       "server is already heavily loaded",
			Detail:      fmt.Sprintf("Threads_running is %d", L.ThreadsRunning),
			Remediation: "a copy engine will throttle immediately and may never catch up; schedule a quieter window",
		})
	case L.ThreadsRunning >= 50:
		out = append(out, Check{
			ID: "D4", Severity: SeverityInfo,
			Title:       "server load is elevated",
			Detail:      fmt.Sprintf("Threads_running is %d", L.ThreadsRunning),
			Remediation: "expect throttling",
		})
	}

	// L5: replica state.
	if L.ReplicaConfigured {
		switch {
		case !L.ReplicaRunning:
			out = append(out, Check{
				ID: "D5", Severity: SeverityBlocker,
				Title:       "replica is configured but not running",
				Detail:      "replication threads are stopped",
				Remediation: "fix replication first; gh-ost reads from a replica and will refuse to start otherwise",
			})
		case L.ReplicaLagSeconds >= 30:
			out = append(out, Check{
				ID: "D5", Severity: SeverityBlocker,
				Title:       "replica lag is already high",
				Detail:      fmt.Sprintf("%.1fs behind", L.ReplicaLagSeconds),
				Remediation: "a copy-based migration would push lag further; resolve lag before starting",
			})
		case L.ReplicaLagSeconds >= 5:
			out = append(out, Check{
				ID: "D5", Severity: SeverityWarn,
				Title:       "replica lag is elevated",
				Detail:      fmt.Sprintf("%.1fs behind", L.ReplicaLagSeconds),
				Remediation: "throttling will engage early",
			})
		}
	}

	// L6: leftover artifacts from a previous attempt.
	if len(L.LeftoverArtifacts) > 0 {
		out = append(out, Check{
			ID: "D8", Severity: SeverityBlocker,
			Title:       "leftover artifacts from a previous migration",
			Detail:      strings.Join(L.LeftoverArtifacts, ", "),
			Remediation: "run 'saksama cleanup' after verifying ownership, or resume that session; starting now would collide with it",
		})
	}

	// L7: verified foreign key charset compatibility (replaces offline A1).
	for _, fk := range L.FKCompatibility {
		if fk.Compatible {
			out = append(out, Check{
				ID: "A1", Severity: SeverityInfo,
				Title:  "foreign key charset and collation agree",
				Detail: fmt.Sprintf("constraint %q (%s %s) is compatible", fk.Constraint, fk.Direction, fk.PeerTable),
			})
			continue
		}
		out = append(out, Check{
			ID: "A1", Severity: SeverityBlocker,
			Title:       "foreign key columns disagree on charset or collation",
			Detail:      fmt.Sprintf("constraint %q (%s %s): %s", fk.Constraint, fk.Direction, fk.PeerTable, fk.Description),
			Remediation: "align charset and collation on both sides before any operation that recreates the constraint",
			MySQLError:  3780,
		})
	}

	// L8: verified duplicate probe for unique indexes this ALTER adds.
	//
	// Only planned indexes are probed: MySQL cannot hold duplicates in an index
	// that already exists, so probing those would be dead weight.
	for _, op := range in.Ops {
		if op.Kind != model.OpAddUniqueIndex || L.Duplicates == nil {
			continue
		}
		name := op.IndexName
		if name == "" && len(op.Parts) > 0 {
			name = op.Parts[0].Column
		}
		if n, ok := L.Duplicates[name]; ok && n > 0 {
			out = append(out, Check{
				ID: "B5", Severity: SeverityBlocker,
				Title:       "unique index would fail: duplicate values exist",
				Detail:      fmt.Sprintf("index %q has at least %d duplicate group(s)", name, n),
				Remediation: "deduplicate the columns first, or use a non-unique index",
				MySQLError:  1062,
			})
		}
	}

	// L9: privileges needed by the planned path.
	out = append(out, privilegeChecks(L.Grants, in.Table, projected)...)

	// L10: partition warning is emitted offline too; nothing to add here.

	return out
}

// privilegeChecks reports the grants a chosen execution path needs.
func privilegeChecks(g Grants, tbl, projected model.Table) []Check {
	if !g.Known {
		return []Check{{
			ID: "D15", Severity: SeverityInfo,
			Title:       "privileges could not be read",
			Detail:      "SHOW GRANTS FOR CURRENT_USER() failed or returned nothing",
			Remediation: "verify the account has the privileges the chosen path needs",
		}}
	}

	var out []Check

	// session variable changes are needed for a short lock_wait_timeout.
	if !g.Super && !g.SessionVarsAdmin && !g.AllPrivileges {
		out = append(out, Check{
			ID: "D15", Severity: SeverityWarn,
			Title:       "account may not be able to shorten lock_wait_timeout",
			Detail:      fmt.Sprintf("user %s lacks SUPER and SESSION_VARIABLES_ADMIN", g.User),
			Remediation: "grant SESSION_VARIABLES_ADMIN (or rds_superuser on RDS) so DDL can fail fast instead of queueing",
			MySQLError:  1227,
		})
	}

	// PROCESS is needed to see other sessions; that is what D1 relies on.
	if !g.Process && !g.AllPrivileges {
		out = append(out, Check{
			ID: "D1c", Severity: SeverityWarn,
			Title:       "account cannot inspect other sessions",
			Detail:      fmt.Sprintf("user %s lacks PROCESS, so long-running statements by other users are invisible", g.User),
			Remediation: "grant PROCESS so the metadata-lock hazard can actually be detected",
		})
	}

	return out
}

func sqlSuffix(sql string) string {
	if sql == "" {
		return ""
	}
	return fmt.Sprintf("; oldest statement: %s", sql)
}
