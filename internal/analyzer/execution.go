package analyzer

import (
	"fmt"
	"strconv"
	"strings"
)

// checksExecution reports what only matters once the engine is known.
//
// The rest of the catalogue is about the table and the server, so it can be
// computed before routing. This one is about the run itself: it asks whether the
// engine that was actually chosen has everything it needs to finish, which is a
// question nobody can answer until the engine exists.
func checksExecution(in Input, engine Engine) []Check {
	var out []Check

	// D13: an in-place rebuild accumulates concurrent DML in the online alter log.
	// When the log is full, the server does not slow down - it fails the statement
	// with 1799 and rolls back everything it has done, which is how a rebuild that
	// ran for an hour ends with nothing to show for it.
	//
	// The log only exists for a server-side in-place rebuild. A copy engine writes
	// its own table and needs none of this, and an INSTANT operation does not touch
	// rows at all, so both are exempt.
	if engine == EngineNative && !allInstant(in.Ops) {
		if size := in.Table.SizeBytes(); size > 0 {
			// Size stands in for duration, and it is said out loud rather than
			// hidden: an hour of writes into a 128 MB log overflows it, and there is
			// no write-rate figure to divide by.
			if size >= 8<<30 {
				if raw, known := in.Settings.Get("innodb_online_alter_log_max_size"); known {
					if logBytes, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && logBytes > 0 {
						if logBytes <= 256<<20 {
							out = append(out, Check{
								ID: chkOnlineAlterLog, Severity: SeverityWarn,
								Title:  "online alter log is small for a rebuild this size",
								Detail: fmt.Sprintf("innodb_online_alter_log_max_size is %s and the table is %s; a rebuild that takes minutes collects every concurrent write in that log", humanBytes(logBytes), humanBytes(size)),
								// The size is a proxy for duration, and the remediation
								// says so: raise the ceiling or take the engine that
								// does not need this log at all.
								Remediation: fmt.Sprintf("raise it before the statement (it is dynamic, and new statements pick it up): SET GLOBAL innodb_online_alter_log_max_size = %d; or plan a copy engine, which keeps its own table and never touches this log. If the log does fill, the server fails with 1799 and rolls the whole ALTER back", 1<<30),
								MySQLError:  1799,
							})
						}
					}
				} else if in.Settings.Known {
					out = append(out, Check{
						ID: chkOnlineAlterLog, Severity: SeverityInfo,
						Title:       "online alter log could not be read",
						Detail:      "innodb_online_alter_log_max_size was not in the settings this build collected",
						Remediation: "check it by hand before a large rebuild: SHOW VARIABLES LIKE 'innodb_online_alter_log_max_size'. The default is 128 MB, and an hour of writes overflows it with 1799 and a rollback",
						MySQLError:  1799,
					})
				}
			}
		}
	}

	return out
}
