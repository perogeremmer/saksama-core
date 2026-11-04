package events

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSeqIsMonotonic(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "s1")
	for i := 0; i < 3; i++ {
		if _, err := w.Emit(TypePhaseStarted, map[string]any{"phase": "copy"}); err != nil {
			t.Fatal(err)
		}
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines", len(lines))
	}
	for i, line := range lines {
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d is not JSON: %v", i, err)
		}
		if ev.Seq != int64(i+1) {
			t.Fatalf("line %d has seq %d", i, ev.Seq)
		}
		if ev.ContractVersion != ContractVersion {
			t.Fatalf("bad contract version %d", ev.ContractVersion)
		}
		if ev.Session != "s1" {
			t.Fatalf("bad session %q", ev.Session)
		}
	}
}

func TestRedactRemovesCredentialKeys(t *testing.T) {
	got := Redact(map[string]any{
		"password": "hunter2",
		"PASSWORD": "hunter2",
		"user":     "root",
		"token":    "abc",
		"api_key":  "k",
		"normal":   "visible",
	})
	for _, k := range []string{"password", "PASSWORD", "token", "api_key"} {
		if got[k] != Redacted {
			t.Errorf("%s not redacted: %v", k, got[k])
		}
	}
	if got["user"] != "root" || got["normal"] != "visible" {
		t.Errorf("non-sensitive values were altered: %+v", got)
	}
}

func TestRedactRecursesIntoNestedStructures(t *testing.T) {
	got := Redact(map[string]any{
		"engine": map[string]any{"password": "hunter2", "argv": []any{"gh-ost", "--password=hunter2"}},
	})
	engine := got["engine"].(map[string]any)
	if engine["password"] != Redacted {
		t.Fatalf("nested key not redacted: %v", engine)
	}
	argv := engine["argv"].([]any)
	if argv[1] == "--password=hunter2" {
		t.Fatalf("nested string not redacted: %v", argv)
	}
}

func TestRedactStringScrubsInlineCredentials(t *testing.T) {
	cases := []struct{ in, want string }{
		{"--password=hunter2", "--password=" + Redacted},
		{"password = hunter2", "password =" + Redacted},
		{"mysql://root:hunter2@db:3306/shop", "mysql://root:[REDACTED]@db:3306/shop"},
	}
	for _, tc := range cases {
		if got := RedactString(tc.in); got != tc.want {
			t.Errorf("RedactString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// A connection string using :[REDACTED]@ must still parse as one replacement.
	if got := RedactString("mysql://root:hunter2@db/shop"); strings.Contains(got, "hunter2") {
		t.Fatalf("password survived: %q", got)
	}
}

func TestRedactLeavesOrdinarySQLAlone(t *testing.T) {
	sql := "ALTER TABLE `orders` ADD INDEX `ix` (`created_at`)"
	if got := RedactString(sql); got != sql {
		t.Fatalf("SQL was mangled: %q", got)
	}
}

func TestProgressEventsAreRateLimited(t *testing.T) {
	var buf bytes.Buffer
	now := time.Unix(1000, 0)
	w := NewWriter(&buf, "s1").WithClock(func() time.Time { return now })

	for i := 0; i < 5; i++ {
		if _, err := w.Emit(TypePhaseProgress, map[string]any{"phase": "copy", "pct": i}); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Fatalf("got %d progress events, want 1", n)
	}

	now = now.Add(1100 * time.Millisecond)
	if _, err := w.Emit(TypePhaseProgress, map[string]any{"phase": "copy", "pct": 5}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "\n"); n != 2 {
		t.Fatalf("got %d progress events after interval, want 2", n)
	}
}

func TestProgressRateLimitIsPerPhase(t *testing.T) {
	var buf bytes.Buffer
	now := time.Unix(1000, 0)
	w := NewWriter(&buf, "s1").WithClock(func() time.Time { return now })

	_, _ = w.Emit(TypePhaseProgress, map[string]any{"phase": "copy"})
	_, _ = w.Emit(TypePhaseProgress, map[string]any{"phase": "index_build"})
	if n := strings.Count(buf.String(), "\n"); n != 2 {
		t.Fatalf("got %d events, want 2 (one per phase)", n)
	}
}

func TestNonProgressEventsAreNeverRateLimited(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "s1")
	for i := 0; i < 3; i++ {
		_, _ = w.Emit(TypePhaseStarted, map[string]any{"phase": "copy"})
	}
	if n := strings.Count(buf.String(), "\n"); n != 3 {
		t.Fatalf("got %d events, want 3", n)
	}
}

func TestEmitReturnsSequencedEvent(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "s1")
	ev, err := w.Emit(TypePlanComputed, map[string]any{"engine": "native", "password": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 1 || ev.Type != TypePlanComputed {
		t.Fatalf("bad event: %+v", ev)
	}
	if ev.Data["password"] != Redacted {
		t.Fatalf("returned event leaked a credential: %+v", ev.Data)
	}
	if w.Seq() != 1 {
		t.Fatalf("Seq() = %d", w.Seq())
	}
}
