package cli

import "strings"

// extractPositionals separates positional arguments from flags, keeping flags and
// their values in order.
//
// The standard library flag package stops parsing at the first positional
// argument, so `saksama engine set gh-ost -d shop -t orders` would silently
// ignore -d and -t. This helper makes the natural typing order work: the
// positional argument is removed, everything else is handed to flag.Parse.
//
// valueFlags lists the flag names (without leading dashes) that consume the next
// argument. Boolean flags must not be listed.
func extractPositionals(args []string, valueFlags map[string]bool) (positional, flags []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]

		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}

		flags = append(flags, arg)

		// --flag=value carries its value inline.
		if strings.Contains(arg, "=") {
			continue
		}
		if valueFlags[strings.TrimLeft(arg, "-")] && i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return positional, flags
}
