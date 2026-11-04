package introspect

import (
	"os"
	"syscall"
	"unicode/utf8"
)

// FreeDisk reports the free bytes on the filesystem holding path.
//
// It returns 0 when the path does not exist locally, which is the normal case on
// a managed service where the client runs on a different machine. A 0 result
// means "not measured", never "no space".
func FreeDisk(path string) int64 {
	// MySQL reports @@datadir with a trailing slash; walk up until it exists.
	for p := path; p != "" && p != "/"; p = parentDir(p) {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(p, &st); err != nil {
			return 0
		}
		free := int64(st.Bavail) * int64(st.Bsize)
		if free < 0 {
			return 0
		}
		return free
	}
	return 0
}

func parentDir(p string) string {
	for i := len(p) - 1; i > 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return ""
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
