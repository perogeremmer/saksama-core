package enginesetup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// maxDownload bounds a fetch so a wrong URL or a hostile redirect cannot fill the
// disk. gh-ost's tarball is under 10 MB and the pt-osc script under 0.5 MB.
const maxDownload = 200 << 20

// InstallOptions is everything Install needs. The zero value installs the
// verified build into BinDir().
type InstallOptions struct {
	// Dir overrides the target directory. Empty means BinDir().
	Dir string
	// Force replaces a file that is already there.
	Force bool
	// Client overrides the HTTP client, for tests and for proxies.
	Client *http.Client
}

// InstallResult is what happened, in the terms a reader wants: where it landed,
// what it hashed to, and what version the installed file reported back.
type InstallResult struct {
	Path    string
	Version string
	Digest  string
	URL     string
	// ChecksumSource says what the digest was compared against, because "verified"
	// means different things for a published list and a pinned digest.
	ChecksumSource string
}

// Install fetches the manifest, verifies it, and puts exactly one executable in
// place.
//
// The order is deliberate: download to a temporary name, hash it, compare, then
// rename. A failure leaves no half-written binary in the target directory, and a
// version that does not match the manifest is removed rather than kept.
func Install(ctx context.Context, m Manifest, opts InstallOptions) (InstallResult, error) {
	// Refuse before the first byte: downloading something that nothing can verify
	// is the one order of operations this package must not get wrong.
	if m.Sums == "" && m.SumsURL == "" {
		return InstallResult{}, fmt.Errorf("no checksum is available for %s; refusing to install bytes nothing can verify", m.URL)
	}

	dir := opts.Dir
	if dir == "" {
		var err error
		if dir, err = BinDir(); err != nil {
			return InstallResult{}, err
		}
	}
	// 0700, not 0755: a world-writable install directory would let another local
	// user swap the payload between the hash check and the rename, and everything
	// this directory holds will be executed.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return InstallResult{}, fmt.Errorf("cannot create %s: %w", dir, err)
	}

	target := filepath.Join(dir, m.Binary)
	if _, err := os.Stat(target); err == nil && !opts.Force {
		return InstallResult{}, fmt.Errorf("%s already exists; re-run with --force to replace it", target)
	}

	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}

	download := target + ".download"
	defer os.Remove(download)
	digest, err := fetch(ctx, client, m.URL, download)
	if err != nil {
		return InstallResult{}, err
	}

	want, source, err := expectedSum(ctx, client, m)
	if err != nil {
		return InstallResult{}, err
	}
	if digest != want {
		return InstallResult{}, fmt.Errorf("%s hashes to %s, but %s says %s; nothing was installed", m.URL, digest, source, want)
	}

	// The digest above was taken over the wire, from the stream as it was read.
	// Re-reading the file from disk right before anything is extracted from it
	// closes the window in between: what gets extracted is what got verified.
	onDisk, err := fileDigest(download)
	if err != nil {
		return InstallResult{}, err
	}
	if onDisk != digest {
		return InstallResult{}, fmt.Errorf("the downloaded bytes changed between verification and use; nothing was installed")
	}

	payload := download
	if m.Archive != "" {
		extracted := target + ".extract"
		defer os.Remove(extracted)
		if err := extractMember(download, m.Archive, extracted); err != nil {
			return InstallResult{}, err
		}
		payload = extracted
	}

	if err := os.Chmod(payload, 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("cannot make %s executable: %w", payload, err)
	}
	if err := os.Rename(payload, target); err != nil {
		return InstallResult{}, fmt.Errorf("cannot move the engine into place: %w", err)
	}

	// A file that will not run, or that reports a different version, is not an
	// installed engine. Remove it and say why.
	got, err := reportedVersion(ctx, target, m)
	if err != nil {
		os.Remove(target)
		return InstallResult{}, fmt.Errorf("%s was removed: %w", target, err)
	}
	if got != m.Version {
		os.Remove(target)
		return InstallResult{}, fmt.Errorf("%s reports version %s, but this build installs %s; the file was removed",
			target, got, m.Version)
	}

	return InstallResult{Path: target, Version: got, Digest: digest, URL: m.URL, ChecksumSource: source}, nil
}

// fetch downloads url to path and returns the sha256 of the bytes written.
func fetch(ctx context.Context, client *http.Client, url, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("cannot build the request for %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cannot download %s: HTTP %s", url, resp.Status)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("cannot write %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return "", fmt.Errorf("download of %s stopped: %w", url, err)
	}
	if n > maxDownload {
		return "", fmt.Errorf("%s is larger than the %d byte limit; refusing it", url, maxDownload)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// expectedSum returns the digest the download must match, and what it came from.
func expectedSum(ctx context.Context, client *http.Client, m Manifest) (string, string, error) {
	if m.Sums != "" {
		return m.Sums, "the digest pinned in this build", nil
	}
	if m.SumsURL == "" {
		return "", "", fmt.Errorf("no checksum is available for %s; refusing to install bytes nothing can verify", m.URL)
	}

	list, err := os.CreateTemp("", "saksama-sums-*")
	if err != nil {
		return "", "", fmt.Errorf("cannot open a temporary file for the checksum list: %w", err)
	}
	listPath := list.Name()
	list.Close()
	defer os.Remove(listPath)
	if _, err := fetch(ctx, client, m.SumsURL, listPath); err != nil {
		return "", "", err
	}
	raw, err := os.ReadFile(listPath)
	if err != nil {
		return "", "", fmt.Errorf("cannot read the checksum list: %w", err)
	}

	asset := filepath.Base(m.URL)
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == asset {
			return fields[0], m.SumsURL, nil
		}
	}
	return "", "", fmt.Errorf("%s does not list %s; refusing to install", m.SumsURL, asset)
}

// extractMember writes one regular file out of a tar.gz.
//
// Only the named member is accepted, by base name, with no path traversal: this
// reads an archive it just downloaded, and an archive is not a filesystem.
func extractMember(archive, member, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("cannot open %s: %w", archive, err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s is not a gzip archive: %w", archive, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", archive, err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != member {
			continue
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return fmt.Errorf("cannot write %s: %w", dest, err)
		}
		defer out.Close()
		if _, err := io.Copy(out, io.LimitReader(tr, maxDownload)); err != nil {
			return fmt.Errorf("cannot extract %s: %w", member, err)
		}
		return nil
	}
	return fmt.Errorf("%s does not contain %s; refusing a partial install", archive, member)
}

var installVersionRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// reportedVersion runs the installed file and reads the version out of its own
// output. An engine that cannot start is not installed, whatever the file says.
func reportedVersion(ctx context.Context, path string, m Manifest) (string, error) {
	cmd := exec.CommandContext(ctx, path, "--version")
	if m.Engine == PtOSC {
		perl, err := exec.LookPath("perl")
		if err != nil {
			return "", fmt.Errorf("perl is not on PATH, so %s cannot run; install perl first", path)
		}
		cmd = exec.CommandContext(ctx, perl, path, "--version")
	}

	raw, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s --version failed: %v: %s", path, err, firstLine(strings.TrimSpace(string(raw))))
	}
	v := installVersionRe.FindString(string(raw))
	if v == "" {
		return "", fmt.Errorf("%s --version printed no version: %s", path, firstLine(strings.TrimSpace(string(raw))))
	}
	return v, nil
}

// fileDigest returns the sha256 of whatever is on disk at path.
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("cannot reopen %s for verification: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("cannot hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
