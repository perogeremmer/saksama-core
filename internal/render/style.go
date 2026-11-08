package render

import (
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Colour is presentation only: no decision, no claim, nothing that changes what the
// tool says about a schema. It exists because a wall of same-coloured text is harder
// to scan than the same text with the two lines that matter marked.
//
// The rules it must never break:
//
//   - piping must produce exactly what it produced before. CI logs, diffs, grep and
//     the test suite all read this output, and an escape sequence in a log file is
//     noise that survives copy-paste into a ticket.
//   - NO_COLOR (the de-facto standard) and TERM=dumb turn it off, as does --no-color.
//   - nothing may depend on colour to be understood: "BLOCKER" is still a word when
//     the colour is gone.
type Palette struct {
	Enabled bool
}

// colour is the process-wide setting, chosen once by a frontend that knows whether
// stdout is a terminal. Package state, not an argument, because the alternative is
// threading a presentation flag through every renderer for a decision that is already
// global to the process.
var colour = Palette{}

// UseColor turns colour on or off for this process.
func UseColor(enabled bool) { colour.Enabled = enabled }

// ColorEnabled reports the current setting.
func ColorEnabled() bool { return colour.Enabled }

const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiRed     = "\x1b[31m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiCyan    = "\x1b[36m"
	ansiMagenta = "\x1b[35m"
)

func paint(code, s string) string {
	if !colour.Enabled || s == "" {
		return s
	}
	return code + s + ansiReset
}

// Bold marks a heading.
func Bold(s string) string { return paint(ansiBold, s) }

// Dim marks secondary text: ids, units, hints that should not compete.
func Dim(s string) string { return paint(ansiDim, s) }

// Good marks a success: a verification that passed, a plan that is ready.
func Good(s string) string { return paint(ansiGreen, s) }

// Bad marks a refusal or a failure.
func Bad(s string) string { return paint(ansiRed, s) }

// Warn marks something to look at.
func Warn(s string) string { return paint(ansiYellow, s) }

// Accent marks something to follow: a value the reader asked for.
func Accent(s string) string { return paint(ansiCyan, s) }

// Severity colours a check's severity word.
func Severity(sev string) string {
	switch strings.ToUpper(sev) {
	case "BLOCKER":
		return paint(ansiRed+ansiBold, sev)
	case "WARN", "WARNING":
		return paint(ansiYellow, sev)
	case "INFO":
		return paint(ansiCyan, sev)
	case "OK", "DONE":
		return paint(ansiGreen, sev)
	}
	return sev
}

// ShouldColor decides whether a writer gets colour, and why not when it does not. The
// reason is returned so `--report` or a debug flag can explain a dull terminal instead
// of leaving the operator to guess.
func ShouldColor(w io.Writer, noColorFlag bool) (bool, string) {
	if noColorFlag {
		return false, "--no-color was given"
	}
	if os.Getenv("NO_COLOR") != "" {
		return false, "NO_COLOR is set"
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false, "TERM=dumb"
	}
	if !isTerminal(w) {
		return false, "the output is not a terminal"
	}
	return true, ""
}

// isTerminal reports whether w is a terminal. The wizard already depends on
// golang.org/x/term for password input, so this asks the same question the same way
// instead of guessing from file modes.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// StyleLine colours one line for the doctor and menu output: the leading mark carries
// the meaning, the rest stays readable.
func StyleLine(mark, line string) string {
	switch mark {
	case "ok", "yes":
		return paint(ansiGreen, mark) + " " + line
	case "FAIL", "no":
		return paint(ansiRed, mark) + " " + line
	case "??", "WARN":
		return paint(ansiYellow, mark) + " " + line
	}
	return line
}
