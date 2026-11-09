package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/enginesetup"
	"github.com/perogeremmer/saksama-core/internal/introspect"
	"github.com/perogeremmer/saksama-core/internal/probe"
	"github.com/perogeremmer/saksama-core/internal/render"
)

// cmdEngineDoctor answers "can this engine run here?" for both sides of the
// problem: the local binary and the server it will talk to. It changes nothing.
func cmdEngineDoctor(args []string, stdout, stderr io.Writer) int {
	var o connFlags
	var jsonOut, noProbe, plain bool

	fs := flag.NewFlagSet("engine doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addConnFlags(fs, &o)
	fs.BoolVar(&jsonOut, "json", false, "JSON output")
	fs.BoolVar(&noProbe, "no-probe", false, "do not probe installed engine binaries")
	fs.BoolVar(&plain, "plain", false, "borderless output, for scripts and logs")
	fs.Bool("no-color", false, "never use colour (already off for a pipe, NO_COLOR, TERM=dumb)")

	// The engine name is positional and people type it first, before the flags, so
	// it is lifted out before the flag package can stop at it.
	positional, flagArgs := extractPositionals(args, map[string]bool{
		"h": true, "host": true, "P": true, "port": true, "u": true, "user": true,
		"d": true, "database": true, "tls": true, "defaults-extra-file": true,
	})
	if err := fs.Parse(flagArgs); err != nil {
		return ExitUsage
	}

	requested := positional
	if len(requested) == 0 {
		requested = []string{enginesetup.GhOst, enginesetup.PtOSC}
	}

	var binaries map[string]analyzer.EngineBinary
	if !noProbe {
		binaries = probe.Binaries()
	}

	settings, live, code := doctorServerSettings(o, stderr)
	if code != 0 {
		return code
	}

	lists := make([]enginesetup.Checklist, 0, len(requested))
	for _, name := range requested {
		if _, err := enginesetup.Normalize(name); err != nil {
			fmt.Fprintf(stderr, "saksama engine doctor: %v\n", err)
			return ExitUsage
		}
		lists = append(lists, enginesetup.Check(name, binaries, settings, live))
	}

	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"target": enginesetup.ThisTarget().String(), "engines": lists}); err != nil {
			fmt.Fprintf(stderr, "saksama engine doctor: %v\n", err)
			return ExitInternal
		}
	} else if err := render.Doctor(stdout, lists, !plain); err != nil {
		fmt.Fprintf(stderr, "saksama engine doctor: %v\n", err)
		return ExitInternal
	}

	for _, l := range lists {
		if !l.Ready {
			return ExitBlocked
		}
	}
	return ExitOK
}

// doctorServerSettings reads the settings the engine checks need, when a host was
// given. Without one the report says the server was not checked, rather than
// pretending the prerequisites are met.
func doctorServerSettings(o connFlags, stderr io.Writer) (analyzer.ServerSettings, bool, int) {
	if o.host == "" {
		return analyzer.ServerSettings{}, false, 0
	}
	conn, err := o.resolve()
	if err != nil {
		fmt.Fprintf(stderr, "saksama engine doctor: %v\n", err)
		return analyzer.ServerSettings{}, false, ExitUsage
	}
	db, err := openDB(conn)
	if err != nil {
		fmt.Fprintf(stderr, "saksama engine doctor: %v\n", err)
		return analyzer.ServerSettings{}, false, ExitInternal
	}
	defer db.Close()

	raw, err := introspect.NewCollector(db, o.database, "").Settings(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "saksama engine doctor: %v\n", err)
		return analyzer.ServerSettings{}, false, ExitInternal
	}
	return analyzer.SettingsFromMap(raw), true, 0
}

// cmdEngineInstall fetches the verified build for this machine.
//
// It is the only command that downloads and writes an executable, so it is never
// reached implicitly, and it says what it is about to do before it does it.
func cmdEngineInstall(args []string, stdout, stderr io.Writer) int {
	var dir string
	var force, system, yes, latest, jsonOut bool

	fs := flag.NewFlagSet("engine install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&dir, "dir", "", "install into this directory instead of the Saksama bin dir")
	fs.BoolVar(&force, "force", false, "replace an engine that is already installed")
	fs.BoolVar(&system, "system", false, "install into /usr/local/bin (needs write access)")
	fs.BoolVar(&yes, "yes", false, "skip the confirmation for a system install")
	fs.BoolVar(&latest, "latest", false, "install a version newer than the verified one")
	fs.BoolVar(&jsonOut, "json", false, "JSON output")

	// Flags may follow the engine name, which is how people type it.
	positional, flagArgs := extractPositionals(args, map[string]bool{"dir": true})
	if err := fs.Parse(flagArgs); err != nil {
		return ExitUsage
	}
	if latest {
		fmt.Fprintf(stderr, "saksama engine install: --latest is not implemented in this build\n")
		fmt.Fprintf(stderr, "this build installs the version internal/support marks verified, so docs/COMPATIBILITY.md stays true\n")
		return ExitUsage
	}
	if len(positional) != 1 {
		fmt.Fprintf(stderr, "saksama engine install: name one engine (gh-ost or pt-osc)\n")
		return ExitUsage
	}

	engine, err := enginesetup.Normalize(positional[0])
	if err != nil {
		fmt.Fprintf(stderr, "saksama engine install: %v\n", err)
		return ExitUsage
	}

	target := enginesetup.ThisTarget()
	m, err := enginesetup.ManifestFor(engine, target)
	if err != nil {
		fmt.Fprintf(stderr, "saksama engine install: %v\n", err)
		return ExitBlocked
	}

	if dir == "" {
		if system {
			dir = "/usr/local/bin"
		} else if dir, err = enginesetup.BinDir(); err != nil {
			fmt.Fprintf(stderr, "saksama engine install: %v\n", err)
			return ExitInternal
		}
	}
	if system && !yes {
		fmt.Fprintf(stderr, "saksama engine install: --system writes to %s, outside anything Saksama manages\n", dir)
		fmt.Fprintf(stderr, "re-run with --yes, or install into the Saksama bin dir instead (no sudo needed)\n")
		return ExitUsage
	}

	if !jsonOut {
		fmt.Fprintf(stdout, "%s %s for %s\n", m.Engine, m.Version, m.Target)
		fmt.Fprintf(stdout, "  source   %s\n", m.URL)
		fmt.Fprintf(stdout, "  target   %s\n", filepath.Join(dir, m.Binary))
		if m.Sums != "" {
			fmt.Fprintf(stdout, "  checksum %s\n", m.Sums)
		} else {
			fmt.Fprintf(stdout, "  checksum %s\n", m.SumsURL)
		}
	}

	result, err := enginesetup.Install(context.Background(), m, enginesetup.InstallOptions{Dir: dir, Force: force})
	if err != nil {
		// A refused install is a decision, not a crash: a digest that does not
		// match, a version that does not match, or a file already in the way.
		fmt.Fprintf(stderr, "saksama engine install: %v\n", err)
		return ExitBlocked
	}

	// Readiness is reported from a fresh probe, so what follows is what the rest
	// of the tool will actually see.
	check := enginesetup.Check(engine, probe.Binaries(), analyzer.ServerSettings{}, false)

	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"install": result, "check": check}); err != nil {
			fmt.Fprintf(stderr, "saksama engine install: %v\n", err)
			return ExitInternal
		}
	} else {
		fmt.Fprintf(stdout, "\nINSTALLED\n")
		fmt.Fprintf(stdout, "  path     %s\n", result.Path)
		fmt.Fprintf(stdout, "  digest   %s (matches %s)\n", result.Digest, result.ChecksumSource)
		fmt.Fprintf(stdout, "  version  %s, reported by the binary itself\n", result.Version)
	}

	if err := render.Doctor(stdout, []enginesetup.Checklist{check}, true); err != nil {
		fmt.Fprintf(stderr, "saksama engine install: %v\n", err)
		return ExitInternal
	}

	if check.Ready {
		if !jsonOut {
			fmt.Fprintf(stdout, "\n  next: saksama engine doctor %s -h <host> -u <user>\n", engine)
			fmt.Fprintf(stdout, "        to check the server side too\n")
		}
		return ExitOK
	}
	if !jsonOut {
		fmt.Fprintf(stdout, "\n%s is in place but not runnable yet; the FAIL lines above say what is missing\n", engine)
	}
	return ExitBlocked
}
