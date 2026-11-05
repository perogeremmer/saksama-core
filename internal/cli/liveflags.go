package cli

import (
	"fmt"
	"io"
	"strings"
)

// Connection flags exist only in live mode. `saksama advise -h db ...` looks like a
// live command and is one, except the flag set it lands on is the offline one, so the
// answer was a bare flag list with no hint that --live was missing.
var connectionFlags = map[string]bool{
	"-h": true, "-host": true, "--host": true,
	"-P": true, "-port": true, "--port": true,
	"-u": true, "-user": true, "--user": true,
	"--tls":                 true,
	"--defaults-extra-file": true,
	"--ask-pass":            true,
	"--no-defaults":         true,
}

// firstConnectionFlag returns the connection flag in args, if any.
func firstConnectionFlag(args []string) string {
	for _, a := range args {
		name := a
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if connectionFlags[name] {
			return name
		}
	}
	return ""
}

// explainMissingLive turns "flag provided but not defined" into the sentence the
// person needed: this is a live flag, and live mode is opt-in.
func explainMissingLive(cmd string, args []string, stderr io.Writer) bool {
	flag := firstConnectionFlag(args)
	if flag == "" {
		return false
	}
	fmt.Fprintf(stderr,
		"saksama %s: %s connects to a server, and %s only does that with --live.\n"+
			"  live:     saksama %s --live %s\n"+
			"  offline:  saksama %s --ddl <file|-> --mysql-version <X.Y.Z> [-t <table>] [-a \"<ALTER>\"]\n",
		cmd, flag, cmd, cmd, strings.Join(summarise(args), " "), cmd)
	return true
}

// summarise trims what the operator typed to something worth repeating back.
func summarise(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		// A password must never be echoed, even as a flag name with its value.
		if strings.Contains(strings.ToLower(a), "password") || strings.Contains(strings.ToLower(a), "ask-pass") {
			continue
		}
		out = append(out, a)
	}
	return out
}
