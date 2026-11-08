package wizard

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Item is one choice in a numbered menu.
type Item struct {
	Key   string
	Label string
	Note  string
}

// Prompter is the only way the wizard talks to a person.
//
// It is an interface for the same reason cred.PromptOverride is one: a scripted
// prompt makes the whole flow testable without a pty, and a pty test is the kind
// of test that gets deleted the first time it flakes in CI.
type Prompter interface {
	Printf(format string, a ...any)
	Ask(label, def string) (string, error)
	Secret(label string) (string, error)
	Choose(label string, items []Item, def string) (string, error)
	Confirm(label string, def bool) (bool, error)
}

// Terminal is a line-based prompter.
//
// Line-based, not a full-screen TUI, on purpose: it works through ssh, inside
// tmux, with TERM=dumb, over a slow link, and while the output is being copied
// into a bug report. Arrow keys buy nothing here.
type Terminal struct {
	In  io.Reader
	Out io.Writer

	buf *bufio.Reader
}

// NewTerminal builds a prompter over the given streams.
func NewTerminal(in io.Reader, out io.Writer) *Terminal {
	return &Terminal{In: in, Out: out, buf: bufio.NewReader(in)}
}

// Printf writes an ordinary line of output.
func (t *Terminal) Printf(format string, a ...any) {
	fmt.Fprintf(t.Out, format, a...)
}

func (t *Terminal) readLine() (string, error) {
	line, err := t.buf.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

// Ask reads a line, falling back to def on an empty answer.
func (t *Terminal) Ask(label, def string) (string, error) {
	if def != "" {
		t.Printf("%s [%s]: ", label, def)
	} else {
		t.Printf("%s: ", label)
	}
	line, err := t.readLine()
	if err != nil {
		return def, err
	}
	if strings.TrimSpace(line) == "" {
		return def, nil
	}
	return strings.TrimSpace(line), nil
}

// Secret reads a value without echoing it when stdin is a terminal, and as a
// plain line otherwise (which is how a scripted run supplies one).
func (t *Terminal) Secret(label string) (string, error) {
	if f, ok := t.In.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		t.Printf("%s: ", label)
		b, err := term.ReadPassword(int(f.Fd()))
		t.Printf("\n")
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return t.Ask(label, "")
}

// Choose shows a numbered menu and re-asks until the answer is one of the items.
func (t *Terminal) Choose(label string, items []Item, def string) (string, error) {
	t.Printf("\n%s\n", label)

	// Columns come from the items, not from a fixed width: a fixed one leaves a
	// ragged gutter as soon as a label is longer, and pads short menus with space
	// nobody asked for. Widths are measured in runes - table names can be
	// multi-byte, and fmt's %-Ns pads by bytes. Notes are aligned only when some
	// item has one.
	keyW, labelW := 0, 0
	notes := false
	for _, it := range items {
		if r := len([]rune(it.Key)); r > keyW {
			keyW = r
		}
		if r := len([]rune(it.Label)); r > labelW {
			labelW = r
		}
		if it.Note != "" {
			notes = true
		}
	}
	for _, it := range items {
		line := "  " + runePad(it.Key, keyW) + "  " + runePad(it.Label, labelW)
		if notes && it.Note != "" {
			line += "  " + it.Note
		}
		t.Printf("%s\n", strings.TrimRight(line, " "))
	}
	prompt := "choose"
	if def != "" {
		prompt += " [" + def + "]"
	}
	for {
		t.Printf("%s: ", prompt)
		line, err := t.readLine()
		if err != nil {
			return "", err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			line = def
		}
		for _, it := range items {
			if strings.EqualFold(line, it.Key) {
				return it.Key, nil
			}
		}
		t.Printf("  %q is not one of the choices\n", line)
	}
}

// runePad pads s with spaces to n visible runes, which is what fmt's %-Ns
// would do if it counted characters instead of bytes.
func runePad(s string, n int) string {
	pad := n - len([]rune(s))
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(" ", pad)
}

// Confirm asks a yes/no question. The default is shown in the prompt, so an
// empty answer is never ambiguous.
func (t *Terminal) Confirm(label string, def bool) (bool, error) {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	for {
		t.Printf("%s %s: ", label, hint)
		line, err := t.readLine()
		if err != nil {
			return def, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return def, nil
		case "y", "yes", "ya":
			return true, nil
		case "n", "no", "tidak":
			return false, nil
		}
	}
}
