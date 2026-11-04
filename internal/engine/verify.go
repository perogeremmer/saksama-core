package engine

import (
	"context"
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
			res.OK = err == nil && n > 0
			res.Detail = presence(res.OK, fmt.Sprintf("constraint %s intact", v.Name), fmt.Sprintf("constraint %s missing", v.Name))

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
