package state

import (
	"fmt"
	"os"
)

// OpenEventLog opens the append-only event log for writing, creating it with
// owner-only permissions. Events are flushed as they are written so a crash
// leaves a usable audit trail up to the failure point.
func (s *Store) OpenEventLog(id string) (*os.File, error) {
	f, err := os.OpenFile(s.EventLogPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening event log: %w", err)
	}
	return f, nil
}
