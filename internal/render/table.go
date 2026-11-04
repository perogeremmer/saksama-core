// Package render draws CLI output.
//
// Tables are ASCII-only on purpose: Saksama runs on servers with TERM=dumb,
// locale C, and unpredictable SSH clients, where box-drawing characters turn
// into mojibake. Borders made of +, - and | are unambiguous everywhere.
package render

import (
	"fmt"
	"io"
	"strings"
)

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
		widths[i] = len(c.Header)
	}
	for _, row := range t.Rows {
		for i := range t.Columns {
			if i >= len(row) {
				continue
			}
			cell := t.prepare(row[i])
			if len(cell) > widths[i] {
				widths[i] = len(cell)
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
			pad := strings.Repeat(" ", widths[i]-len(val))
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
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
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
