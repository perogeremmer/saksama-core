package process

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A started process must be identifiable by more than its number: the recorded
// command line is what stops a later cancel from signalling a reused pid.
func TestRunRecordsIdentity(t *testing.T) {
	var gotPID int
	var gotIdentity string
	res, err := Run(context.Background(), Spec{
		Argv: []string{"sleep", "0.1"},
		OnStart: func(pid int, identity string) {
			gotPID = pid
			gotIdentity = identity
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotPID == 0 {
		t.Fatal("OnStart never fired")
	}
	if !strings.Contains(gotIdentity, "sleep") {
		t.Fatalf("identity must name the command: %q", gotIdentity)
	}
	if !strings.Contains(res.Identity, "sleep") {
		t.Fatalf("result identity must be recorded: %q", res.Identity)
	}
}

// The identity check is what refuses to treat a reused pid as the engine.
func TestIsEngineComparesCommandLine(t *testing.T) {
	type start struct {
		pid      int
		identity string
	}
	started := make(chan start, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Run(context.Background(), Spec{
			Argv: []string{"sleep", "5"},
			OnStart: func(p int, id string) {
				started <- start{pid: p, identity: id}
			},
		}, nil)
	}()
	var s start
	select {
	case s = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the engine never started")
	}
	defer func() {
		_ = Stop(s.pid, time.Second)
		<-done
	}()

	if s.identity == "" {
		t.Skip("no /proc on this platform; identity cannot be verified here")
	}
	if !IsEngine(s.pid, s.identity) {
		t.Fatal("a live process answering to the recorded command must pass")
	}
	if IsEngine(s.pid, s.identity+" --tainted") {
		t.Fatal("a process whose command line differs must not pass as the engine")
	}
	// No recorded identity - an old record, or a platform without /proc - cannot
	// be checked, and must not be refused.
	if !IsEngine(999999, "") {
		t.Fatal("an empty identity cannot be verified and must not be refused")
	}
}
