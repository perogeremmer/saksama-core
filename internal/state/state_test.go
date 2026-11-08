package state

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s
}

func TestNewStoreHardensPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	info, err := os.Stat(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Fatalf("state dir mode = %#o, want 0700", mode)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := newStore(t)
	sess := &Session{
		ID: "s1", CreatedAt: "2026-01-01T00:00:00Z", State: StateRunning,
		Host: "db", Database: "shop", Table: "orders", Alter: "ADD INDEX ix (a)",
		Engine: "native", Blockers: 0,
	}
	if err := s.Save(sess); err != nil {
		t.Fatalf("save: %v", err)
	}

	info, err := os.Stat(filepath.Join(s.Dir(), "s1.session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("session file mode = %#o, want 0600", mode)
	}

	loaded, err := s.Load("s1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Table != "orders" || loaded.State != StateRunning {
		t.Fatalf("round trip lost data: %+v", loaded)
	}
	if loaded.UpdatedAt == "" {
		t.Fatal("UpdatedAt not set on save")
	}
}

func TestLoadMissingSession(t *testing.T) {
	s := newStore(t)
	_, err := s.Load("nope")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestNextIDSkipsExisting(t *testing.T) {
	s := newStore(t)
	id1, err := s.NextID()
	if err != nil {
		t.Fatal(err)
	}
	if id1 != "s1" {
		t.Fatalf("first id = %q", id1)
	}
	if err := s.Save(&Session{ID: id1}); err != nil {
		t.Fatal(err)
	}
	id2, err := s.NextID()
	if err != nil {
		t.Fatal(err)
	}
	if id2 != "s2" {
		t.Fatalf("second id = %q", id2)
	}
}

func TestListReturnsNewestFirst(t *testing.T) {
	s := newStore(t)
	for _, sess := range []*Session{
		{ID: "s1", CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "s2", CreatedAt: "2026-03-01T00:00:00Z"},
		{ID: "s3", CreatedAt: "2026-02-01T00:00:00Z"},
	} {
		if err := s.Save(sess); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	got := []string{list[0].ID, list[1].ID, list[2].ID}
	want := []string{"s2", "s3", "s1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestLockIsExclusivePerTable(t *testing.T) {
	s := newStore(t)
	if err := s.Lock("shop", "orders", "s1", time.Hour); err != nil {
		t.Fatalf("first lock: %v", err)
	}
	// Same session may refresh.
	if err := s.Lock("shop", "orders", "s1", time.Hour); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// Another session may not.
	err := s.Lock("shop", "orders", "s2", time.Hour)
	if err == nil {
		t.Fatal("expected the second lock to be refused")
	}
	if !strings.Contains(err.Error(), "already owned by session s1") {
		t.Fatalf("unexpected error: %v", err)
	}
	// A different table is independent.
	if err := s.Lock("shop", "other", "s2", time.Hour); err != nil {
		t.Fatalf("unrelated table lock: %v", err)
	}
}

func TestLockAllowsTakingOverStaleLock(t *testing.T) {
	s := newStore(t)
	if err := s.Lock("shop", "orders", "s1", time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := s.Lock("shop", "orders", "s2", time.Millisecond); err != nil {
		t.Fatalf("stale lock should be recoverable: %v", err)
	}
}

func TestUnlockOnlyByOwner(t *testing.T) {
	s := newStore(t)
	if err := s.Lock("shop", "orders", "s1", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.Unlock("shop", "orders", "s2"); err == nil {
		t.Fatal("expected refusal when a different session unlocks")
	}
	if err := s.Unlock("shop", "orders", "s1"); err != nil {
		t.Fatalf("owner unlock: %v", err)
	}
	if err := s.Lock("shop", "orders", "s2", time.Hour); err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
}

func TestUnlockMissingLockIsNotAnError(t *testing.T) {
	s := newStore(t)
	if err := s.Unlock("shop", "orders", "s1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEventLogAppendAndRead(t *testing.T) {
	s := newStore(t)
	if err := s.AppendEvents("s1", []byte("{\"seq\":1}\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvents("s1", []byte("{\"seq\":2}\n")); err != nil {
		t.Fatal(err)
	}
	data, err := s.ReadEvents("s1")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "\n"); got != 2 {
		t.Fatalf("lines = %d, want 2", got)
	}
}

func TestOpenEventLogUsesStrictPermissions(t *testing.T) {
	s := newStore(t)
	f, err := s.OpenEventLog("s1")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("event log mode = %#o, want 0600", mode)
	}
}

func TestStateTerminal(t *testing.T) {
	for _, st := range []State{StateDone, StateFailed, StateCancelled, StateBlocked} {
		if !st.Terminal() {
			t.Errorf("%s should be terminal", st)
		}
	}
	for _, st := range []State{StatePlanned, StateRunning, StateVerifying, StateApproved} {
		if st.Terminal() {
			t.Errorf("%s should not be terminal", st)
		}
	}
}

// Two prepares arriving together cannot both believe they own the same name:
// the claim is the file's creation, not an observation about it.
func TestClaimIDIsAtomic(t *testing.T) {
	s := newStore(t)
	const n = 8
	ids := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			id, err := s.ClaimID()
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		}()
	}
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		select {
		case id := <-ids:
			if seen[id] {
				t.Fatalf("id %s was handed out twice", id)
			}
			seen[id] = true
		case err := <-errs:
			t.Fatal(err)
		}
	}
}

// A lock past its stale window is still not stealable while the owning
// session's engine process is alive: thirteen hours into a copy must not read
// as "abandoned".
func TestStaleLockRefusedWhileEngineLives(t *testing.T) {
	s := newStore(t)
	owner := &Session{ID: "s1", CreatedAt: time.Now().UTC().Format(time.RFC3339), EnginePID: os.Getpid()}
	if err := s.Save(owner); err != nil {
		t.Fatal(err)
	}
	if err := s.Lock("shop", "orders", "s1", time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	err := s.Lock("shop", "orders", "s2", time.Nanosecond)
	if err == nil {
		t.Fatal("a lock with a live engine must not be stolen however old it is")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A corrupt session file vanishing from List is how a damaged record of a
// half-finished migration disappears from reconcile. It must be findable.
func TestUnreadableSurfacesCorruptSessions(t *testing.T) {
	s := newStore(t)
	sess := &Session{ID: "s1", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := s.Save(sess); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.sessionPath("s2"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A zero-byte file is a claim that never became a session, not corruption.
	if err := os.WriteFile(s.sessionPath("s3"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	listed, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != "s1" {
		t.Fatalf("list = %v", listed)
	}
	bad, err := s.Unreadable()
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 1 || bad[0] != "s2" {
		t.Fatalf("unreadable = %v, want [s2]", bad)
	}
}

// Appending after a writer crashed mid-line must not glue the next event onto
// the fragment: both lines would be lost.
func TestAppendEventsSeparatesPartialLine(t *testing.T) {
	s := newStore(t)
	if err := s.AppendEvents("s1", []byte(`{"seq":1,"type":"phase.started"`)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvents("s1", []byte(`{"seq":2,"type":"phase.completed"}`+"\n")); err != nil {
		t.Fatal(err)
	}
	raw, err := s.ReadEvents("s1")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("the fragment and the event must stay their own lines, got %d: %q", len(lines), string(raw))
	}
}

// Concurrent read-modify-write cycles through Update all land: the flock makes
// a cancel from another terminal unable to erase what the running apply saved.
func TestUpdateSerialisesReadModifyWrite(t *testing.T) {
	s := newStore(t)
	if err := s.Save(&Session{ID: "s1", CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Update("s1", func(sess *Session) error {
				sess.Statements = append(sess.Statements, StatementRecord{Description: "x"})
				return nil
			})
		}()
	}
	wg.Wait()
	sess, err := s.Load("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Statements) != n {
		t.Fatalf("%d of %d updates survived", len(sess.Statements), n)
	}
}

func TestSaveWritesFormatVersion(t *testing.T) {
	s := newStore(t)
	if err := s.Save(&Session{ID: "s1", CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	sess, err := s.Load("s1")
	if err != nil {
		t.Fatal(err)
	}
	if sess.FormatVersion != FormatVersion {
		t.Fatalf("format_version = %d, want %d", sess.FormatVersion, FormatVersion)
	}
}
