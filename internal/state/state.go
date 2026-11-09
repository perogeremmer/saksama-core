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
	"syscall"
	"time"
)

// ErrSessionNotFound marks a session id that does not exist on disk. Frontends
// use it to answer a typo with a usage error instead of an internal one.
var ErrSessionNotFound = errors.New("session not found")

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

// ArtifactRecord is an object this session is allowed to delete.
//
// Ownership is recorded when the object is observed, never inferred from its name.
// A table called _orders_old might belong to a session, or might be something a
// person made on purpose; the name cannot tell you which.
type ArtifactRecord struct {
	Kind      string `json:"kind"` // table | trigger
	Name      string `json:"name"`
	Engine    string `json:"engine"`
	CreatedAt string `json:"created_at"`
	// SafeToDelete is set only once the session is terminal and the artifact is no
	// longer needed for recovery.
	SafeToDelete bool   `json:"safe_to_delete"`
	Note         string `json:"note,omitempty"`
}

// VerifyRecord is one post-execution assertion.
type VerifyRecord struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// FormatVersion is the version of the session document. It is written into every
// saved session so a future format change can tell which files it must migrate,
// instead of reading renamed fields as zero values. A session without the field
// predates versioning and is read as version 1.
const FormatVersion = 1

// Session is one migration attempt.
type Session struct {
	// FormatVersion records the on-disk shape of this document.
	FormatVersion int    `json:"format_version,omitempty"`
	ID            string `json:"id"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	State         State  `json:"state"`
	Phase         string `json:"phase,omitempty"`

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
	// Artifacts are the objects this session created and therefore owns.
	Artifacts []ArtifactRecord `json:"artifacts,omitempty"`

	// EnginePID is the copy engine's process id, so another command can stop it
	// even after the client that started it is gone.
	EnginePID int `json:"engine_pid,omitempty"`
	// EngineIdentity is what the process table said that pid was at start: its
	// command line. A pid is a number the kernel reuses, so a cancel checks the
	// command line before it signals anything.
	EngineIdentity string `json:"engine_identity,omitempty"`
	// CutoverFlagFile is the gate the engine waits on before swapping tables.
	CutoverFlagFile string `json:"cutover_flag_file,omitempty"`
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
//
// Prefer ClaimID: NextID only observes, so two concurrent prepares can both be
// told the same name is free.
func (s *Store) NextID() (string, error) {
	for i := 1; i < 1_000_000; i++ {
		id := fmt.Sprintf("s%d", i)
		if _, err := os.Stat(s.sessionPath(id)); errors.Is(err, os.ErrNotExist) {
			return id, nil
		}
	}
	return "", fmt.Errorf("no free session id")
}

// ClaimID returns an unused session identifier and atomically claims it by
// creating the session file. Two concurrent prepares cannot both believe they
// own the same name, and an id can never be handed out twice while its event
// log is already open.
func (s *Store) ClaimID() (string, error) {
	for i := 1; i < 1_000_000; i++ {
		id := fmt.Sprintf("s%d", i)
		f, err := os.OpenFile(s.sessionPath(id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			_ = syncDir(s.dir)
			return id, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("no free session id")
}

// lockSessionFile holds an exclusive advisory lock on a session's sidecar lock
// file, so a Load-modify-Save cycle in one process cannot lose another
// process's save in between. The lock file is never removed: flock semantics
// need the inode to stay put.
func (s *Store) lockSessionFile(id string) (func(), error) {
	f, err := os.OpenFile(s.sessionPath(id)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening session lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking session %s: %w", id, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Save writes the session atomically, under the session's file lock, and flushes
// the rename to disk. Without the directory sync, a crash after "successful"
// save can leave the old file behind on a filesystem that defers allocation.
func (s *Store) Save(sess *Session) error {
	release, err := s.lockSessionFile(sess.ID)
	if err != nil {
		return err
	}
	defer release()
	return s.saveLocked(sess)
}

func (s *Store) saveLocked(sess *Session) error {
	sess.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if sess.FormatVersion == 0 {
		sess.FormatVersion = FormatVersion
	}

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
	return syncDir(s.dir)
}

// Update runs fn against the session under the session's file lock: it loads,
// mutates, and saves without a window in which another process's save is lost.
// Every read-modify-write on a session that another command can touch at the
// same time (the running apply, a cancel from another terminal) belongs here.
func (s *Store) Update(id string, fn func(*Session) error) error {
	release, err := s.lockSessionFile(id)
	if err != nil {
		return err
	}
	defer release()
	sess, err := s.Load(id)
	if err != nil {
		return err
	}
	if err := fn(sess); err != nil {
		return err
	}
	return s.saveLocked(sess)
}

// Load reads one session.
func (s *Store) Load(id string) (*Session, error) {
	data, err := os.ReadFile(s.sessionPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q in %s", ErrSessionNotFound, id, s.dir)
		}
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, fmt.Errorf("session %q is corrupt: %w", id, err)
	}
	return &sess, nil
}

// List returns all sessions, newest first. Session files that exist but cannot
// be parsed are reported by Unreadable, not dropped silently: a corrupt record
// is exactly the thing reconcile exists to find.
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
		// A zero-length file is a claim that never became a session: ClaimID
		// creates it empty and the crash came before the first Save.
		if info, ierr := e.Info(); ierr == nil && info.Size() == 0 {
			continue
		}
		sess, err := s.Load(id)
		if err != nil {
			continue
		}
		out = append(out, sess)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

// Unreadable lists the session files whose content cannot be parsed. They are
// not an empty string of bad luck: they are the records of migrations this tool
// may have left behind, and hiding them would let reconcile report "nothing to
// do" while artifacts sit on the database.
func (s *Store) Unreadable() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".session.json") {
			continue
		}
		if info, ierr := e.Info(); ierr == nil && info.Size() == 0 {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".session.json")
		if _, err := s.Load(id); err != nil {
			out = append(out, id)
		}
	}
	return out, nil
}

// ReadEvents returns the raw JSONL event log for a session.
func (s *Store) ReadEvents(id string) ([]byte, error) {
	return os.ReadFile(s.EventLogPath(id))
}

// AppendEvents appends raw JSONL lines, creating the log if needed.
//
// A writer that crashed mid-line can leave the file without a trailing newline.
// Appending after that would glue the new event onto the fragment and destroy
// both, so the last byte is checked first and the fragment is given its own line.
func (s *Store) AppendEvents(id string, lines []byte) error {
	f, err := os.OpenFile(s.EventLogPath(id), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	if len(lines) > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], fSize(f)-1); err == nil && last[0] != '\n' {
			lines = append([]byte("\n"), lines...)
		}
	}
	if _, err := f.Write(lines); err != nil {
		return err
	}
	return f.Sync()
}

func fSize(f *os.File) int64 {
	info, err := f.Stat()
	if err != nil {
		return 0
	}
	return info.Size()
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
// cannot rebuild the same table at once.
//
// The claim is atomic: two processes that arrive together cannot both pass a
// read check and both rename. Only when the name already exists does the file's
// content decide - same-session reentrancy refreshes, a fresh lock refuses, and
// a stale one is taken over only after the owning session's engine has been
// confirmed dead, because "the lock is twelve hours old" and "the copy is
// thirteen hours in" are indistinguishable by the clock alone.
func (s *Store) Lock(database, table, sessionID string, staleAfter time.Duration) error {
	path := s.lockPath(database, table)
	payload, err := json.Marshal(activeLock{SessionID: sessionID, Table: database + "." + table,
		At: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		if _, werr := f.Write(payload); werr != nil {
			f.Close()
			return werr
		}
		if cerr := f.Close(); cerr != nil {
			return cerr
		}
		return syncDir(s.dir)
	}
	if !errors.Is(err, os.ErrExist) {
		return err
	}

	data, rerr := os.ReadFile(path)
	if rerr != nil {
		return rerr
	}
	var existing activeLock
	if err := json.Unmarshal(data, &existing); err != nil {
		// An unparseable lock still means the name is taken; replacing it here
		// would race whoever wrote it. Refusing is the safe direction.
		return fmt.Errorf("table %s.%s is locked, but the lock file is unreadable: %w",
			database, table, err)
	}
	if existing.SessionID == sessionID {
		// Re-entering a claim we already hold: refresh the timestamp.
		return s.writeLockFile(path, payload)
	}
	at, _ := time.Parse(time.RFC3339, existing.At)
	if time.Since(at) < staleAfter {
		return fmt.Errorf("table %s.%s is already owned by session %s (started %s)",
			database, table, existing.SessionID, existing.At)
	}
	if engineAlive(existing.SessionID, s) {
		return fmt.Errorf("table %s.%s is locked by session %s, whose engine process is still running",
			database, table, existing.SessionID)
	}
	return s.writeLockFile(path, payload)
}

func (s *Store) writeLockFile(path string, payload []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(s.dir)
}

// engineAlive reports whether the owning session's recorded engine process is
// still running. A stale lock is only safe to steal once nothing is behind it.
func engineAlive(ownerSessionID string, s *Store) bool {
	owner, err := s.Load(ownerSessionID)
	if err != nil || owner.EnginePID <= 0 {
		return false
	}
	p, err := os.FindProcess(owner.EnginePID)
	if err != nil {
		return false
	}
	// Signal 0 asks whether the process exists without touching it.
	return p.Signal(syscall.Signal(0)) == nil
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

// syncDir flushes a rename to the filesystem. The file's own Sync does not
// promise the rename that published it: on a filesystem that defers allocation,
// a crash after "successful" save can leave the old file behind.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
