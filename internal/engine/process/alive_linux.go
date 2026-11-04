//go:build linux

package process

import (
	"os"
	"strings"
)

// isZombie reports whether pid exists only as an unreaped entry.
//
// A killed child stays visible until its parent reaps it, and signal 0 still
// succeeds on it. Treating a zombie as "still running" makes cancel look broken to
// the very operator who just used it.
func isZombie(pid int) bool {
	data, err := os.ReadFile("/proc/" + itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The state is the field after the parenthesised command name.
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return false
	}
	return s[i+2] == 'Z'
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
