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
