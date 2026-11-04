package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/introspect"
)

// VerifyAgainstSchema asserts the intended end state by reading
// information_schema. It is shared by every engine on purpose: an engine's exit
// code is not evidence, and each engine must be judged by the same standard.
func VerifyAgainstSchema(ctx context.Context, q introspect.Querier, schema, table string, checks []analyzer.Verification) ([]VerifyResult, error) {
	var out []VerifyResult
	for _, v := range checks {
		res := VerifyResult{Kind: v.Kind, Name: v.Name}
		switch v.Kind {
		case "index_exists":
			n, err := count(ctx, q, `
				SELECT COUNT(*) FROM information_schema.STATISTICS
				WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND INDEX_NAME = ?`, schema, table, v.Name)
			res.OK = err == nil && n > 0
			res.Detail = presence(res.OK, fmt.Sprintf("index %s present", v.Name), fmt.Sprintf("index %s missing", v.Name))

		case "index_absent":
			n, err := count(ctx, q, `
				SELECT COUNT(*) FROM information_schema.STATISTICS
				WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND INDEX_NAME = ?`, schema, table, v.Name)
			res.OK = err == nil && n == 0
			res.Detail = presence(res.OK, fmt.Sprintf("index %s removed", v.Name), fmt.Sprintf("index %s still present", v.Name))

		case "column_exists":
			n, err := count(ctx, q, `
				SELECT COUNT(*) FROM information_schema.COLUMNS
				WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?`, schema, table, v.Name)
			res.OK = err == nil && n > 0
			res.Detail = presence(res.OK, fmt.Sprintf("column %s present", v.Name), fmt.Sprintf("column %s missing", v.Name))

		case "column_absent":
			n, err := count(ctx, q, `
				SELECT COUNT(*) FROM information_schema.COLUMNS
				WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?`, schema, table, v.Name)
			res.OK = err == nil && n == 0
			res.Detail = presence(res.OK, fmt.Sprintf("column %s removed", v.Name), fmt.Sprintf("column %s still present", v.Name))

		case "foreign_key_present":
			// Note the two placeholders: schema and constraint name only.
			n, err := count(ctx, q, `
				SELECT COUNT(*) FROM information_schema.REFERENTIAL_CONSTRAINTS
				WHERE CONSTRAINT_SCHEMA = ? AND CONSTRAINT_NAME = ?`, schema, v.Name)
			if err == nil && n > 0 {
				res.OK = true
				res.Detail = fmt.Sprintf("constraint %s intact", v.Name)
				break
			}

			// Constraint names are not stable across engine versions: some keep an
			// underscore prefix on the copy, some restore the original name. The
			// invariant is the constraint itself, so fall back to matching it by
			// structure and report the name it actually ended up with.
			if actual, ok := findConstraintByStructure(ctx, q, schema, table, v); ok {
				res.OK = true
				res.Detail = fmt.Sprintf("constraint %s present as %s (renamed by the engine)", v.Name, actual)
				break
			}
			res.OK = false
			res.Detail = fmt.Sprintf("constraint %s missing", v.Name)
		default:
			res.Detail = fmt.Sprintf("unknown verification kind %q", v.Kind)
		}
		out = append(out, res)
	}
	return out, nil
}

func count(ctx context.Context, q introspect.Querier, query string, args ...any) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func presence(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}

// QuoteIdent escapes a MySQL identifier.
func QuoteIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// findConstraintByStructure looks for a foreign key with the same shape, whatever
// it is called now.
func findConstraintByStructure(ctx context.Context, q introspect.Querier, schema, table string, want analyzer.Verification) (string, bool) {
	if want.RefTable == "" {
		// Without the shape, the best guess is the copy-engine prefix.
		return findRenamedConstraint(ctx, q, schema, want.Name)
	}

	rows, err := q.QueryContext(ctx, `
		SELECT CONSTRAINT_NAME, REFERENCED_TABLE_NAME, COLUMN_NAME
		FROM information_schema.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND REFERENCED_TABLE_NAME IS NOT NULL
		ORDER BY CONSTRAINT_NAME, ORDINAL_POSITION`, schema, table)
	if err != nil {
		return "", false
	}
	defer rows.Close()

	type candidate struct {
		refTable string
		columns  []string
	}
	found := map[string]*candidate{}
	order := []string{}

	for rows.Next() {
		var name, refTable, column sql.NullString
		if err := rows.Scan(&name, &refTable, &column); err != nil {
			return "", false
		}
		c, ok := found[name.String]
		if !ok {
			c = &candidate{refTable: refTable.String}
			found[name.String] = c
			order = append(order, name.String)
		}
		c.columns = append(c.columns, column.String)
	}

	for _, name := range order {
		c := found[name]
		if !strings.EqualFold(c.refTable, want.RefTable) {
			continue
		}
		if len(c.columns) != len(want.Columns) {
			continue
		}
		same := true
		for i := range c.columns {
			if !strings.EqualFold(c.columns[i], want.Columns[i]) {
				same = false
				break
			}
		}
		if same {
			return name, true
		}
	}
	return "", false
}

// findRenamedConstraint looks for the copy-engine rename of a constraint: pt-osc
// has historically prefixed the name with an underscore.
func findRenamedConstraint(ctx context.Context, q introspect.Querier, schema, name string) (string, bool) {
	var found string
	err := q.QueryRowContext(ctx, `
		SELECT CONSTRAINT_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS
		WHERE CONSTRAINT_SCHEMA = ? AND CONSTRAINT_NAME = ?
		LIMIT 1`, schema, "_"+name).Scan(&found)
	if err != nil || found == "" {
		return "", false
	}
	return found, true
}
