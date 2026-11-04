package enginesetup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"gh-ost":                  GhOst,
		"ghost":                   GhOst,
		"GH-OST":                  GhOst,
		"pt-osc":                  PtOSC,
		"percona":                 PtOSC,
		"pt-online-schema-change": PtOSC,
	}
	for in, want := range cases {
		got, err := Normalize(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}

	for _, in := range []string{"native", "ddl", "mysql", ""} {
		if _, err := Normalize(in); err == nil {
			t.Errorf("%q must not be installable", in)
		}
	}
}

func TestManifestPinsTheVerifiedBuild(t *testing.T) {
	target := Target{OS: "linux", Arch: "amd64"}

	gh, err := ManifestFor(GhOst, target)
	if err != nil {
		t.Fatal(err)
	}
	if gh.Version != "1.1.11" {
		t.Errorf("gh-ost version %q, want the verified 1.1.11 from internal/support", gh.Version)
	}
	if !strings.Contains(gh.URL, "/v1.1.11/") || !strings.Contains(gh.URL, "gh-ost-linux-amd64.tar.gz") {
		t.Errorf("gh-ost URL does not name the verified asset: %s", gh.URL)
	}
	if gh.SumsURL == "" {
		t.Error("gh-ost publishes SHA256SUMS; the manifest must point at it")
	}
	if gh.Archive != GhOstBinary {
		t.Errorf("archive member %q", gh.Archive)
	}

	pt, err := ManifestFor(PtOSC, target)
	if err != nil {
		t.Fatal(err)
	}
	if pt.Version != "3.7.2" {
		t.Errorf("pt-osc version %q, want 3.7.2", pt.Version)
	}
	if !strings.Contains(pt.URL, "/v3.7.2/bin/pt-online-schema-change") {
		t.Errorf("pt-osc URL is not pinned to the tag: %s", pt.URL)
	}
	if pt.Sums == "" || pt.SumsURL != "" {
		t.Error("pt-osc has no upstream checksum, so the digest must be pinned and no URL claimed")
	}
	if pt.Archive != "" {
		t.Error("pt-osc is a single file, not an archive")
	}
}

func TestManifestRefusesAnUnsupportedPlatform(t *testing.T) {
	_, err := ManifestFor(GhOst, Target{OS: "windows", Arch: "amd64"})
	if err == nil {
		t.Fatal("windows/amd64 has no published gh-ost build; installing must fail")
	}
	if !strings.Contains(err.Error(), "linux/amd64") {
		t.Errorf("the error should say what is supported: %v", err)
	}
	win := Target{OS: "windows", Arch: "amd64"}
	if win.Supported() {
		t.Fatal("windows/amd64 reported as supported")
	}
}

// fakeEngine builds a tar.gz holding a shell script that reports a version,
// which is the smallest thing that behaves like gh-ost for install purposes.
func fakeEngine(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte(fmt.Sprintf("#!/bin/sh\necho \"%s (git commit: test)\"\n", version))
	hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestInstallVerifiesTheChecksumAndTheReportedVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake engine is a shell script")
	}
	payload := fakeEngine(t, "gh-ost", "9.9.9")

	received := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gh-ost-linux-amd64.tar.gz":
			w.Write(payload)
			received++
		case "/SHA256SUMS":
			fmt.Fprintf(w, "%s  gh-ost-linux-amd64.tar.gz\n", digestOf(payload))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	m := Manifest{
		Engine: GhOst, Binary: GhOstBinary, Version: "9.9.9",
		URL:     srv.URL + "/gh-ost-linux-amd64.tar.gz",
		SumsURL: srv.URL + "/SHA256SUMS",
		Archive: GhOstBinary,
	}
	got, err := Install(context.Background(), m, InstallOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "9.9.9" || got.Digest != digestOf(payload) {
		t.Errorf("unexpected result: %+v", got)
	}
	if got.Path != filepath.Join(dir, GhOstBinary) {
		t.Errorf("path %q", got.Path)
	}
	st, err := os.Stat(got.Path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&0o111 == 0 {
		t.Error("the installed engine is not executable")
	}
	if received != 1 {
		t.Errorf("downloaded %d times", received)
	}
	for _, leftover := range []string{GhOstBinary + ".download", GhOstBinary + ".extract"} {
		if _, err := os.Stat(filepath.Join(dir, leftover)); err == nil {
			t.Errorf("%s was left behind", leftover)
		}
	}
}

func TestInstallRefusesAWrongChecksum(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake engine is a shell script")
	}
	payload := fakeEngine(t, "gh-ost", "9.9.9")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/SHA256SUMS" {
			fmt.Fprintf(w, "%s  gh-ost-linux-amd64.tar.gz\n", strings.Repeat("0", 64))
			return
		}
		w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	m := Manifest{
		Engine: GhOst, Binary: GhOstBinary, Version: "9.9.9",
		URL:     srv.URL + "/gh-ost-linux-amd64.tar.gz",
		SumsURL: srv.URL + "/SHA256SUMS",
		Archive: GhOstBinary,
	}
	if _, err := Install(context.Background(), m, InstallOptions{Dir: dir}); err == nil {
		t.Fatal("a digest mismatch must fail the install")
	}
	for _, name := range []string{GhOstBinary, GhOstBinary + ".download", GhOstBinary + ".extract"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s exists after a refused install", name)
		}
	}
}

func TestInstallRefusesWithoutAnyChecksum(t *testing.T) {
	dir := t.TempDir()
	m := Manifest{Engine: GhOst, Binary: GhOstBinary, Version: "9.9.9", URL: "http://example.invalid/gh-ost"}
	_, err := Install(context.Background(), m, InstallOptions{Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "no checksum") {
		t.Fatalf("expected a checksum refusal, got %v", err)
	}
}

func TestInstallKeepsAVersionMismatchOutOfTheTargetDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake engine is a shell script")
	}
	payload := fakeEngine(t, "gh-ost", "8.8.8")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/SHA256SUMS" {
			fmt.Fprintf(w, "%s  gh-ost-linux-amd64.tar.gz\n", digestOf(payload))
			return
		}
		w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	m := Manifest{
		Engine: GhOst, Binary: GhOstBinary, Version: "9.9.9",
		URL:     srv.URL + "/gh-ost-linux-amd64.tar.gz",
		SumsURL: srv.URL + "/SHA256SUMS",
		Archive: GhOstBinary,
	}
	_, err := Install(context.Background(), m, InstallOptions{Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "1.8.8") && !strings.Contains(err.Error(), "8.8.8") {
		t.Fatalf("expected a version mismatch, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, GhOstBinary)); err == nil {
		t.Fatal("a binary reporting the wrong version was left installed")
	}
}

func TestInstallDoesNotOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, GhOstBinary)
	if err := os.WriteFile(existing, []byte("hand-installed"), 0o755); err != nil {
		t.Fatal(err)
	}

	m := Manifest{Engine: GhOst, Binary: GhOstBinary, Version: "9.9.9", URL: "http://example.invalid/gh-ost"}
	if _, err := Install(context.Background(), m, InstallOptions{Dir: dir}); err == nil {
		t.Fatal("an existing engine must not be replaced without --force")
	}
	body, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hand-installed" {
		t.Errorf("the existing file changed: %q", body)
	}
}

func TestCheckReportsWhatIsMissing(t *testing.T) {
	binaries := map[string]analyzer.EngineBinary{
		GhOstBinary: {Name: GhOstBinary, Available: false, Error: "not found on PATH"},
	}
	c := Check(GhOst, binaries, analyzer.ServerSettings{}, false)

	if c.Ready {
		t.Fatal("a missing binary cannot be ready")
	}
	if !strings.Contains(c.Verdict, "binary") {
		t.Errorf("verdict must name the failure: %q", c.Verdict)
	}
	var fix string
	for _, it := range c.Items {
		if it.Name == "binary" {
			fix = it.Fix
		}
	}
	if !strings.Contains(fix, "saksama engine install gh-ost") {
		t.Errorf("the fix must be a command that exists: %q", fix)
	}
	if c.NotChecked == 0 {
		t.Error("the server side was not checked and must be counted as such")
	}
}

func TestCheckWithoutAProbeDoesNotPass(t *testing.T) {
	c := Check(GhOst, nil, analyzer.ServerSettings{}, false)
	if c.Ready {
		t.Fatal("nothing was probed, so nothing is ready")
	}
	if !strings.Contains(c.Verdict, "not decided") {
		t.Errorf("verdict %q", c.Verdict)
	}
}

func TestCheckMatchesTheInstalledVersionAgainstSupport(t *testing.T) {
	binaries := map[string]analyzer.EngineBinary{
		GhOstBinary: {Name: GhOstBinary, Available: true, Path: "/usr/local/bin/gh-ost", Version: "1.1.11"},
	}
	c := Check(GhOst, binaries, analyzer.ServerSettings{}, false)
	if !c.Ready {
		t.Fatalf("a verified local binary with no server given should be locally ready: %+v", c)
	}
	if c.NotChecked != 1 {
		t.Errorf("expected exactly the server check to be outstanding, got %d", c.NotChecked)
	}
	if !strings.Contains(c.Verdict, "not run") {
		t.Errorf("verdict %q", c.Verdict)
	}

	old := map[string]analyzer.EngineBinary{
		PtOSCBinary: {Name: PtOSCBinary, Available: true, Path: "/usr/bin/pt-online-schema-change", Version: "2.2.20"},
	}
	c = Check(PtOSC, old, analyzer.ServerSettings{}, true)
	if c.Ready {
		t.Fatal("pt-osc 2.x is unsupported; doctor must not report it as ready")
	}
	var versionState string
	for _, it := range c.Items {
		if it.Name == "version" {
			versionState = it.State
		}
	}
	if versionState != StateMissing {
		t.Errorf("pt-osc 2.2.20 must be reported as a missing version, got %q (%s)", versionState, c.Verdict)
	}
}

func TestTidyPerlErrorDropsTheIncludePath(t *testing.T) {
	raw := "Can't locate DBI.pm in @INC (you may need to install the DBI module) (@INC contains: /a /b /c)\n"
	got := TidyPerlError(raw)
	if strings.Contains(got, "@INC contains") {
		t.Errorf("the include path survived: %q", got)
	}
	if !strings.Contains(got, "Can't locate DBI.pm") {
		t.Errorf("the fact was lost: %q", got)
	}
}

// The compatibility table is not a list of exact strings. An installed 2.2.20 is
// covered by the "2.x" row, and 1.1.5 by the "1.1.2 - 1.1.7" range: matching only
// exact versions would call both of them unknown.
func TestRowCovers(t *testing.T) {
	cases := []struct {
		row, installed string
		want           bool
	}{
		{"3.7.2", "3.7.2", true},
		{"3.7.2", "3.7.3", false},
		{"2.x", "2.2.20", true},
		{"2.x", "3.0.13", false},
		{"1.1.2 - 1.1.7", "1.1.5", true},
		{"1.1.2 - 1.1.7", "1.1.11", false},
		{"1.1.2 - 1.1.7", "1.1.1", false},
		{"older than 1.1.2", "1.1.1", true},
		{"older than 1.1.2", "1.1.2", false},
		{"8.0.46, 8.4.13", "8.0.46", false},
	}
	for _, c := range cases {
		if got := rowCovers(c.row, c.installed); got != c.want {
			t.Errorf("rowCovers(%q, %q) = %v, want %v", c.row, c.installed, got, c.want)
		}
	}
}
