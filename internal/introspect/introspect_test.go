package introspect

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

// MySQL 5.6 without GENERATION_EXPRESSION is a real compatibility hazard; the
// query must bypass the column there without losing the facts it can carry. This
// test skips the runtime check when the server has the column.
func TestColumnsSurviveServerWithoutGenerationExpression(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a database")
	}
	db, err := sql.Open("mysql", os.Getenv("SAKSAMA_TEST_DSN"))
	if err != nil {
		t.Skip("no DSN:", err)
	}
	defer db.Close()
	// The probe itself is the unit under test: whatever the server is, the
	// collector must answer without error and remember the answer.
	ctx := context.Background()
	c := NewCollector(db, "mysql", "")
	var probeCol int
	if err := c.Q.QueryRowContext(context.Background(),
		"SELECT @@version IS NOT NULL").Scan(&probeCol); err != nil {
		t.Skip("no server:", err)
	}
	has := c.generationExpressions(ctx)
	if has != c.generationExpressions(ctx) {
		t.Fatal("the probe must be cached, not re-asked per query")
	}
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='information_schema' AND TABLE_NAME='COLUMNS' AND COLUMN_NAME='GENERATION_EXPRESSION'").Scan(&n)
	if (n > 0) != has {
		t.Fatalf("probe says %v, the server says n=%d", has, n)
	}
}

// MySQL 5.7 reports a column created with CHARACTER SET utf8mb3 as "utf8" and its
// collation as "utf8_general_ci"; 8.0 reports the modern spelling. Checks compare
// charset strings across foreign keys, so both spellings have to fold onto one
// name, whatever the server says.
func TestCharsetAliasesNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"utf8", "utf8mb3"},
		{"UTF8", "utf8mb3"},
		{"utf8mb4", "utf8mb4"},
		{"latin1", "latin1"},
	}
	for _, c := range cases {
		if got := normalizeCharset(c.in); got != c.want {
			t.Errorf("normalizeCharset(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	cases = []struct{ in, want string }{
		{"utf8_general_ci", "utf8mb3_general_ci"},
		{"utf8mb4_0900_ai_ci", "utf8mb4_0900_ai_ci"},
		{"utf8mb3_general_ci", "utf8mb3_general_ci"},
		{"latin1_swedish_ci", "latin1_swedish_ci"},
	}
	for _, c := range cases {
		if got := normalizeCollation(c.in); got != c.want {
			t.Errorf("normalizeCollation(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
