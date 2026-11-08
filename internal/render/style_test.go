package render

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
)

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")

// The contract of colour in this tool: with it off, the bytes are what they always
// were; with it on, removing the escapes gives back exactly those bytes. Anything else
// means colour is carrying information, and a log or a `grep` would lose it.
func TestColourNeverChangesTheText(t *testing.T) {
	p := samplePlan()
	p.Checks = []analyzer.Check{
		{ID: "D6", Severity: analyzer.SeverityWarn, Title: "free disk space could not be measured",
			Detail: "no data directory", Remediation: "pass --free-disk and --disk-total"},
		{ID: "E1", Severity: analyzer.SeverityBlocker, Title: "MariaDB is not a verified target"},
	}

	UseColor(false)
	var plain bytes.Buffer
	if err := PlanCard(&plain, p); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.String(), "\x1b[") {
		t.Fatal("colour is off but escape codes were written")
	}

	UseColor(true)
	defer UseColor(false)
	var colored bytes.Buffer
	if err := PlanCard(&colored, p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(colored.String(), "\x1b[") {
		t.Fatal("colour is on but nothing was coloured")
	}
	if got := ansi.ReplaceAllString(colored.String(), ""); got != plain.String() {
		t.Fatalf("the text changed when colour was added:\n--- plain ---\n%s\n--- stripped ---\n%s", plain.String(), got)
	}
}

func TestPaletteIsIdempotentWhenDisabled(t *testing.T) {
	UseColor(false)
	for _, s := range []string{"", "WARN", "hello"} {
		if got := Severity(s); got != s {
			t.Errorf("Severity(%q) = %q", s, got)
		}
		if got := Bold(s); got != s {
			t.Errorf("Bold(%q) = %q", s, got)
		}
	}
}

func TestShouldColorExplainsItself(t *testing.T) {
	var buf bytes.Buffer // not a terminal
	if ok, why := ShouldColor(&buf, false); ok || why == "" {
		t.Fatalf("a buffer must not get colour, and the reason must be given: ok=%v why=%q", ok, why)
	}

	f, err := os.CreateTemp(t.TempDir(), "tty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	t.Setenv("NO_COLOR", "1")
	if ok, why := ShouldColor(f, false); ok || !strings.Contains(why, "NO_COLOR") {
		t.Fatalf("NO_COLOR must win: ok=%v why=%q", ok, why)
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if ok, why := ShouldColor(f, false); ok || !strings.Contains(why, "TERM") {
		t.Fatalf("TERM=dumb must win: ok=%v why=%q", ok, why)
	}
	t.Setenv("TERM", "xterm-256color")
	if ok, why := ShouldColor(f, true); ok || !strings.Contains(why, "--no-color") {
		t.Fatalf("--no-color must win: ok=%v why=%q", ok, why)
	}
}

// A number that means trouble is coloured; a count of nothing is not. Both must read
// the same once the escapes are gone, because the table alongside them is measured in
// characters.
func TestTallyColoursOnlyTrouble(t *testing.T) {
	UseColor(false)
	plainZero := tally(0, "blocking", Bad)
	if plainZero != "0 blocking" {
		t.Fatalf("plain tally = %q", plainZero)
	}
	UseColor(true)
	defer UseColor(false)

	if got := tally(0, "blocking", Bad); strings.Contains(got, "\x1b") {
		t.Errorf("zero must stay plain: %q", got)
	}
	got := tally(2, "blocking", Bad)
	if !strings.Contains(got, "\x1b") {
		t.Errorf("a non-zero count must be coloured: %q", got)
	}
	if stripped := ansi.ReplaceAllString(got, ""); stripped != "2 blocking" {
		t.Errorf("stripped = %q", stripped)
	}
}

// The environment report keeps its table plain, and the summary tells the operator the
// same thing in or out of a terminal.
func TestEnvSummaryIsColourSafe(t *testing.T) {
	checks := []analyzer.SettingCheck{
		{Setting: "log_bin", Current: "OFF", Required: "ON", Status: analyzer.EnvFail, NeededBy: []analyzer.Engine{analyzer.EngineGhOst}, Note: "gh-ost follows writes through it"},
		{Setting: "server_id", Current: "0", Required: "non-zero", Status: analyzer.EnvWarn, NeededBy: []analyzer.Engine{analyzer.EngineGhOst}},
		{Setting: "gtid_mode", Current: "OFF", Required: "either", Status: analyzer.EnvOK, NeededBy: []analyzer.Engine{analyzer.EngineNative}},
	}
	UseColor(false)
	var plain bytes.Buffer
	if err := Environment(&plain, checks, true); err != nil {
		t.Fatal(err)
	}
	UseColor(true)
	var colored bytes.Buffer
	if err := Environment(&colored, checks, true); err != nil {
		t.Fatal(err)
	}
	if got := ansi.ReplaceAllString(colored.String(), ""); got != plain.String() {
		t.Fatalf("the report text changed with colour on:\n--- plain ---\n%s\n--- stripped ---\n%s", plain.String(), got)
	}
}
