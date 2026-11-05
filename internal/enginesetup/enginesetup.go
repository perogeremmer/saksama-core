// Package enginesetup detects whether a copy engine can run here, and installs
// the verified build when it cannot.
//
// This is the only package in Saksama that downloads and writes an executable,
// so it is deliberately narrow. Three rules hold it together:
//
//  1. One version per engine: the one internal/support records as verified. An
//     installer that pulled "latest" would produce binaries no test in this
//     repository has run, which makes docs/COMPATIBILITY.md a lie.
//  2. Nothing is written before the bytes match a checksum. gh-ost publishes
//     SHA256SUMS per release. Percona publishes no checksum for the single-file
//     pt-online-schema-change, so its digest is pinned in this package.
//  3. Nothing runs implicitly. No command here is called from plan, advise, or
//     apply: installing is always an explicit, separate decision.
package enginesetup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/support"
)

// The two engines Saksama installs. Native is MySQL itself and needs nothing.
const (
	GhOst = "gh-ost"
	PtOSC = "pt-osc"
)

// Binary names on disk.
const (
	GhOstBinary = "gh-ost"
	PtOSCBinary = "pt-online-schema-change"
)

// Normalize maps the names the CLI accepts onto the installable engines.
func Normalize(name string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "gh-ost", "ghost", "gh":
		return GhOst, nil
	case "pt-osc", "pt", "percona", "pt-online-schema-change":
		return PtOSC, nil
	case "native", "ddl":
		return "", fmt.Errorf("native is MySQL itself: there is nothing to install")
	}
	return "", fmt.Errorf("unknown engine %q: installable engines are gh-ost and pt-osc", name)
}

// BinaryName is the file name each engine is installed as.
func BinaryName(engine string) string {
	if engine == PtOSC {
		return PtOSCBinary
	}
	return GhOstBinary
}

// BinDir is where Saksama puts the engines it installs.
//
// It is a directory this tool owns, so installing never needs root and never
// touches /usr/local/bin. Probe looks here before PATH, so an installed engine
// works without the operator editing their shell profile.
func BinDir() (string, error) {
	if dir := os.Getenv("SAKSAMA_BIN_DIR"); dir != "" {
		return dir, nil
	}
	if data := os.Getenv("XDG_DATA_HOME"); data != "" {
		return filepath.Join(data, "saksama", "bin"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate a home directory for the engine bin dir: %w", err)
	}
	return filepath.Join(home, ".local", "share", "saksama", "bin"), nil
}

// Target is the machine that will run the engine.
//
// The copy engines run locally and reach MySQL over the network, so this is the
// machine running Saksama, not the database server.
type Target struct {
	OS   string
	Arch string
}

func (t Target) String() string { return t.OS + "/" + t.Arch }

// ThisTarget is the machine this binary is running on.
func ThisTarget() Target { return Target{OS: runtime.GOOS, Arch: runtime.GOARCH} }

// ghOstAssets is the published asset name per platform. gh-ost ships one tarball
// per os/arch, so an unsupported platform is a refusal, not a guess.
var ghOstAssets = map[Target]string{
	{OS: "linux", Arch: "amd64"}:  "gh-ost-linux-amd64.tar.gz",
	{OS: "linux", Arch: "arm64"}:  "gh-ost-linux-arm64.tar.gz",
	{OS: "darwin", Arch: "amd64"}: "gh-ost-darwin-amd64.tar.gz",
	{OS: "darwin", Arch: "arm64"}: "gh-ost-darwin-arm64.tar.gz",
}

// Supported reports whether this platform has a published build.
func (t Target) Supported() bool {
	if t.OS == "" || t.Arch == "" {
		return false
	}
	_, ok := ghOstAssets[t]
	return ok
}

// TargetNote explains the platform in the operator's terms.
func (t Target) Note() string {
	if !t.Supported() {
		return fmt.Sprintf("%s has no published gh-ost build; supported: darwin/amd64, darwin/arm64, linux/amd64, linux/arm64", t)
	}
	return ghOstAssets[t] + " is published for " + t.String()
}

// Manifest is the recipe for one engine on one target: what to fetch, what the
// bytes must hash to, and what the installed file must report as its version.
type Manifest struct {
	Engine  string
	Binary  string
	Version string
	URL     string
	// Archive is the member to extract from a tar.gz. Empty means the download
	// itself is the file.
	Archive string
	// SumsURL is a published checksum list. Sums is a digest pinned in this
	// package, used when upstream publishes nothing.
	SumsURL string
	Sums    string
	Target  Target
}

// verifiedVersion is the version internal/support records as verified by a test
// in this repository. It is read from that table rather than repeated here, so
// raising the verified version cannot leave the installer behind.
func verifiedVersion(engine string) (string, error) {
	for _, row := range support.Engines {
		if row.Engine != engine || row.Status != support.Verified {
			continue
		}
		// "1.1.11" or "3.7.2"; a range like "1.1.2 - 1.1.7" is never verified.
		for _, field := range strings.Fields(row.Version) {
			if strings.Count(field, ".") == 2 && !strings.ContainsAny(field, "-,") {
				return field, nil
			}
		}
	}
	return "", fmt.Errorf("no verified %s version is recorded in internal/support; refusing to install one nobody tested", engine)
}

// ptOscDigests pins the single Perl file. Percona publishes no checksum for it,
// so this digest is the check. It was computed from the v<version> tag, and a
// silent change upstream fails the install instead of being trusted.
var ptOscDigests = map[string]string{
	"3.7.2": "d24341d04ca7650e4d11c8b413e89f032d06e700db452b0c2edbfa61c2883896",
}

// ManifestFor resolves the verified build for an engine on a target.
func ManifestFor(engine string, target Target) (Manifest, error) {
	name, err := Normalize(engine)
	if err != nil {
		return Manifest{}, err
	}
	version, err := verifiedVersion(name)
	if err != nil {
		return Manifest{}, err
	}

	m := Manifest{
		Engine:  name,
		Binary:  BinaryName(name),
		Version: version,
		Target:  target,
	}

	switch name {
	case GhOst:
		asset, ok := ghOstAssets[target]
		if !ok {
			return Manifest{}, fmt.Errorf("%s: %s", target.Note(), "install gh-ost by hand and re-run `saksama engine doctor`")
		}
		m.URL = fmt.Sprintf("https://github.com/github/gh-ost/releases/download/v%s/%s", version, asset)
		m.SumsURL = fmt.Sprintf("https://github.com/github/gh-ost/releases/download/v%s/SHA256SUMS", version)
		m.Archive = GhOstBinary
	case PtOSC:
		m.URL = fmt.Sprintf("https://raw.githubusercontent.com/percona/percona-toolkit/v%s/bin/%s", version, PtOSCBinary)
		digest, ok := ptOscDigests[version]
		if !ok {
			return Manifest{}, fmt.Errorf("pt-osc %s is verified but has no pinned digest; refusing to install bytes nobody checked", version)
		}
		m.Sums = digest
	}
	return m, nil
}

// Item is one line of the readiness checklist.
type Item struct {
	Name   string `json:"name"`
	State  string `json:"state"` // ok | missing | unknown
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Checklist answers "can this engine run here, and if not, why not".
type Checklist struct {
	Engine   string `json:"engine"`
	Version  string `json:"verified_version"`
	Evidence string `json:"evidence"` // verified | claimed | unsupported | unrecorded
	Target   string `json:"target"`
	Items    []Item `json:"items"`
	// Ready is true when nothing is missing. Checks that were not run are
	// counted in NotChecked, not silently treated as passes.
	Ready      bool   `json:"ready"`
	NotChecked int    `json:"not_checked"`
	Verdict    string `json:"verdict"`
}

const (
	StateOK      = "ok"
	StateMissing = "missing"
	StateUnknown = "unknown"
)

// Check reports what is missing for one engine.
//
// binaries is the probe result and may be nil when nothing was probed; settings
// is the live server state and may be nil when no host was given. Neither absence
// is treated as a pass: unchecked work is counted and shown.
func Check(engine string, binaries map[string]analyzer.EngineBinary, settings analyzer.ServerSettings, live bool) Checklist {
	name, err := Normalize(engine)
	if err != nil {
		return Checklist{Engine: engine, Ready: false, Verdict: err.Error()}
	}
	target := ThisTarget()
	c := Checklist{Engine: name, Target: target.String()}

	if v, err := verifiedVersion(name); err == nil {
		c.Version = v
		if st, _ := supportLookup(name, v); st != "" {
			c.Evidence = string(st)
		}
	}

	if name == GhOst {
		state := StateOK
		detail := target.Note()
		if !target.Supported() {
			state, detail = StateMissing, target.Note()
		}
		c.Items = append(c.Items, Item{Name: "platform", State: state, Detail: detail})
	} else {
		c.Items = append(c.Items, Item{Name: "platform", State: StateOK,
			Detail: "pt-osc is a Perl script: " + Host().Label()})
		c.Items = append(c.Items, perlItems()...)
	}

	c.Items = append(c.Items, binaryItem(name, binaries)...)

	if !live {
		c.Items = append(c.Items, Item{Name: "server", State: StateUnknown,
			Detail: "not checked: no server given",
			Fix:    "saksama engine doctor " + name + " -h <host> -u <user>"})
	} else {
		for _, r := range analyzer.EngineEnvironment(settings, analyzerEngine(name)) {
			item := Item{Name: r.Name, State: StateOK, Detail: r.Detail}
			if !r.Met {
				item.State = StateMissing
				item.Fix = "fix the setting, or pick an engine this server can run: see `saksama advise --live -d <db>`"
			}
			c.Items = append(c.Items, item)
		}
	}

	c.Ready, c.NotChecked, c.Verdict = summarise(c.Items)
	return c
}

// binaryItem reports the installed binary, and whether its version is one this
// repository has evidence for. A build nobody verified is reported as such
// rather than waved through.
func binaryItem(engine string, binaries map[string]analyzer.EngineBinary) []Item {
	name := BinaryName(engine)

	if binaries == nil {
		return []Item{{Name: "binary", State: StateUnknown,
			Detail: "not checked: no probe was run",
			Fix:    "run without --no-probe"}}
	}

	bin, ok := binaries[name]
	if !ok {
		return []Item{{Name: "binary", State: StateUnknown, Detail: "not probed"}}
	}
	if !bin.Available {
		detail := "not found on PATH or in " + binDirForDisplay()
		if bin.Error != "" {
			detail = bin.Error
		}
		fix := "saksama engine install " + engine
		if strings.Contains(detail, "cannot start") {
			// The file is in place; installing it again would not help. The perl
			// line above names the package that does.
			fix = ""
		}
		return []Item{{Name: "binary", State: StateMissing, Detail: detail, Fix: fix}}
	}

	items := []Item{{Name: "binary", State: StateOK,
		Detail: fmt.Sprintf("%s (%s)", bin.Path, orUnknown(bin.Version))}}

	if bin.Version == "" || bin.Version == "unknown" {
		items = append(items, Item{Name: "version", State: StateUnknown,
			Detail: "the binary did not report a version; cannot match it against the verified table"})
		return items
	}

	status, detail := supportLookup(engine, bin.Version)
	switch status {
	case support.Verified:
		items = append(items, Item{Name: "version", State: StateOK,
			Detail: bin.Version + " is a version this repository has run"})
	case support.Claimed:
		items = append(items, Item{Name: "version", State: StateUnknown,
			Detail: bin.Version + " is claimed, not exercised end to end: " + detail})
	case support.Unsupported:
		items = append(items, Item{Name: "version", State: StateMissing,
			Detail: bin.Version + " is not supported: " + detail,
			Fix:    "saksama engine install " + engine + " --force"})
	default:
		items = append(items, Item{Name: "version", State: StateUnknown,
			Detail: bin.Version + " is not in the compatibility table"})
	}
	return items
}

// supportLookup finds the row that covers an installed version.
func supportLookup(engine, version string) (support.Status, string) {
	for _, row := range support.Engines {
		if row.Engine != engine {
			continue
		}
		if rowCovers(row.Version, version) {
			return row.Status, row.Detail
		}
	}
	return "", ""
}

// rowCovers reports whether a compatibility row is talking about an installed
// version. The table uses four shapes, and matching only the exact one would
// report a supported version as unknown:
//
//	"3.7.2"                one exact version
//	"2.x"                  a major line (pt-osc 2.x is unsupported)
//	"1.1.2 - 1.1.7"        a closed range
//	"older than 1.1.2"     an open lower bound
func rowCovers(row, installed string) bool {
	row = strings.TrimSpace(row)
	installed = strings.TrimSpace(installed)

	switch {
	case strings.Contains(row, " - "):
		parts := strings.SplitN(row, " - ", 2)
		return cmpVersion(installed, parts[0]) >= 0 && cmpVersion(installed, parts[1]) <= 0
	case strings.HasPrefix(row, "older than "):
		return cmpVersion(installed, strings.TrimPrefix(row, "older than ")) < 0
	case strings.HasSuffix(row, ".x"):
		return strings.HasPrefix(installed, strings.TrimSuffix(row, "x"))
	}
	return row == installed
}

// cmpVersion compares dotted versions field by field. A field that is not a
// number sorts before one that is, and a missing field counts as zero, so
// "8.0" and "8.0.0" compare equal.
func cmpVersion(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x = atoiOr(as[i], -1)
		}
		if i < len(bs) {
			y = atoiOr(bs[i], -1)
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}

func atoiOr(s string, fallback int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}

var versionRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// PerlDepsReason reports whether the interpreter and the modules pt-osc loads at
// startup are present, and what is missing when they are not.
//
// Installing the Perl file alone is the classic "installed, still will not
// start", so this answer is shared: probe uses it to decide whether pt-osc is
// usable at all, and doctor uses it to name the package to install.
func PerlDepsReason() (string, bool) {
	perl, err := exec.LookPath("perl")
	if err != nil {
		return "perl is not on PATH", false
	}
	if raw, err := exec.Command(perl, "-MDBI", "-MDBD::mysql", "-e", "1").CombinedOutput(); err != nil {
		reason := "DBI and DBD::mysql are required"
		if trimmed := TidyPerlError(string(raw)); trimmed != "" {
			reason += ": " + trimmed
		}
		return reason, false
	}
	return "", true
}

// perlItems checks the interpreter and the modules the pt-osc script loads at
// startup.
func perlItems() []Item {
	perl, err := exec.LookPath("perl")
	if err != nil {
		return []Item{
			{Name: "perl", State: StateMissing, Detail: "perl is not on PATH",
				Fix: Host().FixPerl()},
			{Name: "DBD::mysql", State: StateUnknown, Detail: "not checked: perl is missing"},
		}
	}

	version := "version unknown"
	if raw, err := exec.Command(perl, "--version").CombinedOutput(); err == nil {
		if v := versionRe.FindString(string(raw)); v != "" {
			version = v
		}
	}
	items := []Item{{Name: "perl", State: StateOK, Detail: perl + " (" + version + ")"}}

	if reason, ok := PerlDepsReason(); !ok {
		detail := reason
		if !strings.HasPrefix(detail, "DBI and DBD::mysql") {
			detail = "pt-osc loads DBI and DBD::mysql at startup: " + detail
		}
		return append(items, Item{Name: "DBD::mysql", State: StateMissing, Detail: detail,
			Fix: Host().FixDBD()})
	}
	return append(items, Item{Name: "DBD::mysql", State: StateOK, Detail: "DBI and DBD::mysql load"})
}

// summarise turns the checklist into a verdict.
//
// Ready means nothing is missing *and* the binary was actually looked at: a run
// with no probe has not earned a pass, whatever else it found. NotChecked keeps
// the work that was skipped visible instead of letting it disappear.
func summarise(items []Item) (ready bool, notChecked int, verdict string) {
	var missing []string
	binaryChecked := false
	for _, it := range items {
		switch it.State {
		case StateMissing:
			missing = append(missing, it.Name)
		case StateUnknown:
			notChecked++
		}
		if it.Name == "binary" {
			binaryChecked = it.State == StateOK
		}
	}

	switch {
	case len(missing) > 0:
		return false, notChecked, "cannot run: " + strings.Join(missing, ", ")
	case !binaryChecked:
		return false, notChecked, "not decided: the engine binary was not checked"
	case notChecked > 0:
		return true, notChecked, fmt.Sprintf("local side ready; %d check(s) not run", notChecked)
	}
	return true, 0, "ready"
}

func analyzerEngine(engine string) analyzer.Engine {
	if engine == GhOst {
		return analyzer.EngineGhOst
	}
	return analyzer.EnginePtOSC
}

func binDirForDisplay() string {
	if dir, err := BinDir(); err == nil {
		return dir
	}
	return "the engine bin dir"
}

func orUnknown(s string) string {
	if s == "" {
		return "version unknown"
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// TidyPerlError keeps the useful half of a Perl failure. The module loader dumps
// its whole @INC search path, which is fifty times longer than the fact being
// reported and buries it.
func TidyPerlError(raw string) string {
	line := strings.TrimSpace(firstLine(strings.TrimSpace(raw)))
	if i := strings.Index(line, "(@INC"); i > 0 {
		line = strings.TrimSpace(line[:i])
	}
	if len(line) > 120 {
		line = line[:117] + "..."
	}
	return line
}

// ProbeTimeout bounds the version checks this package runs. A hung binary must
// not hang the CLI.
const ProbeTimeout = 5 * time.Second
