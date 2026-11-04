// Package probe discovers the online-schema-change binaries installed on this
// machine.
//
// It exists so the analyzer can stay pure: probing runs processes, deciding does
// not. The result is injected into the analysis input.
package probe

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/enginesetup"
)

// names are the binaries whose availability changes what the tool can recommend.
var names = []string{
	"gh-ost",
	"pt-online-schema-change",
	"perl",
}

// Timeout bounds each version probe. A hung binary must not hang the CLI.
const Timeout = 3 * time.Second

// Binaries finds and versions the known engine executables. Missing binaries are
// reported, not treated as errors.
func Binaries() map[string]analyzer.EngineBinary {
	out := make(map[string]analyzer.EngineBinary, len(names))
	for _, name := range names {
		out[name] = probeOne(name)
	}
	return out
}

// Result is a probe finding, kept separate from the analyzer type so the
// renderer can report local versions without depending on the decision layer.
type Result struct {
	Path      string
	Version   string
	Available bool
	Error     string
}

// Results probes the known binaries and reports plain results.
func Results() map[string]Result {
	out := make(map[string]Result, len(names))
	for _, name := range names {
		b := probeOne(name)
		out[name] = Result{Path: b.Path, Version: b.Version, Available: b.Available, Error: b.Error}
	}
	return out
}

func probeOne(name string) analyzer.EngineBinary {
	bin := analyzer.EngineBinary{Name: name}

	path, err := lookPath(name)
	if err != nil {
		bin.Error = "not found on PATH or in " + managedDir()
		return bin
	}
	bin.Path = path

	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "--version")
	// Percona Toolkit prints its version on stderr in some builds.
	raw, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		bin.Error = "version probe timed out"
		bin.Available = true // the binary exists; we just could not ask it
		return bin
	}
	if err != nil {
		// Some scanners exit non-zero for --version; still usable if we can read a
		// version-looking string.
		if v := parseVersion(string(raw)); v != "" {
			bin.Version = v
			bin.Available = true
			return bin
		}
		bin.Error = "version probe failed: " + err.Error()
		return bin
	}

	bin.Version = parseVersion(string(raw))
	if bin.Version == "" {
		bin.Version = "unknown"
	}
	bin.Available = true

	// pt-osc is a Perl script: the file can be present and still unable to start,
	// because it loads DBI and DBD::mysql at compile time. Reporting it as
	// available would hand the planner an engine that dies on the first run.
	if name == enginesetup.PtOSCBinary {
		if reason, ok := enginesetup.PerlDepsReason(); !ok {
			bin.Available = false
			bin.Error = "installed at " + path + ", but pt-osc cannot start: " + enginesetup.TidyPerlError(reason)
		}
	}
	return bin
}

// lookPath finds an engine, preferring the copy Saksama installed itself.
//
// The managed directory is checked first for two reasons: that copy is the exact
// version this build verified, and it lets an install work without the operator
// editing PATH. Everything else still comes from PATH.
func lookPath(name string) (string, error) {
	if dir, err := enginesetup.BinDir(); err == nil {
		candidate := filepath.Join(dir, name)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return exec.LookPath(name)
}

func managedDir() string {
	if dir, err := enginesetup.BinDir(); err == nil {
		return dir
	}
	return "the engine bin dir"
}

// parseVersion extracts the first dotted version number from probe output.
func parseVersion(out string) string {
	fields := strings.FieldsFunc(out, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\r' || r == '\t' || r == ',' || r == ')' || r == '('
	})
	for _, f := range fields {
		f = strings.TrimSpace(strings.TrimPrefix(f, "v"))
		parts := strings.Split(f, ".")
		if len(parts) < 2 {
			continue
		}
		ok := true
		for _, p := range parts {
			if p == "" {
				ok = false
				break
			}
			for _, r := range p {
				if r < '0' || r > '9' {
					ok = false
					break
				}
			}
			if !ok {
				break
			}
		}
		if ok {
			return f
		}
	}
	return ""
}
