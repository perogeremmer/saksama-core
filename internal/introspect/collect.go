package introspect

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// Collector reads one table plus the surrounding server state.
type Collector struct {
	Q      Querier
	Schema string
	// TableName is the table being inspected. It is not called Table because the
	// Collector also has a Table method.
	TableName string
	// LongQueryThreshold is the age, in seconds, at which a running statement is
	// reported as a metadata-lock hazard.
	LongQueryThreshold int64
	// hasGeneration is a cache for generationExpressions, which asks the server
	// itself whether information_schema.COLUMNS carries a GENERATION_EXPRESSION.
	hasGeneration *bool
	// TrxThreshold is the age, in seconds, at which a transaction is reported.
	TrxThreshold int64
	// DataDir, when it exists on this machine, is used to measure free space.
	DataDir string
}

// NewCollector returns a collector with conservative defaults.
func NewCollector(q Querier, schema, table string) *Collector {
	return &Collector{
		Q: q, Schema: schema, TableName: table,
		LongQueryThreshold: 30,
		TrxThreshold:       30,
	}
}

// Server reads the server identity and defaults.
func (c *Collector) Server(ctx context.Context) (model.Server, error) {
	var srv model.Server
	var version, comment, charset, collation sql.NullString
	var port sql.NullInt64

	err := c.Q.QueryRowContext(ctx, `
		SELECT VERSION(), @@version_comment, @@character_set_server, @@collation_server, @@port
	`).Scan(&version, &comment, &charset, &collation, &port)
	if err != nil {
		return srv, fmt.Errorf("reading server identity: %w", err)
	}

	srv.Version = version.String
	srv.VersionCmt = comment.String
	srv.DefaultChar = normalizeCharset(charset.String)
	srv.DefaultColl = normalizeCollation(collation.String)
	srv.Port = int(port.Int64)
	srv.Vendor = detectVendor(version.String, comment.String)
	return srv, nil
}

// detectVendor classifies the server from its version strings.
func detectVendor(version, comment string) string {
	l := strings.ToLower(version + " " + comment)
	switch {
	case strings.Contains(l, "mariadb"):
		return "mariadb"
	case strings.Contains(l, "aurora"):
		return "aurora"
	case strings.Contains(l, "amazon") || strings.Contains(l, "rds"):
		return "rds"
	case strings.Contains(l, "percona"):
		return "percona"
	}
	return "mysql"
}

// generationExpressions reports whether information_schema.COLUMNS carries a
// GENERATION_EXPRESSION column on this server.
//
// The column exists on MySQL 5.7.6+, MariaDB 10.2+ (where it was already used), and
// everything newer. MySQL 5.6 does not have it, and naming it there fails the whole
// introspection with "Unknown column 'GENERATION_EXPRESSION'". The server is asked
// directly, through the metadata that is always present, so no version parsing and
// no vendor table has to stay in sync with reality.
func (c *Collector) generationExpressions(ctx context.Context) bool {
	if c.hasGeneration != nil {
		return *c.hasGeneration
	}
	var n int
	err := c.Q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = 'information_schema'
		  AND TABLE_NAME = 'COLUMNS'
		  AND COLUMN_NAME = 'GENERATION_EXPRESSION'
	`).Scan(&n)
	truth := err == nil && n > 0
	c.hasGeneration = &truth
	return truth
}

// normalizeCharset unifies the MySQL 5.7 alias "utf8" with the modern name
// "utf8mb3", and follows the collation through. Checks rely on exact string
// comparison across FKs; an alias used on one side and the explicit name on the
// other would read as a mismatch that the server itself accepts. 5.7 reports
// whichever spelling the DDL used, so introspection folds both onto utf8mb3.
func normalizeCharset(name string) string {
	if strings.EqualFold(name, "utf8") {
		return "utf8mb3"
	}
	return name
}

// utf8CollationRewrite renames only collations of the old alias, e.g.
// "utf8_general_ci" becomes "utf8mb3_general_ci"; utf8mb4_* collations pass through.
func normalizeCollation(name string) string {
	if strings.HasPrefix(name, "utf8_") {
		return "utf8mb3_" + name[len("utf8_"):]
	}
	return name
}

// Table reads the full table model.
func (c *Collector) Table(ctx context.Context) (model.Table, error) {
	tbl := model.Table{Schema: c.Schema, Name: c.TableName}

	var engine, rowFormat, collation, createOptions sql.NullString
	var rows sql.NullInt64
	var dataLen, indexLen sql.NullInt64

	err := c.Q.QueryRowContext(ctx, `
		SELECT ENGINE, ROW_FORMAT, TABLE_COLLATION, TABLE_ROWS,
		       DATA_LENGTH, INDEX_LENGTH, CREATE_OPTIONS
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
	`, c.Schema, c.TableName).Scan(&engine, &rowFormat, &collation, &rows, &dataLen, &indexLen, &createOptions)
	if err == sql.ErrNoRows {
		return tbl, fmt.Errorf("table %s.%s does not exist or is not visible to this user", c.Schema, c.TableName)
	}
	if err != nil {
		return tbl, fmt.Errorf("reading table metadata: %w", err)
	}

	tbl.Engine = engine.String
	tbl.RowFormat = strings.ToUpper(rowFormat.String)
	tbl.Collation = strings.ToLower(normalizeCollation(collation.String))
	tbl.Rows = rows.Int64
	tbl.DataLength = dataLen.Int64
	tbl.IndexLength = indexLen.Int64
	tbl.Charset = c.charsetForCollation(ctx, tbl.Collation)
	if strings.Contains(strings.ToLower(createOptions.String), "partitioned") {
		tbl.Partitioned = true
	}
	if p, err := c.isPartitioned(ctx); err == nil && p {
		tbl.Partitioned = true
	}

	if tbl.Columns, err = c.columns(ctx); err != nil {
		return tbl, err
	}
	if tbl.Indexes, err = c.indexes(ctx); err != nil {
		return tbl, err
	}
	if tbl.ForeignKeys, err = c.foreignKeys(ctx, true); err != nil {
		return tbl, err
	}
	if tbl.ReferencedBy, err = c.foreignKeys(ctx, false); err != nil {
		return tbl, err
	}
	if tbl.Triggers, err = c.triggers(ctx); err != nil {
		return tbl, err
	}

	// Fill in the effective charset for textual columns that inherit the table
	// default, so byte-width estimation works.
	for i := range tbl.Columns {
		col := &tbl.Columns[i]
		if col.IsTextual() || col.DataType == "enum" || col.DataType == "set" {
			if col.Charset == "" {
				col.Charset = tbl.Charset
			}
			if col.Collation == "" {
				col.Collation = tbl.Collation
			}
		}
	}

	return tbl, nil
}

func (c *Collector) charsetForCollation(ctx context.Context, collation string) string {
	if collation == "" {
		return ""
	}
	var cs sql.NullString
	err := c.Q.QueryRowContext(ctx, `
		SELECT CHARACTER_SET_NAME FROM information_schema.COLLATIONS WHERE COLLATION_NAME = ?
	`, collation).Scan(&cs)
	if err != nil {
		return ""
	}
	return strings.ToLower(cs.String)
}

func (c *Collector) isPartitioned(ctx context.Context) (bool, error) {
	var n int
	err := c.Q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.PARTITIONS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND PARTITION_NAME IS NOT NULL
	`, c.Schema, c.TableName).Scan(&n)
	return n > 0, err
}

func (c *Collector) columns(ctx context.Context) ([]model.Column, error) {
	// MySQL 5.6 has no GENERATION_EXPRESSION; selecting it there aborts the
	// introspection before any other fact is read.
	genExpr := "''"
	if c.generationExpressions(ctx) {
		genExpr = "GENERATION_EXPRESSION"
	}
	rows, err := c.Q.QueryContext(ctx, `
		SELECT COLUMN_NAME, COLUMN_TYPE, DATA_TYPE, CHARACTER_SET_NAME, COLLATION_NAME,
		       IS_NULLABLE, COLUMN_DEFAULT, EXTRA, `+genExpr+`
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		ORDER BY ORDINAL_POSITION
	`, c.Schema, c.TableName)
	if err != nil {
		return nil, fmt.Errorf("reading columns: %w", err)
	}
	defer rows.Close()

	var out []model.Column
	for rows.Next() {
		var name, colType, dataType, nullable, extra, gen sql.NullString
		var charset, collation, def sql.NullString
		if err := rows.Scan(&name, &colType, &dataType, &charset, &collation,
			&nullable, &def, &extra, &gen); err != nil {
			return nil, fmt.Errorf("scanning column: %w", err)
		}
		col := model.Column{
			Name:      name.String,
			Type:      strings.ToLower(colType.String),
			DataType:  strings.ToLower(dataType.String),
			Charset:   strings.ToLower(normalizeCharset(charset.String)),
			Collation: strings.ToLower(normalizeCollation(collation.String)),
			Nullable:  strings.EqualFold(nullable.String, "YES"),
			Extra:     strings.ToLower(extra.String),
			Generated: gen.String != "",
		}
		if def.Valid {
			v := def.String
			col.Default = &v
		}
		out = append(out, col)
	}
	return out, rows.Err()
}

func (c *Collector) indexes(ctx context.Context) ([]model.Index, error) {
	rows, err := c.Q.QueryContext(ctx, `
		SELECT INDEX_NAME, NON_UNIQUE, SEQ_IN_INDEX, COLUMN_NAME, SUB_PART, INDEX_TYPE, COLLATION
		FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		ORDER BY INDEX_NAME, SEQ_IN_INDEX
	`, c.Schema, c.TableName)
	if err != nil {
		return nil, fmt.Errorf("reading indexes: %w", err)
	}
	defer rows.Close()

	order := []string{}
	byName := map[string]*model.Index{}

	for rows.Next() {
		var name, colName, idxType, idxCollation sql.NullString
		var nonUnique sql.NullInt64
		var seq, subPart sql.NullInt64
		if err := rows.Scan(&name, &nonUnique, &seq, &colName, &subPart, &idxType, &idxCollation); err != nil {
			return nil, fmt.Errorf("scanning index: %w", err)
		}

		n := name.String
		ix, ok := byName[n]
		if !ok {
			ix = &model.Index{
				Name:     n,
				Unique:   nonUnique.Int64 == 0,
				Primary:  n == "PRIMARY",
				Fulltext: strings.EqualFold(idxType.String, "FULLTEXT"),
				Spatial:  strings.EqualFold(idxType.String, "SPATIAL"),
			}
			byName[n] = ix
			order = append(order, n)
		}
		part := model.IndexPart{
			Column:    colName.String,
			PrefixLen: int(subPart.Int64),
			Desc:      strings.EqualFold(idxCollation.String, "D"),
		}
		ix.Parts = append(ix.Parts, part)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]model.Index, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out, nil
}

// foreignKeys reads outgoing FKs (outgoing=true) or incoming ones.
//
// The referential actions are read from REFERENTIAL_CONSTRAINTS, which is the
// only reliable source. They are never inferred from SHOW CREATE TABLE text.
func (c *Collector) foreignKeys(ctx context.Context, outgoing bool) ([]model.ForeignKey, error) {
	query := `
		SELECT k.CONSTRAINT_NAME, k.COLUMN_NAME,
		       k.REFERENCED_TABLE_SCHEMA, k.REFERENCED_TABLE_NAME, k.REFERENCED_COLUMN_NAME,
		       r.DELETE_RULE, r.UPDATE_RULE, r.MATCH_OPTION, k.ORDINAL_POSITION,
		       k.TABLE_SCHEMA, k.TABLE_NAME
		FROM information_schema.KEY_COLUMN_USAGE k
		JOIN information_schema.REFERENTIAL_CONSTRAINTS r
		  ON r.CONSTRAINT_SCHEMA = k.CONSTRAINT_SCHEMA
		 AND r.CONSTRAINT_NAME   = k.CONSTRAINT_NAME
		 AND r.TABLE_NAME        = k.TABLE_NAME
		WHERE `
	if outgoing {
		query += `k.TABLE_SCHEMA = ? AND k.TABLE_NAME = ? AND k.REFERENCED_TABLE_NAME IS NOT NULL`
	} else {
		query += `k.REFERENCED_TABLE_SCHEMA = ? AND k.REFERENCED_TABLE_NAME = ?`
	}
	query += ` ORDER BY k.CONSTRAINT_NAME, k.ORDINAL_POSITION`

	rows, err := c.Q.QueryContext(ctx, query, c.Schema, c.TableName)
	if err != nil {
		return nil, fmt.Errorf("reading foreign keys: %w", err)
	}
	defer rows.Close()

	order := []string{}
	byName := map[string]*model.ForeignKey{}

	for rows.Next() {
		var name, column, refSchema, refTable, refColumn sql.NullString
		var deleteRule, updateRule, matchOption sql.NullString
		var ordinal sql.NullInt64
		var childSchema, childTable sql.NullString

		if err := rows.Scan(&name, &column, &refSchema, &refTable, &refColumn,
			&deleteRule, &updateRule, &matchOption, &ordinal, &childSchema, &childTable); err != nil {
			return nil, fmt.Errorf("scanning foreign key: %w", err)
		}

		n := name.String
		fk, ok := byName[n]
		if !ok {
			fk = &model.ForeignKey{
				Name:      n,
				OnDelete:  parseAction(deleteRule.String),
				OnUpdate:  parseAction(updateRule.String),
				Match:     strings.ToUpper(matchOption.String),
				RefSchema: refSchema.String,
				RefTable:  refTable.String,
			}
			if !outgoing {
				// For incoming constraints: Columns are the child's columns and
				// RefColumns are this table's columns; RefTable names the child.
				fk.RefTable = childTable.String
				fk.RefSchema = childSchema.String
			}
			byName[n] = fk
			order = append(order, n)
		}

		if outgoing {
			fk.Columns = append(fk.Columns, column.String)
			fk.RefColumns = append(fk.RefColumns, refColumn.String)
		} else {
			fk.Columns = append(fk.Columns, column.String)
			fk.RefColumns = append(fk.RefColumns, refColumn.String)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]model.ForeignKey, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out, nil
}

func parseAction(s string) model.FKAction {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "CASCADE":
		return model.ActionCascade
	case "SET NULL":
		return model.ActionSetNull
	case "SET DEFAULT":
		return model.ActionSetDefault
	case "NO ACTION":
		return model.ActionNoAction
	case "RESTRICT", "":
		return model.ActionRestrict
	}
	return model.FKAction(strings.ToUpper(strings.TrimSpace(s)))
}

func (c *Collector) triggers(ctx context.Context) ([]string, error) {
	rows, err := c.Q.QueryContext(ctx, `
		SELECT TRIGGER_NAME FROM information_schema.TRIGGERS
		WHERE EVENT_OBJECT_SCHEMA = ? AND EVENT_OBJECT_TABLE = ?
		ORDER BY TRIGGER_NAME
	`, c.Schema, c.TableName)
	if err != nil {
		return nil, fmt.Errorf("reading triggers: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Runtime reads operational state: load, locks, transactions, replica lag,
// grants, leftovers, and (when possible) free disk.
func (c *Collector) Runtime(ctx context.Context) (Runtime, error) {
	var rt Runtime

	// Threads_running. SHOW GLOBAL STATUS is available without extra privileges.
	var name string
	var value string
	if err := c.Q.QueryRowContext(ctx, `SHOW GLOBAL STATUS LIKE 'Threads_running'`).Scan(&name, &value); err == nil {
		fmt.Sscanf(value, "%d", &rt.ThreadsRunning)
	}

	rt.LongQueries = c.longQueries(ctx)
	rt.Transactions = c.transactions(ctx)
	rt.PendingMDLocks = c.pendingMDLocks(ctx)
	rt.Grants = c.grants(ctx)
	rt.Artifacts = c.artifacts(ctx)
	rt.ReplicaLagSec, rt.ReplicaConfigured, rt.ReplicaRunning = c.replica(ctx)

	var lwt sql.NullInt64
	if err := c.Q.QueryRowContext(ctx, `SELECT @@SESSION.lock_wait_timeout`).Scan(&lwt); err == nil {
		rt.LockWaitTimeout = lwt.Int64
	}

	// The data directory is needed to say anything about disk space, so read it
	// here rather than depending on the caller to inject it.
	if c.DataDir == "" {
		var dir sql.NullString
		if err := c.Q.QueryRowContext(ctx, `SELECT @@datadir`).Scan(&dir); err == nil {
			c.DataDir = dir.String
		}
	}
	if c.DataDir != "" {
		rt.DataDir = c.DataDir
		rt.Disk = c.diskSpace(ctx)
	}

	return rt, nil
}

func (c *Collector) longQueries(ctx context.Context) []LongQuery {
	rows, err := c.Q.QueryContext(ctx, `
		SELECT ID, USER, HOST, TIME, STATE, INFO
		FROM information_schema.PROCESSLIST
		WHERE COMMAND NOT IN ('Sleep', 'Daemon')
		  AND INFO IS NOT NULL
		  AND TIME >= ?
		ORDER BY TIME DESC
		LIMIT 20
	`, c.LongQueryThreshold)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []LongQuery
	for rows.Next() {
		var id, secs sql.NullInt64
		var user, host, state, info sql.NullString
		if err := rows.Scan(&id, &user, &host, &secs, &state, &info); err != nil {
			return out
		}
		out = append(out, LongQuery{
			ID: id.Int64, User: user.String, Host: host.String,
			Seconds: secs.Int64, State: state.String, SQL: truncate(info.String, 300),
		})
	}
	return out
}

func (c *Collector) transactions(ctx context.Context) []Transaction {
	rows, err := c.Q.QueryContext(ctx, `
		SELECT trx_id, TIMESTAMPDIFF(SECOND, trx_started, NOW()) AS age,
		       trx_state, trx_rows_locked, trx_rows_modified
		FROM information_schema.INNODB_TRX
		WHERE TIMESTAMPDIFF(SECOND, trx_started, NOW()) >= ?
		ORDER BY age DESC
		LIMIT 20
	`, c.TrxThreshold)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Transaction
	for rows.Next() {
		var id, state sql.NullString
		var age sql.NullInt64
		var locked, modified sql.NullInt64
		if err := rows.Scan(&id, &age, &state, &locked, &modified); err != nil {
			return out
		}
		out = append(out, Transaction{
			ID: id.String, AgeSeconds: age.Int64, State: state.String,
			RowsLocked: locked.Int64, RowsModified: modified.Int64,
		})
	}
	return out
}

func (c *Collector) pendingMDLocks(ctx context.Context) int {
	var n int
	err := c.Q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM performance_schema.metadata_locks WHERE LOCK_STATUS = 'PENDING'
	`).Scan(&n)
	if err != nil {
		return 0
	}
	return n
}

func (c *Collector) replica(ctx context.Context) (lag float64, configured, running bool) {
	// MySQL 8.0 uses SHOW REPLICA STATUS; fall back to SHOW SLAVE STATUS.
	for _, q := range []string{"SHOW REPLICA STATUS", "SHOW SLAVE STATUS"} {
		rows, err := c.Q.QueryContext(ctx, q)
		if err != nil {
			continue
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			continue
		}
		got := false
		for rows.Next() {
			vals := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				break
			}
			got = true
			for i, col := range cols {
				switch col {
				case "Seconds_Behind_Source", "Seconds_Behind_Master":
					fmt.Sscanf(vals[i].String, "%f", &lag)
				case "Replica_IO_Running", "Slave_IO_Running":
					running = strings.EqualFold(vals[i].String, "Yes")
				}
			}
		}
		rows.Close()
		if got {
			return lag, true, running
		}
	}
	return 0, false, false
}

func (c *Collector) grants(ctx context.Context) Grants {
	rows, err := c.Q.QueryContext(ctx, `SHOW GRANTS FOR CURRENT_USER()`)
	if err != nil {
		return Grants{}
	}
	defer rows.Close()

	g := Grants{}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			break
		}
		g.Raw = append(g.Raw, line)
		up := strings.ToUpper(line)
		if i := strings.Index(up, " ON "); i > 0 {
			privs := up[:i]
			check := func(want string) bool { return strings.Contains(privs, want) }
			if check("ALL PRIVILEGES") {
				g.AllPrivileges = true
			}
			if check("SUPER") {
				g.Super = true
			}
			if check("PROCESS") {
				g.Process = true
			}
			if check("RELOAD") {
				g.Reload = true
			}
			if check("SYSTEM_VARIABLES_ADMIN") {
				g.SystemVarsAdmin = true
			}
			if check("SESSION_VARIABLES_ADMIN") {
				g.SessionVarsAdmin = true
			}
			if check("BACKUP_ADMIN") {
				g.BackupAdmin = true
			}
			if check("REPLICATION CLIENT") {
				g.RepClient = true
			}
		}
	}
	var user sql.NullString
	if err := c.Q.QueryRowContext(ctx, `SELECT CURRENT_USER()`).Scan(&user); err == nil {
		g.User = user.String
	}
	return g
}

// artifacts finds leftovers from aborted migrations in this schema.
func (c *Collector) artifacts(ctx context.Context) []Artifact {
	var out []Artifact

	rows, err := c.Q.QueryContext(ctx, `
		SELECT TABLE_NAME FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ?
		  AND (TABLE_NAME LIKE '\\_%\\_new' OR TABLE_NAME LIKE '\\_%\\_old'
		       OR TABLE_NAME LIKE '\\_%\\_gho' OR TABLE_NAME LIKE '\\_%\\_del')
		ORDER BY TABLE_NAME
	`, c.Schema)
	if err == nil {
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err == nil {
				out = append(out, Artifact{Kind: "table", Name: n, Evidence: "shadow table naming pattern"})
			}
		}
		rows.Close()
	}

	rows, err = c.Q.QueryContext(ctx, `
		SELECT TRIGGER_NAME FROM information_schema.TRIGGERS
		WHERE TRIGGER_SCHEMA = ?
		  AND (TRIGGER_NAME LIKE 'pt\\_osc\\_%' OR TRIGGER_NAME LIKE 'ghost\\_%')
		ORDER BY TRIGGER_NAME
	`, c.Schema)
	if err == nil {
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err == nil {
				out = append(out, Artifact{Kind: "trigger", Name: n, Evidence: "online-schema-change trigger naming pattern"})
			}
		}
		rows.Close()
	}

	return out
}

// UniqueDuplicates counts duplicate groups for the given column tuple. It is
// bounded: it returns as soon as it finds anything, and reports at most 2.
func (c *Collector) UniqueDuplicates(ctx context.Context, columns []string) (int64, error) {
	if len(columns) == 0 {
		return 0, fmt.Errorf("no columns given")
	}
	quoted := make([]string, len(columns))
	for i, col := range columns {
		quoted[i] = "`" + strings.ReplaceAll(col, "`", "``") + "`"
	}
	groupBy := strings.Join(quoted, ", ")
	query := fmt.Sprintf(
		"SELECT COUNT(*) FROM (SELECT 1 FROM `%s`.`%s` GROUP BY %s HAVING COUNT(*) > 1 LIMIT 2) AS d",
		escapeIdent(c.Schema), escapeIdent(c.TableName), groupBy)

	var n int64
	if err := c.Q.QueryRowContext(ctx, query).Scan(&n); err != nil {
		return 0, fmt.Errorf("duplicate probe failed: %w", err)
	}
	return n, nil
}

// FKCharset checks the character set of both sides of every foreign key that
// involves the table. This is the verifiable form of check A1.
func (c *Collector) FKCharset(ctx context.Context, tbl model.Table) ([]FKCheck, error) {
	var out []FKCheck

	for _, fk := range tbl.ForeignKeys {
		check := FKCheck{
			Constraint: fk.Name, Direction: "outgoing", PeerTable: fk.RefTable, Compatible: true,
		}
		peer, err := c.columnsOf(ctx, fk.RefSchema, fk.RefTable)
		if err != nil {
			// Unreadable peer: leave Compatible true and let the caller note the
			// gap, rather than reporting a false conflict.
			continue
		}
		for i, local := range fk.Columns {
			if i >= len(fk.RefColumns) {
				break
			}
			localCol, ok1 := tbl.Column(local)
			peerCol, ok2 := peer[fk.RefColumns[i]]
			if !ok1 || !ok2 || !localCol.IsTextual() || !peerCol.IsTextual() {
				continue
			}
			pair := FKColumnPair{
				LocalColumn: local, LocalCharset: localCol.Charset, LocalCollation: localCol.Collation,
				PeerColumn: peerCol.Name, PeerCharset: peerCol.Charset, PeerCollation: peerCol.Collation,
			}
			pair.Compatible = strings.EqualFold(pair.LocalCharset, pair.PeerCharset) &&
				strings.EqualFold(pair.LocalCollation, pair.PeerCollation)
			if !pair.Compatible {
				check.Compatible = false
			}
			check.Pairs = append(check.Pairs, pair)
		}
		out = append(out, check)
	}

	for _, fk := range tbl.ReferencedBy {
		check := FKCheck{
			Constraint: fk.Name, Direction: "incoming", PeerTable: fk.RefTable, Compatible: true,
		}
		peer, err := c.columnsOf(ctx, fk.RefSchema, fk.RefTable)
		if err != nil {
			continue
		}
		for i, childCol := range fk.Columns {
			if i >= len(fk.RefColumns) {
				break
			}
			localName := fk.RefColumns[i]
			localCol, ok1 := tbl.Column(localName)
			peerCol, ok2 := peer[childCol]
			if !ok1 || !ok2 || !localCol.IsTextual() || !peerCol.IsTextual() {
				continue
			}
			pair := FKColumnPair{
				LocalColumn: localName, LocalCharset: localCol.Charset, LocalCollation: localCol.Collation,
				PeerColumn: peerCol.Name, PeerCharset: peerCol.Charset, PeerCollation: peerCol.Collation,
			}
			pair.Compatible = strings.EqualFold(pair.LocalCharset, pair.PeerCharset) &&
				strings.EqualFold(pair.LocalCollation, pair.PeerCollation)
			if !pair.Compatible {
				check.Compatible = false
			}
			check.Pairs = append(check.Pairs, pair)
		}
		out = append(out, check)
	}

	return out, nil
}

func (c *Collector) columnsOf(ctx context.Context, schema, table string) (map[string]model.Column, error) {
	rows, err := c.Q.QueryContext(ctx, `
		SELECT COLUMN_NAME, COLUMN_TYPE, DATA_TYPE, CHARACTER_SET_NAME, COLLATION_NAME
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
	`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]model.Column{}
	for rows.Next() {
		var name, colType, dataType, charset, collation sql.NullString
		if err := rows.Scan(&name, &colType, &dataType, &charset, &collation); err != nil {
			return nil, err
		}
		out[name.String] = model.Column{
			Name:      name.String,
			Type:      strings.ToLower(colType.String),
			DataType:  strings.ToLower(dataType.String),
			Charset:   strings.ToLower(normalizeCharset(charset.String)),
			Collation: strings.ToLower(normalizeCollation(collation.String)),
		}
	}
	return out, rows.Err()
}

// Collect takes a full snapshot.
func (c *Collector) Collect(ctx context.Context) (Snapshot, error) {
	snap := Snapshot{
		Schema:      c.Schema,
		CollectedAt: time.Now().UTC().Format(time.RFC3339),
	}

	srv, err := c.Server(ctx)
	if err != nil {
		return snap, err
	}
	snap.Server = srv

	tbl, err := c.Table(ctx)
	if err != nil {
		return snap, err
	}
	snap.Table = tbl

	rt, err := c.Runtime(ctx)
	if err != nil {
		return snap, err
	}
	snap.Runtime = rt

	if fk, err := c.FKCharset(ctx, tbl); err == nil {
		snap.FKChecks = fk
	}

	return snap, nil
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func escapeIdent(s string) string {
	return strings.ReplaceAll(s, "`", "``")
}

// diskSpace measures the volume holding the data directory, but only when the
// server is plausibly on this machine.
//
// The path alone is not proof: a CI runner that has MySQL preinstalled also has
// /var/lib/mysql, so a container's data directory can look local while the free
// space belongs to a completely different filesystem. Comparing hostnames is a
// heuristic, and it is the conservative one: a mismatch means "do not measure",
// which makes the operator supply the numbers rather than trusting a wrong one.
func (c *Collector) diskSpace(ctx context.Context) DiskSpace {
	local, _ := os.Hostname()

	var serverHost sql.NullString
	_ = c.Q.QueryRowContext(ctx, `SELECT @@hostname`).Scan(&serverHost)

	if local != "" && serverHost.String != "" && !strings.EqualFold(local, serverHost.String) {
		return DiskSpace{
			Path: c.DataDir,
			Reason: fmt.Sprintf("the server reports hostname %q and this machine is %q, so its data directory is not on this host",
				serverHost.String, local),
		}
	}
	return ReadDiskSpace(c.DataDir)
}
