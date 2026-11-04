package introspect

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadDiskSpaceMeasuresAnExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	d := ReadDiskSpace(dir)

	if !d.Measured {
		t.Fatalf("a temp dir should be measurable: %s", d.Reason)
	}
	if d.Free <= 0 || d.Total <= 0 {
		t.Fatalf("invalid numbers: %+v", d)
	}
	if d.Free > d.Total {
		t.Fatalf("free exceeds total: %+v", d)
	}
	if pct := d.FreePercent(); pct <= 0 || pct > 100 {
		t.Fatalf("free percent out of range: %v", pct)
	}
}

// This is the bug worth remembering: walking up to a parent directory reports the
// free space of a different volume, which is worse than reporting nothing.
func TestReadDiskSpaceDoesNotFallBackToAParent(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does", "not", "exist")
	d := ReadDiskSpace(missing)

	if d.Measured {
		t.Fatalf("a missing data directory must not be measured via a parent: %+v", d)
	}
	if d.Reason == "" {
		t.Fatal("an unmeasured disk must explain itself")
	}
}

func TestReadDiskSpaceHandlesEmptyPath(t *testing.T) {
	d := ReadDiskSpace("")
	if d.Measured {
		t.Fatal("an empty path cannot be measured")
	}
	if d.Reason == "" {
		t.Fatal("expected a reason")
	}
}

func TestReadDiskSpaceHandlesTrailingSlash(t *testing.T) {
	dir := t.TempDir()
	d := ReadDiskSpace(dir + string(os.PathSeparator))
	if !d.Measured {
		t.Fatalf("a trailing slash should still work: %s", d.Reason)
	}
}

func TestFreeDiskReturnsZeroWhenUnmeasurable(t *testing.T) {
	if got := FreeDisk(filepath.Join(t.TempDir(), "nope")); got != 0 {
		t.Fatalf("expected 0 for an unmeasurable path, got %d", got)
	}
}

// The hostname check exists because a path can be misleading: a CI runner with
// MySQL preinstalled also has /var/lib/mysql, so a container's data directory can
// look local while the free space belongs to a different filesystem.
func TestDiskSpaceReasonNamesTheHostnameMismatch(t *testing.T) {
	dir := t.TempDir()
	// A local directory is measurable, which is the case the guard must not block.
	d := ReadDiskSpace(dir)
	if !d.Measured {
		t.Fatalf("a local directory should be measurable: %s", d.Reason)
	}
	// The guard itself lives in Collector.diskSpace, which needs a server; the
	// integration suite covers it there.
	d = ReadDiskSpace(filepath.Join(dir, "absent"))
	if d.Measured || d.Reason == "" {
		t.Fatalf("expected an explained refusal, got %+v", d)
	}
}
