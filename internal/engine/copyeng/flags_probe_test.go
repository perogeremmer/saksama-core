//go:build probe

package copyeng

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// The engine wrappers emit flags with fixed spellings, and a copy engine rejects
// the whole run on one unknown flag: "flag provided but not defined". That
// failure was once shipped as release candidate 1: --postpone-cutover-file does
// not exist in any gh-ost release, but the integration stubs are /bin/sh and
// accept anything.
//
// These tests compare, flag by flag, what the wrappers emit against a real
// engine binary, using gh-ost's own --check-flag for cross-version scripting and
// --help parsing for pt-osc. They run with `go test -tags=probe` and skip when a
// binary is not supplied:
//
//	SAKSAMA_GH_OST_BIN=/path/to/gh-ost  go test -tags=probe ./internal/engine/copyeng/
//	SAKSAMA_PT_OSC_BIN=/path/to/pt-online-schema-change go test -tags=probe ./internal/engine/copyeng/
//
// The CI job "engine-flag-matrix" runs them against gh-ost 1.1.11 and pt-osc
// 3.7.2, the versions docs/COMPATIBILITY.md claims as verified.

func probeBinary(t *testing.T, envVar string) string {
	t.Helper()
	path := os.Getenv(envVar)
	if path == "" {
		t.Skipf("%s is not set; supply a real engine binary to run the flag probe", envVar)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s points at %q which does not exist: %v", envVar, path, err)
	}
	return path
}

func flagsOf(argv []string) map[string]string {
	// Flags in our argv appear as --name=value, as "--name value", or alone.
	out := map[string]string{}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "--") {
			continue
		}
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			out[a[:eq]] = a[eq+1:]
			continue
		}
		if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
			out[a] = argv[i+1]
			i++
			continue
		}
		out[a] = ""
	}
	return out
}

// ghOstFlagsExistInBinary asks the binary itself with --check-flag, in one batch,
// using the same values this wrapper would send at runtime: the flag must
// survive its own type parsing, not merely exist. A batch failure falls back to
// one flag per invocation, so the report names every missing flag, and its error
// must be about the flag itself rather than the value.
func ghOstFlagsExistInBinary(t *testing.T, bin string, argv []string) []string {
	t.Helper()
	flags := flagsOf(argv)

	// One pass, all at once: booleans bare, value flags as --name=value.
	args := append([]string{"--check-flag"}, dedupFlags(flags)...)
	if cmd := exec.Command(bin, args...); cmd.Run() == nil {
		return nil
	}

	var missing []string
	for name, value := range flags {
		probe := "--" + strings.TrimPrefix(name, "--")
		if value != "" {
			probe = probe + "=" + valueARGS(value)
		}
		if cmd := exec.Command(bin, "--check-flag", probe); cmd.Run() != nil {
			missing = append(missing, name)
		}
	}
	return missing
}

// dedupFlags converts the flag map into a check-flag argument list.
func dedupFlags(flags map[string]string) []string {
	var out []string
	for name, value := range flags {
		if value != "" {
			out = append(out, "--"+strings.TrimPrefix(name, "--")+"="+valueARGS(value))
		} else {
			out = append(out, "--"+strings.TrimPrefix(name, "--"))
		}
	}
	return out
}

// valueARGS keeps the argument shape we normally send. Check-flag only parses
// types; contents are irrelevant, so a placeholder that parses is safest.
func valueARGS(v string) string {
	if v == "" {
		return ""
	}
	// Int-typed flags cannot take arbitrary strings; keep digit-only values,
	// and anything else is a string flag already, which parses as quoted text.
	isInt := true
	for _, r := range v {
		if r < '0' || r > '9' {
			isInt = false
			break
		}
	}
	if isInt {
		return v
	}
	return "probe"
}

func ptOscFlagsExistInBinary(t *testing.T, bin string, flags map[string]string) []string {
	t.Helper()
	out, err := exec.Command(bin, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("%s --help failed: %v (%s)", bin, err, out)
	}
	help := string(out)
	var missing []string
	for name := range flags {
		// pt-osc lists options as "--name[=value]", and boolean-or-no variants
		// as "--[no]name", which is how "--no-version-check" appears. Our DSN
		// tail and the binary path are not options and carry no -- prefix.
		if !ptHelpKnows(help, name) {
			missing = append(missing, name)
		}
	}
	return missing
}

// ptHelpKnows reports whether a flag appears in pt-osc --help. It knows about
// the "[no]" convention: "--[no]version-check" documents "--no-version-check".
func ptHelpKnows(help, flag string) bool {
	base := strings.TrimPrefix(flag, "--")
	if no := strings.TrimPrefix(base, "no-"); no != base {
		return strings.Contains(help, "--[no]"+no)
	}
	for _, form := range []string{flag + "=", flag + " ", flag + ",", flag + "\n"} {
		if strings.Contains(help, form) {
			return true
		}
	}
	return false
}

func TestGhOstEmitsOnlyFlagsTheBinaryKnows(t *testing.T) {
	bin := probeBinary(t, "SAKSAMA_GH_OST_BIN")

	versions, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v (%s)", bin, err, versions)
	}
	t.Logf("probing gh-ost: %s", strings.TrimSpace(string(versions)))

	// The plain argv, the one with a cutover gate, and the one connecting
	// through a mapped port, which adds --skip-port-validation: that flag
	// appeared only in gh-ost 1.1.8, and it is the whole reason the minimum is
	// what docs/COMPATIBILITY.md says it is.
	variants := []struct {
		name  string
		hold  bool
		mPort string
	}{
		{"plain", false, ""},
		{"hold-cutover", true, ""},
		{"mapped-port", false, "5555"},
	}
	for _, v := range variants {
		opts := testOptions()
		opts.HoldCutover = v.hold
		opts.CutoverGate = "/tmp/saksama-probe-gate.cutover"
		probeConn := conn()
		if v.mPort != "" {
			// The server really listens on 5555; we connect through 3306.
			probeConn.ServerPort = 5555
		}
		e := New(GhOst(), nil, probeConn, opts, nil)
		stmts, err := e.Statements()
		if err != nil {
			t.Fatalf("statements: %v", err)
		}
		argv := stmts[0].Argv
		flags := flagsOf(argv)
		t.Logf("variant %s: %v", v.name, argv)
		if v.hold {
			if _, ok := flags["--postpone-cut-over-flag-file"]; !ok {
				t.Fatalf("hold-cutover must emit --postpone-cut-over-flag-file: %v", argv)
			}
		}
		if missing := ghOstFlagsExistInBinary(t, bin, argv); len(missing) > 0 {
			t.Fatalf("flags this wrapper emits that the binary rejects:\n  %s\nargv: %v",
				strings.Join(missing, "\n  "), argv)
		}
	}
}

func TestPtOSCEmitsOnlyFlagsTheBinaryKnows(t *testing.T) {
	bin := probeBinary(t, "SAKSAMA_PT_OSC_BIN")

	versions, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v (%s)", bin, err, versions)
	}
	t.Logf("probing pt-osc: %s", strings.TrimSpace(string(versions)))

	// With and without foreign keys, because --alter-foreign-keys-method only
	// appears for tables that are referenced by one.
	variants := []struct {
		fk bool
	}{
		{false},
		{true},
	}
	for _, v := range variants {
		opts := testOptions()
		if v.fk {
			opts.TableModel.ReferencedBy = []model.ForeignKey{{
				Name: "fk_probe", RefTable: "parents", RefSchema: "shop", RefColumns: []string{"id"},
			}}
		}
		e := New(PtOSC(), nil, conn(), opts, nil)
		stmts, err := e.Statements()
		if err != nil {
			t.Fatalf("statements: %v", err)
		}
		argv := stmts[0].Argv
		if missing := ptOscFlagsExistInBinary(t, bin, flagsOf(argv)); len(missing) > 0 {
			t.Fatalf("flags this wrapper emits that pt-osc's --help does not list:\n  %s\nargv: %v",
				strings.Join(missing, "\n  "), argv)
		}
	}
}
