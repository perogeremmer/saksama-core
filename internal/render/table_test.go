package render

import (
	"bytes"
	"strings"
	"testing"
)

// renderTable runs a table with colour off and on, and returns both outputs.
func renderTable(t *testing.T, build func() *Table) (plain, colored string) {
	t.Helper()
	UseColor(false)
	var p bytes.Buffer
	if err := build().Render(&p); err != nil {
		t.Fatal(err)
	}
	UseColor(true)
	defer UseColor(false)
	var c bytes.Buffer
	if err := build().Render(&c); err != nil {
		t.Fatal(err)
	}
	return p.String(), c.String()
}

// The contract of colour in a table: the text is the same with it off and on,
// and every row keeps its geometry. An escape sequence counted as a character
// would push the border of every row after the first out of line.
func TestTableStaysAlignedWithColour(t *testing.T) {
	build := func() *Table {
		return &Table{
			Border: true,
			Columns: []Column{
				{Header: "SETTING"}, {Header: "STATUS"}, {Header: "NEEDED BY"},
			},
			Rows: [][]string{
				{"log_bin", Good("ok"), "gh-ost"},
				{"server_id", Bad("FAIL"), "gh-ost, pt-osc"},
				{"gtid_mode", Warn("warn"), ""},
				{"a-plain-row", "ok", "native"},
			},
		}
	}
	plain, colored := renderTable(t, build)

	if strings.Contains(plain, "\x1b[") {
		t.Fatal("colour is off but escape codes were written")
	}
	if got := ansi.ReplaceAllString(colored, ""); got != plain {
		t.Fatalf("the table text changed when colour was added:\n--- plain ---\n%s\n--- stripped ---\n%s", plain, got)
	}

	// Every row, plain or coloured, spans the same number of visible columns.
	want := -1
	for _, line := range strings.Split(plain, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		if want == -1 {
			want = visibleLen(line)
		}
		if got := visibleLen(line); got != want {
			t.Fatalf("rows do not share a width: %q is %d, want %d", line, got, want)
		}
	}
	for _, line := range strings.Split(colored, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		if got := visibleLen(line); got != want {
			t.Fatalf("coloured rows do not share a width: %q is %d, want %d", line, got, want)
		}
	}
}

// A long coloured cell is truncated by what it shows, never by what it hides:
// no half sequence, no open colour bleeding into the padding or the next cell.
func TestTableNeverTruncatesInsideAnEscape(t *testing.T) {
	build := func() *Table {
		return &Table{
			Border:   true,
			MaxWidth: 8,
			Columns:  []Column{{Header: "NAME"}, {Header: "STATE"}},
			Rows: [][]string{
				{"a-very-long-table-name-indeed", Bad("FAILED")}, // the long cell is plain
				{Warn("a-very-long-warned-value"), Good("ok")},   // the long cell is coloured
			},
		}
	}
	plain, colored := renderTable(t, build)

	if got := ansi.ReplaceAllString(colored, ""); got != plain {
		t.Fatalf("truncation changed the text when colour was added:\n--- plain ---\n%s\n--- stripped ---\n%s", plain, got)
	}
	// Every data row is capped: a coloured cell shows no more than its plain
	// neighbour would.
	for _, line := range strings.Split(colored, "\n") {
		if !strings.HasPrefix(line, "|") || strings.Contains(line, "NAME") || strings.Contains(line, "--") {
			continue
		}
		if got := visibleLen(line); got > len("| a-ver... | FAILED |") {
			t.Fatalf("row escaped the width cap: %q is %d visible columns", line, got)
		}
	}
	// No sequence was cut in half: every escape that starts in a line is a
	// complete one, so nothing downstream is swallowed as escape parameters.
	for _, line := range strings.Split(colored, "\n") {
		starts := strings.Count(line, "\x1b[")
		if complete := len(ansiEscape.FindAllString(line, -1)); starts != complete {
			t.Fatalf("a truncated cell left a half-written sequence (%d starts, %d complete): %q", starts, complete, line)
		}
		if strings.Contains(line, "a-very-long-warned-value") {
			t.Fatalf("the coloured cell was not truncated: %q", line)
		}
	}
}

// Widths are measured in runes, so a UTF-8 name lines up with ASCII ones.
func TestTableMeasuresRunesNotBytes(t *testing.T) {
	var b bytes.Buffer
	tt := &Table{
		Border: true,
		Columns: []Column{
			{Header: "TABLE"}, {Header: "ROWS", Align: Right},
		},
		Rows: [][]string{
			{"pesanan", "1000"}, // ASCII
			{"tabelü", "5"},     // same rune count, more bytes
		},
	}
	if err := tt.Render(&b); err != nil {
		t.Fatal(err)
	}
	widths := map[int]bool{}
	for _, line := range strings.Split(b.String(), "\n") {
		if strings.HasPrefix(line, "|") {
			widths[visibleLen(line)] = true
		}
	}
	if len(widths) != 1 {
		t.Fatalf("a multi-byte name broke the alignment: %v", b.String())
	}
}
