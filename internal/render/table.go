// Package render draws CLI output.
//
// Tables are ASCII-only on purpose: Saksama runs on servers with TERM=dumb,
// locale C, and unpredictable SSH clients, where box-drawing characters turn
// into mojibake. Borders made of +, - and | are unambiguous everywhere.
package render

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ansiEscape matches the escape sequences the palette writes: CSI, parameters,
// one final byte. Anything it matches is invisible on screen and therefore zero
// width to a table.
var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")

// stripANSI returns s without its escape sequences.
func stripANSI(s string) string { return ansiEscape.ReplaceAllString(s, "") }

// visibleLen is the width a cell occupies on screen: escape sequences count for
// nothing, and the rest counts in runes, so a UTF-8 table name is not measured
// as three columns wide per character.
func visibleLen(s string) int { return utf8.RuneCountInString(stripANSI(s)) }

// truncateVisible cuts s to n visible runes, copying escape sequences through
// untouched and never cutting one in half. A cut inside coloured text closes
// the colour, so the reset cannot bleed into the padding or the next cell.
func truncateVisible(s string, n int) string {
	var b strings.Builder
	count, cut := 0, false
	// esc walks the same grammar the palette writes and ansiEscape matches:
	// ESC, then '[', then parameters, then one final letter.
	const (
		escNone = iota
		escGot
		escCSI
	)
	esc := escNone
	for _, r := range s {
		switch esc {
		case escGot:
			esc = escNone
			if r == '[' {
				esc = escCSI
			}
			b.WriteRune(r)
			continue
		case escCSI:
			b.WriteRune(r)
			switch {
			case r >= '0' && r <= '9', r == ';':
				// still in the parameters
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
				esc = escNone
			default:
				esc = escNone
			}
			continue
		}
		if r == '\x1b' {
			esc = escGot
			b.WriteRune(r)
			continue
		}
		if count == n {
			cut = true
			break
		}
		b.WriteRune(r)
		count++
	}
	out := b.String()
	if cut && strings.Contains(s, "\x1b[") && !strings.HasSuffix(out, ansiReset) {
		out += ansiReset
	}
	return out
}

// Align is a column alignment.
type Align int

const (
	// Left aligns text to the left, which is right for names.
	Left Align = iota
	// Right aligns numbers so magnitudes line up.
	Right
)

// Column is one table column.
type Column struct {
	Header string
	Align  Align
}

// Table is a small table renderer.
//
// A cell may carry colour: widths are measured from the visible text, the
// padding re-applies what the escapes hide, and truncation never lands inside a
// sequence. What a piped table and a terminal table must have in common is the
// geometry, not the bytes.
type Table struct {
	Columns []Column
	Rows    [][]string
	// Border draws the +---+ frame. When false the table is space-aligned only,
	// which is what you want when piping into another tool.
	Border bool
	// MaxWidth caps a cell, truncating with "..." so one long value cannot push
	// the whole table off a 80-column terminal.
	MaxWidth int
}

// Add appends a row. Missing cells are filled with "".
func (t *Table) Add(cells ...string) {
	row := make([]string, len(t.Columns))
	copy(row, cells)
	t.Rows = append(t.Rows, row)
}

// Render writes the table.
func (t *Table) Render(w io.Writer) error {
	if len(t.Columns) == 0 {
		return nil
	}

	widths := make([]int, len(t.Columns))
	for i, c := range t.Columns {
		widths[i] = utf8.RuneCountInString(c.Header)
	}
	for _, row := range t.Rows {
		for i := range t.Columns {
			if i >= len(row) {
				continue
			}
			cell := t.prepare(row[i])
			if v := visibleLen(cell); v > widths[i] {
				widths[i] = v
			}
		}
	}

	var b strings.Builder
	rule := func(l, m, r string) {
		b.WriteString(l)
		for i, w := range widths {
			b.WriteString(strings.Repeat("-", w+2))
			if i == len(widths)-1 {
				b.WriteString(r)
			} else {
				b.WriteString(m)
			}
		}
		b.WriteString("\n")
	}
	line := func(cells []string, header bool) {
		if t.Border {
			b.WriteString("|")
		}
		for i, c := range t.Columns {
			val := ""
			if i < len(cells) {
				val = t.prepare(cells[i])
			}
			if header {
				val = c.Header
			}
			pad := strings.Repeat(" ", widths[i]-visibleLen(val))
			if t.Border {
				if c.Align == Right && !header {
					b.WriteString(" " + pad + val + " ")
				} else {
					b.WriteString(" " + val + pad + " ")
				}
				b.WriteString("|")
			} else {
				if c.Align == Right && !header {
					b.WriteString(pad + val)
				} else {
					b.WriteString(val + pad)
				}
				if i == len(t.Columns)-1 {
					b.WriteString("\n")
				} else {
					b.WriteString("  ")
				}
			}
		}
		if t.Border {
			b.WriteString("\n")
		}
	}

	if t.Border {
		rule("+", "+", "+")
	}
	headers := make([]string, len(t.Columns))
	line(headers, true)
	if t.Border {
		rule("+", "+", "+")
	}
	for _, row := range t.Rows {
		line(row, false)
	}
	if t.Border {
		rule("+", "+", "+")
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// prepare truncates a cell to MaxWidth and strips control characters.
func (t *Table) prepare(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.Join(strings.Fields(s), " ")
	max := t.MaxWidth
	if max <= 3 {
		max = 60
	}
	if visibleLen(s) <= max {
		return s
	}
	return truncateVisible(s, max-3) + "..."
}

// plainTable builds a borderless table.
func plainTable(cols ...Column) *Table {
	return &Table{Columns: cols}
}

// boxTable builds a bordered table.
func boxTable(cols ...Column) *Table {
	return &Table{Columns: cols, Border: true, MaxWidth: 48}
}

var _ = fmt.Sprintf
