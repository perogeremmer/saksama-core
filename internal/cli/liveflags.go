package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// Connection flags exist only in live mode. `saksama advise -h db ...` looks like a
// live command and is one, except the flag set it lands on is the offline one, so the
// answer was a bare flag list with no hint that --live was missing.
var connectionFlags = map[string]bool{
	"h": true, "host": true,
	"P": true, "port": true,
	"u": true, "user": true,
	"tls":                 true,
	"defaults-extra-file": true,
	"ask-pass":            true,
	"no-defaults":         true,
}

// missingLiveFlag returns the first connection flag that the offline flag set does not
// define, which is the only case worth explaining.
//
// It asks the flag set rather than guessing from a list, because the same name can
// mean different things in the two modes: `--host` is a live connection on `advise`
// and a reporting label on offline `plan`, where it parses fine and must keep working.
func missingLiveFlag(fs *flag.FlagSet, args []string) string {
	for _, a := range args {
		name := strings.TrimLeft(a, "-")
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if name == "" || !connectionFlags[name] {
			continue
		}
		if fs.Lookup(name) == nil {
			return name
		}
	}
	return ""
}

// explainMissingLive turns a flag the offline set cannot accept into the sentence the
// person needed: this flag connects to a server, and live mode is opt-in.
func explainMissingLive(cmd, flagName string, args []string, stderr io.Writer) bool {
	if flagName == "" {
		return false
	}
	fmt.Fprintf(stderr,
		"saksama %s: -%s connects to a server, and %s only does that with --live.\n"+
			"  live:     saksama %s --live %s\n"+
			"  offline:  saksama %s --ddl <file|-> --mysql-version <X.Y.Z> [-t <table>] [-a \"<ALTER>\"]\n",
		cmd, flagName, cmd, cmd, strings.Join(summarise(args), " "), cmd)
	return true
}

// summarise trims what the operator typed to something worth repeating back.
func summarise(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		// A password must never be echoed, even as a flag name with its value.
		lower := strings.ToLower(a)
		if strings.Contains(lower, "password") || strings.Contains(lower, "ask-pass") {
			continue
		}
		out = append(out, a)
	}
	return out
}
