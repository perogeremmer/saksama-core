package introspect

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// SchemaTables reads every base table in a schema using a handful of bulk
// queries instead of five per table.
//
// Inspecting a schema table by table would mean hundreds of round trips on a real
// database, so the whole schema is read at once and assembled in memory.
func (c *Collector) SchemaTables(ctx context.Context, includeViews bool) ([]model.Table, error) {
	byName := map[string]*model.Table{}
	var order []string

	rows, err := c.Q.QueryContext(ctx, `
		SELECT TABLE_NAME, ENGINE, ROW_FORMAT, TABLE_COLLATION, TABLE_ROWS,
		       DATA_LENGTH, INDEX_LENGTH, CREATE_OPTIONS, TABLE_TYPE
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ?
		ORDER BY TABLE_NAME
	`, c.Schema)
	if err != nil {
		return nil, fmt.Errorf("reading tables: %w", err)
	}
	for rows.Next() {
		var name, engine, rowFormat, collation, createOptions, tableType sql.NullString
		var rowCount, dataLen, indexLen sql.NullInt64
		if err := rows.Scan(&name, &engine, &rowFormat, &collation, &rowCount, &dataLen, &indexLen, &createOptions, &tableType); err != nil {
			rows.Close()
			return nil, err
		}
		if !includeViews && strings.EqualFold(tableType.String, "VIEW") {
			continue
		}
		t := &model.Table{
			Schema:      c.Schema,
			Name:        name.String,
			Engine:      engine.String,
			RowFormat:   strings.ToUpper(rowFormat.String),
			Collation:   strings.ToLower(normalizeCollation(collation.String)),
			Rows:        rowCount.Int64,
			DataLength:  dataLen.Int64,
			IndexLength: indexLen.Int64,
			Partitioned: strings.Contains(strings.ToLower(createOptions.String), "partitioned"),
		}
		byName[name.String] = t
		order = append(order, name.String)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(order) == 0 {
		return nil, nil
	}

	c.attachCharsets(ctx, byName)
	if err := c.attachColumns(ctx, byName); err != nil {
		return nil, err
	}
	if err := c.attachIndexes(ctx, byName); err != nil {
		return nil, err
	}
	if err := c.attachForeignKeys(ctx, byName); err != nil {
		return nil, err
	}
	if err := c.attachTriggers(ctx, byName); err != nil {
		return nil, err
	}
	if err := c.attachPartitions(ctx, byName); err != nil {
		return nil, err
	}

	out := make([]model.Table, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out, nil
}

func (c *Collector) attachCharsets(ctx context.Context, tables map[string]*model.Table) {
	rows, err := c.Q.QueryContext(ctx, `
		SELECT COLLATION_NAME, CHARACTER_SET_NAME FROM information_schema.COLLATIONS
	`)
	if err != nil {
		return
	}
	defer rows.Close()
	byCollation := map[string]string{}
	for rows.Next() {
		var collation, charset sql.NullString
		if rows.Scan(&collation, &charset) == nil {
			byCollation[strings.ToLower(normalizeCollation(collation.String))] = strings.ToLower(normalizeCharset(charset.String))
		}
	}
	for _, t := range tables {
		if cs, ok := byCollation[t.Collation]; ok {
			t.Charset = cs
		}
	}
}

func (c *Collector) attachColumns(ctx context.Context, tables map[string]*model.Table) error {
	// MySQL 5.6 has no GENERATION_EXPRESSION; selecting it there aborts the
	// introspection before any other fact is read.
	genExpr := "''"
	if c.generationExpressions(ctx) {
		genExpr = "GENERATION_EXPRESSION"
	}
	rows, err := c.Q.QueryContext(ctx, `
		SELECT TABLE_NAME, COLUMN_NAME, COLUMN_TYPE, DATA_TYPE, CHARACTER_SET_NAME,
		       COLLATION_NAME, IS_NULLABLE, EXTRA, `+genExpr+`
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ?
		ORDER BY TABLE_NAME, ORDINAL_POSITION
	`, c.Schema)
	if err != nil {
		return fmt.Errorf("reading columns: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var table, name, colType, dataType, nullable, extra, gen sql.NullString
		var charset, collation sql.NullString
		if err := rows.Scan(&table, &name, &colType, &dataType, &charset, &collation, &nullable, &extra, &gen); err != nil {
			return err
		}
		t, ok := tables[table.String]
		if !ok {
			continue
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
		if col.IsTextual() && col.Charset == "" {
			col.Charset = t.Charset
		}
		if col.IsTextual() && col.Collation == "" {
			col.Collation = t.Collation
		}
		t.Columns = append(t.Columns, col)
	}
	return rows.Err()
}

func (c *Collector) attachIndexes(ctx context.Context, tables map[string]*model.Table) error {
	rows, err := c.Q.QueryContext(ctx, `
		SELECT TABLE_NAME, INDEX_NAME, NON_UNIQUE, SEQ_IN_INDEX, COLUMN_NAME, SUB_PART, INDEX_TYPE, COLLATION
		FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = ?
		ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX
	`, c.Schema)
	if err != nil {
		return fmt.Errorf("reading indexes: %w", err)
	}
	defer rows.Close()

	type key struct{ table, index string }
	seen := map[key]*model.Index{}

	for rows.Next() {
		var table, name, colName, idxType, idxCollation sql.NullString
		var nonUnique, seq, subPart sql.NullInt64
		if err := rows.Scan(&table, &name, &nonUnique, &seq, &colName, &subPart, &idxType, &idxCollation); err != nil {
			return err
		}
		t, ok := tables[table.String]
		if !ok {
			continue
		}
		k := key{table.String, name.String}
		ix, ok := seen[k]
		if !ok {
			ix = &model.Index{
				Name:     name.String,
				Unique:   nonUnique.Int64 == 0,
				Primary:  name.String == "PRIMARY",
				Fulltext: strings.EqualFold(idxType.String, "FULLTEXT"),
				Spatial:  strings.EqualFold(idxType.String, "SPATIAL"),
			}
			seen[k] = ix
			t.Indexes = append(t.Indexes, *ix)
		}
		part := model.IndexPart{
			Column:    colName.String,
			PrefixLen: int(subPart.Int64),
			Desc:      strings.EqualFold(idxCollation.String, "D"),
		}
		// Re-fetch the pointer, since appending a copy above invalidates ix.
		last := &t.Indexes[len(t.Indexes)-1]
		last.Parts = append(last.Parts, part)
	}
	return rows.Err()
}

func (c *Collector) attachForeignKeys(ctx context.Context, tables map[string]*model.Table) error {
	rows, err := c.Q.QueryContext(ctx, `
		SELECT k.TABLE_NAME, k.CONSTRAINT_NAME, k.COLUMN_NAME,
		       k.REFERENCED_TABLE_NAME, k.REFERENCED_COLUMN_NAME,
		       r.DELETE_RULE, r.UPDATE_RULE, r.MATCH_OPTION, k.ORDINAL_POSITION
		FROM information_schema.KEY_COLUMN_USAGE k
		JOIN information_schema.REFERENTIAL_CONSTRAINTS r
		  ON r.CONSTRAINT_SCHEMA = k.CONSTRAINT_SCHEMA
		 AND r.CONSTRAINT_NAME   = k.CONSTRAINT_NAME
		 AND r.TABLE_NAME        = k.TABLE_NAME
		WHERE k.TABLE_SCHEMA = ? AND k.REFERENCED_TABLE_NAME IS NOT NULL
		ORDER BY k.TABLE_NAME, k.CONSTRAINT_NAME, k.ORDINAL_POSITION
	`, c.Schema)
	if err != nil {
		return fmt.Errorf("reading foreign keys: %w", err)
	}
	defer rows.Close()

	type key struct{ table, name string }
	seen := map[key]*model.ForeignKey{}

	for rows.Next() {
		var table, name, column, refTable, refColumn sql.NullString
		var deleteRule, updateRule, matchOption sql.NullString
		var ordinal sql.NullInt64
		if err := rows.Scan(&table, &name, &column, &refTable, &refColumn,
			&deleteRule, &updateRule, &matchOption, &ordinal); err != nil {
			return err
		}
		t, ok := tables[table.String]
		if !ok {
			continue
		}
		k := key{table.String, name.String}
		if _, ok := seen[k]; !ok {
			fk := model.ForeignKey{
				Name:      name.String,
				OnDelete:  parseAction(deleteRule.String),
				OnUpdate:  parseAction(updateRule.String),
				Match:     strings.ToUpper(matchOption.String),
				RefSchema: c.Schema,
				RefTable:  refTable.String,
			}
			t.ForeignKeys = append(t.ForeignKeys, fk)
			seen[k] = &t.ForeignKeys[len(t.ForeignKeys)-1]
		}
		last := &t.ForeignKeys[len(t.ForeignKeys)-1]
		last.Columns = append(last.Columns, column.String)
		last.RefColumns = append(last.RefColumns, refColumn.String)

		// Incoming: the referenced table sees this constraint from the child.
		if parent, ok := tables[refTable.String]; ok {
			found := false
			for i := range parent.ReferencedBy {
				if parent.ReferencedBy[i].Name == name.String && parent.ReferencedBy[i].RefTable == table.String {
					parent.ReferencedBy[i].Columns = append(parent.ReferencedBy[i].Columns, column.String)
					parent.ReferencedBy[i].RefColumns = append(parent.ReferencedBy[i].RefColumns, refColumn.String)
					found = true
					break
				}
			}
			if !found {
				parent.ReferencedBy = append(parent.ReferencedBy, model.ForeignKey{
					Name: name.String, RefSchema: c.Schema, RefTable: table.String,
					Columns: []string{column.String}, RefColumns: []string{refColumn.String},
					OnDelete: parseAction(deleteRule.String), OnUpdate: parseAction(updateRule.String),
				})
			}
		}
	}
	return rows.Err()
}

func (c *Collector) attachTriggers(ctx context.Context, tables map[string]*model.Table) error {
	rows, err := c.Q.QueryContext(ctx, `
		SELECT EVENT_OBJECT_TABLE, TRIGGER_NAME FROM information_schema.TRIGGERS
		WHERE EVENT_OBJECT_SCHEMA = ? ORDER BY EVENT_OBJECT_TABLE, TRIGGER_NAME
	`, c.Schema)
	if err != nil {
		return fmt.Errorf("reading triggers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, trigger sql.NullString
		if err := rows.Scan(&table, &trigger); err != nil {
			return err
		}
		if t, ok := tables[table.String]; ok {
			t.Triggers = append(t.Triggers, trigger.String)
		}
	}
	return rows.Err()
}

func (c *Collector) attachPartitions(ctx context.Context, tables map[string]*model.Table) error {
	rows, err := c.Q.QueryContext(ctx, `
		SELECT DISTINCT TABLE_NAME FROM information_schema.PARTITIONS
		WHERE TABLE_SCHEMA = ? AND PARTITION_NAME IS NOT NULL
	`, c.Schema)
	if err != nil {
		return nil // not fatal: CREATE_OPTIONS already hinted at it
	}
	defer rows.Close()
	for rows.Next() {
		var table sql.NullString
		if err := rows.Scan(&table); err == nil {
			if t, ok := tables[table.String]; ok {
				t.Partitioned = true
			}
		}
	}
	return nil
}
