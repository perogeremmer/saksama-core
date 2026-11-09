// Package process runs an external migration engine as a child process.
//
// The wrapper owns the lifecycle: it starts the engine, streams its output for
// diagnosis, and never treats that output as a progress source. Progress comes
// from polling the database, because engine log formats change between versions
// and parsing them is how a wrapper breaks on an upgrade.
package process

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Spec describes one invocation.
type Spec struct {
	Argv []string
	// Env adds to the inherited environment. Never place a secret here expecting
	// it to be hidden: a child process environment is readable the same way argv is.
	Env []string
	// Timeout bounds the whole run. Zero means no limit.
	Timeout time.Duration
	// Detach puts the engine in its own process group, so closing the terminal or
	// killing the client does not take the migration down with it. The child is
	// still our child: cmd.Wait still works, and a recorded PID lets another
	// command stop it later.
	Detach bool
	// OnStart receives the child PID, and what the process table says the process
	// is, as soon as both are known, so they can be recorded together before the
	// long part of the run begins.
	OnStart func(pid int, identity string)
}

// Result is the outcome of a run.
type Result struct {
	Argv     []string
	ExitCode int
	Started  time.Time
	Finished time.Time
	Err      error
	// Identity is what the process table said this process was at start: its
	// command line. It is recorded next to the PID so a later cancel can check
	// that the number still points at the same command before signalling it.
	Identity string
}

// Line is one line of engine output.
type Line struct {
	Stream string // stdout | stderr
	Text   string
}

// Run starts the command and streams output. onLine may be nil.
func Run(ctx context.Context, spec Spec, onLine func(Line)) (Result, error) {
	res := Result{Argv: spec.Argv, Started: time.Now()}

	// A nil context must not panic: callers build these from flags, and losing a
	// migration because of a nil dereference is not an acceptable failure mode.
	if ctx == nil {
		ctx = context.Background()
	}

	if len(spec.Argv) == 0 {
		res.Err = fmt.Errorf("no command given")
		res.Finished = time.Now()
		return res, res.Err
	}

	bin, err := exec.LookPath(spec.Argv[0])
	if err != nil {
		res.Err = fmt.Errorf("%s is not installed or not on PATH", spec.Argv[0])
		res.Finished = time.Now()
		return res, res.Err
	}

	runCtx := ctx
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, bin, spec.Argv[1:]...)
	cmd.Env = append(os.Environ(), spec.Env...)
	if spec.Detach {
		// A new session means no controlling terminal, so an SSH hangup does not
		// deliver SIGHUP to the engine.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		res.Err = err
		res.Finished = time.Now()
		return res, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		res.Err = err
		res.Finished = time.Now()
		return res, err
	}

	if err := cmd.Start(); err != nil {
		res.Err = err
		res.Finished = time.Now()
		return res, err
	}
	if cmd.Process != nil {
		res.Identity = waitForIdentity(cmd.Process.Pid)
		if spec.OnStart != nil {
			spec.OnStart(cmd.Process.Pid, res.Identity)
		}
	}

	var wg sync.WaitGroup
	pump := func(r io.Reader, stream string) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			if onLine != nil {
				onLine(Line{Stream: stream, Text: sc.Text()})
			}
		}
	}
	wg.Add(2)
	go pump(stdout, "stdout")
	go pump(stderr, "stderr")
	wg.Wait()

	res.Err = cmd.Wait()
	res.Finished = time.Now()
	if ee, ok := res.Err.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else if res.Err == nil {
		res.ExitCode = 0
	} else {
		res.ExitCode = -1
	}
	if runCtx.Err() == context.DeadlineExceeded {
		res.Err = fmt.Errorf("engine timed out after %s", spec.Timeout)
	}
	return res, res.Err
}

// Stop sends SIGTERM to a detached engine and waits briefly for it to leave.
//
// It is used by `saksama cancel`, which runs in a different process from the one
// that started the engine, so the PID is the only handle available.
func Stop(pid int, wait time.Duration) error {
	if pid <= 0 {
		return fmt.Errorf("no process id recorded")
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("process %d is gone: %w", pid, err)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("cannot signal process %d: %w", pid, err)
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if p.Signal(syscall.Signal(0)) != nil || isZombie(pid) {
			return nil // gone, or waiting to be reaped
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("process %d did not stop within %s", pid, wait)
}

// Alive reports whether a process id is still running.
// StopForce is Stop with an escalation: after the graceful window it sends SIGKILL.
//
// SIGKILL is not the default anywhere. A copy engine that is killed outright
// leaves its shadow table, its triggers, and its changelog behind, and it never
// gets the chance to drop them; asking is the difference between a tool that
// cleans up and a tool that hides debris. It exists for the case a graceful stop
// cannot fix: a process wedged in a metadata lock wait.
func StopForce(pid int, wait time.Duration) (forced bool, err error) {
	if pid <= 0 {
		return false, fmt.Errorf("no process id recorded")
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false, fmt.Errorf("process %d is gone: %w", pid, err)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return false, fmt.Errorf("cannot signal process %d: %w", pid, err)
	}
	if waitForExit(p, pid, wait) {
		return false, nil
	}
	if err := p.Signal(syscall.SIGKILL); err != nil {
		return true, fmt.Errorf("cannot kill process %d: %w", pid, err)
	}
	if waitForExit(p, pid, stopKillWindow) {
		return true, nil
	}
	return true, fmt.Errorf("process %d did not die even after SIGKILL; it is likely stuck in the kernel", pid)
}

// stopKillWindow is how long SIGKILL is given. A killed process is reaped almost
// immediately; anything longer means it is unkillable, and waiting is pointless.
const stopKillWindow = 5 * time.Second

func waitForExit(p *os.Process, pid int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if p.Signal(syscall.Signal(0)) != nil || isZombie(pid) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return p.Signal(syscall.Signal(0)) != nil || isZombie(pid)
}

// Alive reports whether a process id is still running.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if isZombie(pid) {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// IsEngine reports whether the process now holding pid still answers to the
// command line recorded when it was started. Linux reuses pids, and signalling
// whoever owns the number now is worse than not signalling at all. An empty
// identity - no /proc on this platform, or a record from before identity capture
// - cannot be checked, and the old behaviour applies.
func IsEngine(pid int, identity string) bool {
	if identity == "" {
		return true
	}
	got := identityOf(pid)
	if got == "" {
		return true
	}
	return got == identity
}

// waitForIdentity reads the child's command line, retrying briefly: the child
// may not have been scheduled yet when Start returns, and an empty command line
// would defeat the very check the identity exists for.
func waitForIdentity(pid int) string {
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		if id := identityOf(pid); id != "" {
			return id
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// identityOf reads the command line of a process. It is empty wherever there is
// no /proc, which is the signal for every caller that verification is not
// possible here.
func identityOf(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	return strings.Join(parts, " ")
}
