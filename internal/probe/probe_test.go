package probe

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestParseVersion(t *testing.T) {
	cases := map[string]string{
		"gh-ost 1.1.6\n":                  "1.1.6",
		"1.1.7":                           "1.1.7",
		"pt-online-schema-change 3.5.1\n": "3.5.1",
		"pt-online-schema-change version 2.2.20\n":           "2.2.20",
		"Percona Toolkit 3.0.4 (perl 5.30)":                  "3.0.4",
		"This is perl 5, version 36, subversion 0 (v5.36.0)": "5.36.0",
		"no version here":                                    "",
		"":                                                   "",
		"v2.1":                                               "2.1",
	}
	for in, want := range cases {
		if got := parseVersion(in); got != want {
			t.Errorf("parseVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBinariesReportsMissingWithoutError(t *testing.T) {
	// The environment may or may not have these installed; either way the probe
	// must return an entry per known binary and must never panic.
	got := Binaries()
	for _, name := range names {
		bin, ok := got[name]
		if !ok {
			t.Fatalf("no entry for %s", name)
		}
		if bin.Name != name {
			t.Errorf("%s: Name = %q", name, bin.Name)
		}
		if !bin.Available && bin.Error == "" {
			t.Errorf("%s: unavailable but no reason given", name)
		}
	}
}

// A binary Saksama installed itself must be found without the operator touching
// PATH, and it must win over a different copy that happens to be on PATH: the
// managed one is the version this build verified.
func TestManagedBinaryWinsOverPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake engine is a shell script")
	}
	managed := t.TempDir()
	onPath := t.TempDir()
	for _, dir := range []string{managed, onPath} {
		script := "#!/bin/sh\necho \"gh-ost 9.9.9\"\n"
		if err := os.WriteFile(filepath.Join(dir, "gh-ost"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("SAKSAMA_BIN_DIR", managed)
	t.Setenv("PATH", onPath+string(os.PathListSeparator)+os.Getenv("PATH"))

	got := probeOne("gh-ost")
	if !got.Available {
		t.Fatalf("the managed binary was not found: %+v", got)
	}
	if got.Path != filepath.Join(managed, "gh-ost") {
		t.Errorf("probe used %q, want the managed copy in %q", got.Path, managed)
	}
	if got.Version != "9.9.9" {
		t.Errorf("version %q", got.Version)
	}
}
