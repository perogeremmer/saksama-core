package introspect

import (
	"os"
	"strings"
	"syscall"
	"unicode/utf8"
)

// DiskSpace is what can be observed about the volume holding the data directory.
type DiskSpace struct {
	// Path is the data directory MySQL reported.
	Path string
	// Free and Total are bytes. Zero means "not measured", which is not the same
	// as "no space".
	Free  int64
	Total int64
	// Measured reports whether a filesystem was actually read.
	Measured bool
	// Reason explains an unmeasured result, so the operator knows why.
	Reason string
}

// Used reports the bytes occupied on the volume.
func (d DiskSpace) Used() int64 {
	if !d.Measured || d.Total < d.Free {
		return 0
	}
	return d.Total - d.Free
}

// FreePercent reports the free share of the volume, or 0 when unmeasured.
func (d DiskSpace) FreePercent() float64 {
	if !d.Measured || d.Total <= 0 {
		return 0
	}
	return float64(d.Free) / float64(d.Total) * 100
}

// ReadDiskSpace measures the volume holding path.
//
// MySQL cannot report free filesystem space over SQL, so this only works when
// Saksama runs on the database host. Everywhere else it returns Measured=false
// with a reason, and the analyzer asks the operator for the number instead of
// guessing.
func ReadDiskSpace(path string) DiskSpace {
	if path == "" {
		return DiskSpace{Reason: "the server did not report a data directory"}
	}

	d := DiskSpace{Path: path}

	// Only the exact data directory may be measured. Walking up to a parent would
	// happily report the free space of a different volume, which is worse than
	// reporting nothing: it turns "I cannot see the server" into a confident lie.
	if _, err := os.Stat(path); err != nil {
		d.Reason = "the data directory " + strings.TrimSuffix(path, "/") + " is not on this machine"
		return d
	}

	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		d.Reason = "filesystem statistics are not readable for " + path
		return d
	}
	free := int64(st.Bavail) * int64(st.Bsize)
	total := int64(st.Blocks) * int64(st.Bsize)
	if free < 0 || total <= 0 {
		d.Reason = "filesystem statistics look invalid for " + path
		return d
	}
	d.Free, d.Total, d.Measured = free, total, true
	return d
}

// FreeDisk reports the free bytes on the filesystem holding path, or 0 when it is
// not measurable from here.
func FreeDisk(path string) int64 {
	d := ReadDiskSpace(path)
	if !d.Measured {
		return 0
	}
	return d.Free
}

// SanitizeSQL prepares a statement for storage in an event or an audit log. It
// truncates long text and replaces invalid UTF-8 so the log stays parseable.
func SanitizeSQL(s string, max int) string {
	if !utf8.ValidString(s) {
		s = toValidUTF8(s)
	}
	s = truncate(s, max)
	return s
}

func toValidUTF8(s string) string {
	out := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			out = append(out, '?')
			i++
			continue
		}
		out = append(out, r)
		i += size
	}
	return string(out)
}
