package introspect

import "testing"

// MySQL 5.6 calls transaction_isolation "tx_isolation". Both spellings must end
// up under one name, or the check silently reports unknown on a server that
// answered the question.
func TestNormalizeSettingsMapsThe56Name(t *testing.T) {
	got := normalizeSettings(map[string]string{"tx_isolation": "REPEATABLE-READ"})
	if got["transaction_isolation"] != "REPEATABLE-READ" {
		t.Errorf("tx_isolation was not mapped: %v", got)
	}

	// A server that reports both (or only the new name) keeps its own value.
	got = normalizeSettings(map[string]string{
		"transaction_isolation": "READ-COMMITTED",
		"tx_isolation":          "REPEATABLE-READ",
	})
	if got["transaction_isolation"] != "READ-COMMITTED" {
		t.Errorf("the authoritative name must win: %v", got)
	}
}
