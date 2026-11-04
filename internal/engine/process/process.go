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
	"sync"
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
}

// Result is the outcome of a run.
type Result struct {
	Argv     []string
	ExitCode int
	Started  time.Time
	Finished time.Time
	Err      error
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
