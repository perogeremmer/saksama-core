package introspect

import "context"

// TableExists reports whether a table is visible in the schema.
func (c *Collector) TableExists(ctx context.Context, schema, table string) bool {
	var n int
	err := c.Q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, schema, table).Scan(&n)
	return err == nil && n > 0
}

// TriggerExists reports whether a trigger is visible in the schema.
func (c *Collector) TriggerExists(ctx context.Context, schema, trigger string) bool {
	var n int
	err := c.Q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.TRIGGERS
		WHERE TRIGGER_SCHEMA = ? AND TRIGGER_NAME = ?`, schema, trigger).Scan(&n)
	return err == nil && n > 0
}
