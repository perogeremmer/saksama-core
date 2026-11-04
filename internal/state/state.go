// Package state persists migration sessions.
//
// A session is the durable record of a decision and what happened because of it:
// the plan, the exact statements, the approvals, the timings, the verification
// results, and the append-only event log. The state directory is the reason a
// dropped SSH connection does not lose a migration.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// State is the session lifecycle state.
type State string

const (
	StatePlanned   State = "planned"
	StateAnalyzing State = "analyzing"
	StateApproved  State = "approved"
	StateRunning   State = "running"
	StateVerifying State = "verifying"
	StateDone      State = "done"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
	StateBlocked   State = "blocked"
)

// Terminal reports whether no further transition is expected.
func (s State) Terminal() bool {
	switch s {
	case StateDone, StateFailed, StateCancelled, StateBlocked:
		return true
	}
	return false
}

// Approval records who allowed something risky, and why.
type Approval struct {
	Action          string `json:"action"`
	Actor           string `json:"actor"`
	Decision        string `json:"decision"`
	Note            string `json:"note,omitempty"`
	PlanFingerprint string `json:"plan_fingerprint"`
	At              string `json:"at"`
}

// StatementRecord is one executed (or planned) statement.
type StatementRecord struct {
	Description  string `json:"description"`
	SQL          string `json:"sql"`
	Algorithm    string `json:"algorithm"`
	Lock         string `json:"lock,omitempty"`
	DurationMS   int64  `json:"duration_ms"`
	RowsAffected int64  `json:"rows_affected"`
	Error        string `json:"error,omitempty"`
}

// VerifyRecord is one post-execution assertion.
type VerifyRecord struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Session is one migration attempt.
type Session struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	State     State  `json:"state"`
	Phase     string `json:"phase,omitempty"`

	Host       string `json:"host"`
	Database   string `json:"database"`
	Table      string `json:"table"`
	Production bool   `json:"production"`

	Alter             string `json:"alter"`
	Engine            string `json:"engine"`
	SchemaFingerprint string `json:"schema_fingerprint"`
	ServerVersion     string `json:"server_version"`

	Blockers int      `json:"blockers"`
	Warnings int      `json:"warnings"`
	Reasons  []string `json:"reasons,omitempty"`

	Plan         json.RawMessage   `json:"plan,omitempty"`
	Statements   []StatementRecord `json:"statements,omitempty"`
	Approvals    []Approval        `json:"approvals,omitempty"`
	Verification []VerifyRecord    `json:"verification,omitempty"`

	// ResumedFrom records that this execution followed an earlier plan.
	ResumedFrom string `json:"resumed_from,omitempty"`
	LastSeq     int64  `json:"last_seq"`
	Error       string `json:"error,omitempty"`
	Actor       string `json:"actor,omitempty"`
	CommandID   string `json:"command_id,omitempty"`
}

// Store is a directory of sessions.
type Store struct {
	dir string
}

// DefaultDir returns the per-user state directory.
func DefaultDir() string {
	if d := os.Getenv("SAKSAMA_STATE_DIR"); d != "" {
		return d
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".local", "state", "saksama")
	}
	return filepath.Join(os.TempDir(), "saksama-state")
}

// NewStore creates (and hardens) the state directory.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		dir = DefaultDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating state directory: %w", err)
	}
	// The state directory holds delete-capable SQL and session metadata.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("securing state directory: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir returns the state directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) sessionPath(id string) string {
	return filepath.Join(s.dir, id+".session.json")
}

// EventLogPath is where the append-only event stream for a session lives.
func (s *Store) EventLogPath(id string) string {
	return filepath.Join(s.dir, id+".events.jsonl")
}

// NextID returns an unused session identifier. Identifiers are short and
// human-typable because they appear in commands like `saksama status s3`.
func (s *Store) NextID() (string, error) {
	for i := 1; i < 1_000_000; i++ {
		id := fmt.Sprintf("s%d", i)
		if _, err := os.Stat(s.sessionPath(id)); errors.Is(err, os.ErrNotExist) {
			return id, nil
		}
	}
	return "", fmt.Errorf("no free session id")
}

// Save writes the session atomically.
func (s *Store) Save(sess *Session) error {
	sess.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding session: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-session-*")
	if err != nil {
		return fmt.Errorf("creating temp session file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("securing session file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing session: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("flushing session: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.sessionPath(sess.ID)); err != nil {
		return fmt.Errorf("publishing session: %w", err)
	}
	return nil
}

// Load reads one session.
func (s *Store) Load(id string) (*Session, error) {
	data, err := os.ReadFile(s.sessionPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("session %q not found in %s", id, s.dir)
		}
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, fmt.Errorf("session %q is corrupt: %w", id, err)
	}
	return &sess, nil
}

// List returns all sessions, newest first.
func (s *Store) List() ([]*Session, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []*Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".session.json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".session.json")
		sess, err := s.Load(id)
		if err != nil {
			continue
		}
		out = append(out, sess)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

// ReadEvents returns the raw JSONL event log for a session.
func (s *Store) ReadEvents(id string) ([]byte, error) {
	return os.ReadFile(s.EventLogPath(id))
}

// AppendEvents appends raw JSONL lines, creating the log if needed.
func (s *Store) AppendEvents(id string, lines []byte) error {
	f, err := os.OpenFile(s.EventLogPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(lines); err != nil {
		return err
	}
	return f.Sync()
}

// activeLock is written while a session owns a target table.
type activeLock struct {
	SessionID string `json:"session_id"`
	Table     string `json:"table"`
	At        string `json:"at"`
}

func (s *Store) lockPath(database, table string) string {
	key := strings.ToLower(database + "." + table)
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return filepath.Join(s.dir, "lock-"+b.String()+".json")
}

// Lock claims exclusive execution rights on one target table, so two sessions
// cannot rebuild the same table at once. A lock whose owner is no longer running
// is considered stale only when `staleAfter` has passed.
func (s *Store) Lock(database, table, sessionID string, staleAfter time.Duration) error {
	path := s.lockPath(database, table)

	if data, err := os.ReadFile(path); err == nil {
		var existing activeLock
		if json.Unmarshal(data, &existing) == nil {
			at, _ := time.Parse(time.RFC3339, existing.At)
			if existing.SessionID != sessionID && time.Since(at) < staleAfter {
				return fmt.Errorf("table %s.%s is already owned by session %s (started %s)",
					database, table, existing.SessionID, existing.At)
			}
		}
	}

	payload, err := json.Marshal(activeLock{SessionID: sessionID, Table: database + "." + table,
		At: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Unlock releases the table lock if this session owns it.
func (s *Store) Unlock(database, table, sessionID string) error {
	path := s.lockPath(database, table)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var existing activeLock
	if json.Unmarshal(data, &existing) == nil && existing.SessionID != sessionID {
		return fmt.Errorf("lock on %s.%s is owned by session %s", database, table, existing.SessionID)
	}
	return os.Remove(path)
}
