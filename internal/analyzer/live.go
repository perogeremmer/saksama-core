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
	ThreadsRunning   int
	LongQuerySeconds int64 // age of the oldest non-sleeping statement
	LongQueryCount   int
	LongestQuerySQL  string
	LongestQueryID   int64
	LongTransactions int
	OldestTrxSeconds int64
	PendingMDLocks   int
	// MDLObservable reports whether the lock queue could be read at all. A zero
	// that was never observed is not a pass.
	MDLObservable     bool
	ReplicaConfigured bool
	ReplicaRunning    bool
	ReplicaLagSeconds float64

	Grants Grants

	LeftoverArtifacts []string

	// FKCompatibility reports whether both sides of each foreign key agree on
	// charset and collation. Empty when it could not be verified.
	FKCompatibility []FKCompat

	// Duplicates maps an index name to the number of duplicate groups found.
	// Absent keys mean "not checked".
	Duplicates map[string]int64
}

// mdlUnavailableByDesign reports whether this server version simply has no
// metadata lock table, as opposed to hiding it behind a privilege.
func mdlUnavailableByDesign(srv model.Server) bool {
	v, err := model.ParseVersion(srv.Version)
	if err != nil {
		return false
	}
	return v.Major < 5 || (v.Major == 5 && v.Minor <= 6)
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
	// Verified is false when nothing was compared - an unreadable peer table, or
	// no text column pair to compare. A check that was never run is a warning,
	// not an affirmation.
	Verified bool
}

// The queries a remedy needs to be followable. Each was run against a real server
// before being printed here: an action line with a broken query in it is worse than
// no action line, because it costs the reader the time to find that out.
const (
	trxQuery = "SELECT trx_id, trx_started, trx_mysql_thread_id, LEFT(trx_query, 60) " +
		"FROM information_schema.innodb_trx ORDER BY trx_started"
	loadQuery   = "SHOW GLOBAL STATUS LIKE 'Threads_running'"
	lagQuery    = "SHOW SLAVE STATUS\\G   -- Seconds_Behind_Master, Slave_SQL_Running"
	grantsQuery = "SHOW GRANTS FOR CURRENT_USER()"
	dupQuery    = "SELECT COUNT(*) AS duplicate_groups FROM (SELECT 1 FROM <table> " +
		"GROUP BY <columns> HAVING COUNT(*) > 1) d"
)

// mdlChainHelp is the one thing an operator needs when a plan says the lock queue is
// not empty or a statement is holding it: who holds what, and which statement is
// waiting behind it. Spelled out instead of described, because "look at the metadata
// locks" is an instruction that arrives while everything is already on fire.
const mdlChainHelp = "\n" +
	"see who holds what, and who is waiting:" +
	"\nSELECT t.PROCESSLIST_ID, t.PROCESSLIST_TIME, l.OBJECT_SCHEMA, l.OBJECT_NAME," +
	"\n       l.LOCK_TYPE, l.LOCK_STATUS, LEFT(t.PROCESSLIST_INFO, 60) AS waiting_sql" +
	"\n  FROM performance_schema.metadata_locks l" +
	"\n  JOIN performance_schema.threads t ON t.THREAD_ID = l.OWNER_THREAD_ID" +
	"\n WHERE l.OBJECT_SCHEMA NOT IN ('performance_schema','mysql','information_schema')" +
	"\n ORDER BY l.LOCK_STATUS DESC, t.PROCESSLIST_TIME DESC"

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
			Remediation: "wait for it to finish, or terminate it deliberately; DDL queues behind an open metadata lock and blocks every other query." + mdlChainHelp,
			MySQLError:  1205,
		})
	}

	// L2: pending metadata locks.
	switch {
	case L.MDLObservable && L.PendingMDLocks > 0:
		out = append(out, Check{
			ID: "D1b", Severity: SeverityBlocker,
			Title:       "metadata locks are already pending",
			Detail:      fmt.Sprintf("%d metadata lock request(s) are waiting; a DDL statement would enqueue behind them", L.PendingMDLocks),
			Remediation: "resolve the queue before adding to it: the waiting DDL already blocks every query behind it, and one more statement makes the outage worse. Kill the queued DDL first if it is stale - the long SELECT underneath is usually the innocent party." + mdlChainHelp,
			MySQLError:  1205,
		})
	case !L.MDLObservable && !mdlUnavailableByDesign(in.Server):
		// The server claims a version that has the table, so this is a privilege
		// or configuration problem, not an old server. Either way, nobody looked.
		out = append(out, Check{
			ID: "D1b", Severity: SeverityWarn,
			Title:       "the metadata lock queue could not be read",
			Detail:      "performance_schema.metadata_locks was not readable, so a pending lock queue would not show up in this plan",
			Remediation: "grant SELECT on performance_schema to this user, or confirm the queue is empty by hand before applying",
			MySQLError:  1142,
		})
	}

	// L3: open long transactions.
	if L.LongTransactions > 0 {
		out = append(out, Check{
			ID: "D3", Severity: SeverityWarn,
			Title:  "long-running transaction is open",
			Detail: fmt.Sprintf("%d transaction(s) older than the threshold; the oldest is %ds", L.LongTransactions, L.OldestTrxSeconds),
			Remediation: "an open transaction keeps row locks and old row versions, and a copy-based migration waits behind it; commit or roll it back first. Who is holding what:" +
				"\n" + trxQuery + "\n  then KILL <trx_mysql_thread_id> if that session is stale",
		})
	}

	// L4: current server load.
	switch {
	case L.ThreadsRunning >= 100:
		out = append(out, Check{
			ID: "D4", Severity: SeverityWarn,
			Title:  "server is already heavily loaded",
			Detail: fmt.Sprintf("Threads_running is %d", L.ThreadsRunning),
			Remediation: "a copy engine throttles to protect the server and may never catch up while this lasts; run it when the server is quieter. How loaded is it right now:" +
				"\n" + loadQuery + "\n  the plan also accepts --max-load if you want the copy to tolerate more",
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
				Title:  "replica is configured but not running",
				Detail: "replication threads are stopped",
				Remediation: "fix replication before planning again: gh-ost reads from a replica and refuses to start otherwise. What the replica says:" +
					"\n" + lagQuery,
			})
		case L.ReplicaLagSeconds >= 30:
			out = append(out, Check{
				ID: "D5", Severity: SeverityBlocker,
				Title:  "replica lag is already high",
				Detail: fmt.Sprintf("%.1fs behind", L.ReplicaLagSeconds),
				Remediation: "a copy-based migration would push lag further, so drain it first: wait until Seconds_Behind_Master is small and Slave_SQL_Running is Yes, then plan again. What the replica says:" +
					"\n" + lagQuery,
			})
		case L.ReplicaLagSeconds >= 5:
			out = append(out, Check{
				ID: "D5", Severity: SeverityWarn,
				Title:  "replica lag is elevated",
				Detail: fmt.Sprintf("%.1fs behind", L.ReplicaLagSeconds),
				Remediation: "throttling will engage early and the copy will crawl; wait for the lag to drain, then plan again:" +
					"\n" + lagQuery,
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
		// Order matters: an unverified check is a warning whatever its flag says,
		// a verified pass is info, and only a verified mismatch is a blocker.
		if !fk.Verified {
			out = append(out, Check{
				ID: "A1", Severity: SeverityWarn,
				Title:  "foreign key charset could not be verified",
				Detail: fmt.Sprintf("constraint %q (%s %s): %s", fk.Constraint, fk.Direction, fk.PeerTable, fk.Description),
				Remediation: "verify it yourself: " +
					"SELECT TABLE_NAME, COLUMN_NAME, CHARACTER_SET_NAME, COLLATION_NAME FROM information_schema.COLUMNS" +
					" WHERE TABLE_NAME IN ('<fk_table>', '<parent_table>')",
			})
			continue
		}
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
			Title:  "foreign key columns disagree on charset or collation",
			Detail: fmt.Sprintf("constraint %q (%s %s): %s", fk.Constraint, fk.Direction, fk.PeerTable, fk.Description),
			Remediation: "align charset and collation before anything recreates the constraint, or MySQL fails the ALTER with error 1215/1832. Which side is which:" +
				"\nSELECT TABLE_NAME, COLUMN_NAME, CHARACTER_SET_NAME, COLLATION_NAME FROM information_schema.COLUMNS" +
				"\n WHERE TABLE_SCHEMA = DATABASE() AND COLUMN_NAME IN ('<fk_column>', '<parent_column>')",
			MySQLError: 3780,
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
				Title:  "unique index would fail: duplicate values exist",
				Detail: fmt.Sprintf("index %q has at least %d duplicate group(s)", name, n),
				Remediation: "deduplicate before adding the index, or make it non-unique. How many groups collide:" +
					"\n" + dupQuery,
				MySQLError: 1062,
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
			Title:  "privileges could not be read",
			Detail: "SHOW GRANTS FOR CURRENT_USER() failed or returned nothing",
			Remediation: "the plan could not read this account's grants, so it cannot tell whether the chosen path will work; check it before applying:" +
				"\n" + grantsQuery + "\n  native needs ALTER, CREATE, INDEX, and SELECT on the table; gh-ost adds REPLICATION CLIENT/SLAVE, SUPER or SESSION_VARIABLES_ADMIN, and TRIGGER",
		}}
	}

	var out []Check

	// session variable changes are needed for a short lock_wait_timeout.
	if !g.Super && !g.SessionVarsAdmin && !g.AllPrivileges {
		out = append(out, Check{
			ID: "D15", Severity: SeverityWarn,
			Title:       "account may not be able to shorten lock_wait_timeout",
			Detail:      fmt.Sprintf("user %s lacks SUPER and SESSION_VARIABLES_ADMIN", g.User),
			Remediation: "Saksama cannot shorten lock_wait_timeout with this account, so a DDL it runs may queue behind a metadata lock for the server default (up to a day). Either grant it: GRANT SESSION_VARIABLES_ADMIN ON *.* TO '<user>'@'<host>'; on RDS use rds_superuser - or set a short wait for the session yourself with SET SESSION lock_wait_timeout = 10 before the ALTER",
			MySQLError:  1227,
		})
	}

	// PROCESS is needed to see other sessions; that is what D1 relies on.
	if !g.Process && !g.AllPrivileges {
		out = append(out, Check{
			ID: "D1c", Severity: SeverityWarn,
			Title:  "account cannot inspect other sessions",
			Detail: fmt.Sprintf("user %s lacks PROCESS, so long-running statements by other users are invisible", g.User),
			Remediation: "without PROCESS this tool cannot see other sessions, so a lock queue is invisible rather than absent. Grant it:" +
				"\nGRANT PROCESS ON *.* TO '<user>'@'<host>'; FLUSH PRIVILEGES;" +
				"\n  then plan again - D1 and D1b can finally answer",
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
