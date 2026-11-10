package introspect

import (
	"context"
	"database/sql"
	"strings"
)

// relevantVariables is what the engines and the safety rules actually care about.
//
// It is a deliberately short list: dumping 600 server variables is noise, and
// noise is how a real problem stays invisible.
var relevantVariables = []string{
	"log_bin",
	"binlog_format",
	"binlog_row_image",
	"binlog_expire_logs_seconds",
	"server_id",
	"gtid_mode",
	"read_only",
	"super_read_only",
	"performance_schema",
	"transaction_isolation",
	"tx_isolation", // MySQL 5.6 name for transaction_isolation
	"innodb_file_per_table",
	"innodb_autoinc_lock_mode",
	"lower_case_table_names",
	"sql_require_primary_key",
	"log_bin_trust_function_creators",
	"version",
	"version_comment",
	"hostname",
	"datadir",
	"tmpdir",
	"max_connections",
	"wait_timeout",
	"innodb_lock_wait_timeout",
	// D13: the ceiling on concurrent DML an in-place rebuild can absorb before it
	// fails with 1799 and rolls back.
	"innodb_online_alter_log_max_size",
}

// Settings reads the server variables the engines depend on.
//
// Missing variables are not an error: a variable added in a later MySQL version
// simply does not appear, and the check that needs it reports "unknown" rather
// than inventing a value.
func (c *Collector) Settings(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}

	rows, err := c.Q.QueryContext(ctx, `SHOW GLOBAL VARIABLES`)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	want := map[string]bool{}
	for _, v := range relevantVariables {
		want[v] = true
	}

	for rows.Next() {
		var name, value sql.NullString
		if err := rows.Scan(&name, &value); err != nil {
			return out, err
		}
		key := strings.ToLower(name.String)
		if want[key] {
			out[key] = value.String
		}
	}
	return normalizeSettings(out), rows.Err()
}

// normalizeSettings maps version-specific variable names onto one spelling, so a
// check means the same thing on every server. MySQL 5.6 calls
// transaction_isolation "tx_isolation".
func normalizeSettings(in map[string]string) map[string]string {
	if v, ok := in["tx_isolation"]; ok {
		if _, exists := in["transaction_isolation"]; !exists {
			in["transaction_isolation"] = v
		}
	}
	return in
}
