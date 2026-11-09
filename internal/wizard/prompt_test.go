package wizard

import (
	"bytes"
	"strings"
	"testing"
)

// Choose re-asks on anything that is not a choice, and the invalid answer is
// named back - "12" must not silently become the default.
func TestChooseReasksUntilValid(t *testing.T) {
	var out bytes.Buffer
	tm := NewTerminal(strings.NewReader("nope\n\n2\n"), &out)

	got, err := tm.Choose("pick", []Item{
		{"1", "first", ""},
		{"2", "second", ""},
	}, "2")
	if err != nil {
		t.Fatal(err)
	}
	if got != "2" {
		t.Fatalf("choose = %q, want 2", got)
	}
	if !strings.Contains(out.String(), `"nope" is not one of the choices`) {
		t.Fatalf("the invalid answer must be named back:\n%s", out.String())
	}
	// The empty line took the default; only one invalid line was rejected.
	if !strings.Contains(out.String(), "choose [2]:") {
		t.Fatalf("the default must be shown in the prompt:\n%s", out.String())
	}
}

// Columns are measured in runes: a multi-byte label must not leave the note
// column ragged.
func TestChooseMeasuresRunes(t *testing.T) {
	var out bytes.Buffer
	tm := NewTerminal(strings.NewReader("1\n"), &out)

	_, err := tm.Choose("which", []Item{
		{"1", "tabelé", "utf8 note"},
		{"2", "plain", "other"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if len(lines) < 3 {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
	// Both labels start at the same column, whatever their byte length.
	var col [2]int
	rows := 0
	for _, line := range lines {
		if !strings.HasPrefix(line, "  ") || strings.Contains(line, "which") {
			continue
		}
		if rows < 2 {
			col[rows] = strings.Index(line, strings.Fields(strings.TrimSpace(line))[1])
			rows++
		}
	}
	if rows != 2 || col[0] != col[1] {
		t.Fatalf("the note columns do not line up: %v\n%s", col, out.String())
	}
}

// EOF with a partial line still returns that line: a heredoc without a trailing
// newline must not silently lose the last answer.
func TestReadLineKeepsAPartialFinalLine(t *testing.T) {
	var out bytes.Buffer
	tm := NewTerminal(strings.NewReader("final answer"), &out)

	v, err := tm.Ask("name", "")
	if err != nil {
		t.Fatalf("a partial final line is not an error: %v", err)
	}
	if v != "final answer" {
		t.Fatalf("ask = %q", v)
	}
	// The next read hits real EOF and reports it.
	if _, err := tm.Ask("next", ""); err == nil {
		t.Fatal("the read after the partial line must see EOF")
	}
}

// Ask falls back to its default on an empty answer, and trims whitespace around
// a typed one.
func TestAskDefaultAndTrim(t *testing.T) {
	var out bytes.Buffer
	tm := NewTerminal(strings.NewReader("  padded  \n\n"), &out)

	v, err := tm.Ask("first", "")
	if err != nil || v != "padded" {
		t.Fatalf("ask = %q err = %v", v, err)
	}
	v, err = tm.Ask("second", "fallback")
	if err != nil || v != "fallback" {
		t.Fatalf("empty answer must take the default: %q err = %v", v, err)
	}
}

// Confirm is the gate before a mutation, so every spelling it accepts and every
// spelling it refuses is part of the contract. It re-asks on anything it cannot
// read rather than guessing, because a guessed "yes" here runs an ALTER.
func TestConfirmAcceptsAndReAsks(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"\n", true},            // default true
		{"y\n", true},           //
		{"YES\n", true},         //
		{"ya\n", true},          // the repository's own language
		{"n\n", false},          //
		{"No\n", false},         //
		{"tidak\n", false},      //
		{"hmm\n" + "y\n", true}, // an unreadable answer is re-asked, not guessed
		{"\n", false},           // default false
		{"n\n", false},          //
	}
	for _, c := range cases {
		var out bytes.Buffer
		def := c.in == "\n" && c.want
		tm := NewTerminal(strings.NewReader(c.in), &out)
		got, err := tm.Confirm("execute now?", def)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("%q with default %v: got %v, want %v\n%s", c.in, def, got, c.want, out.String())
		}
	}

	// The default is shown, so an empty line is never ambiguous.
	var out bytes.Buffer
	tm := NewTerminal(strings.NewReader("\n"), &out)
	if _, err := tm.Confirm("sure?", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "[y/N]") {
		t.Fatalf("the default must be visible in the prompt:\n%s", out.String())
	}
}

// EOF in the middle of a confirmation is an error, never a default: the caller
// aborts, which is the only safe reading of a question nobody answered.
func TestConfirmReportsEOFInsteadOfTakingTheDefault(t *testing.T) {
	tm := NewTerminal(strings.NewReader(""), &bytes.Buffer{})
	if got, err := tm.Confirm("execute now?", true); err == nil {
		t.Fatalf("EOF must be an error, got %v", got)
	}
}

// Secret reads a line when stdin is not a terminal, which is how a scripted run
// supplies a password. The terminal path needs a pty and is not exercised here;
// this pins the branch every test and every CI run takes.
func TestSecretReadsALineWithoutATerminal(t *testing.T) {
	var out bytes.Buffer
	tm := NewTerminal(strings.NewReader("hunter2\n"), &out)
	got, err := tm.Secret("password")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hunter2" {
		t.Fatalf("secret = %q", got)
	}
	if strings.Contains(out.String(), "hunter2") {
		t.Fatalf("the value must not be echoed:\n%s", out.String())
	}
}
