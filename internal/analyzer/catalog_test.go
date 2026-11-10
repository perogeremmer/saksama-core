package analyzer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// docs/CHECKS.md says, in its own words, that its `done` column is "the number to
// trust: it is the one this file cannot inflate without the analyzer
// contradicting it". Nothing enforced that until this test.
//
// It reads both sides - the ID constants in this package and the catalogue in the
// document - and fails when they disagree in either direction: a check that fires
// but is documented as planned, and a row documented as done that no code emits.
//
// The counts line at the top of the file is checked too, because a stale total is
// how a catalogue starts lying politely.
func TestCheckCatalogueMatchesTheCode(t *testing.T) {
	docPath := filepath.Join("..", "..", "docs", "CHECKS.md")
	doc, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}

	catalogued, countsLine := parseCatalogue(t, string(doc))
	implemented := emittedIDs(t)

	if len(implemented) == 0 {
		t.Fatal("no check IDs were found; the parser is looking in the wrong place")
	}

	for id := range implemented {
		if !isCheckID(id) {
			continue
		}
		row, ok := catalogued[id]
		if !ok {
			t.Errorf("check %s fires but is not in docs/CHECKS.md", id)
			continue
		}
		if row.status != "done" {
			t.Errorf("check %s fires but the catalogue calls it %q", id, row.status)
		}
	}

	for id, row := range catalogued {
		if !isCheckID(id) || row.status != "done" {
			continue
		}
		if !implemented[id] {
			t.Errorf("docs/CHECKS.md says %s is done, but no code emits it", id)
		}
	}

	// The catalogue's own arithmetic: A-E only. R rows (engine routing) and S rows
	// (server settings) are separate tables and the counts line does not include
	// them, which is exactly the kind of scope a stale total loses track of.
	var done, planned, moved int
	for id, row := range catalogued {
		if len(id) == 0 || id[0] < 'A' || id[0] > 'E' {
			continue
		}
		switch row.status {
		case "done":
			done++
		case "planned":
			planned++
		case "moved":
			moved++
		}
	}
	if done != countsLine.done || planned != countsLine.planned || moved != countsLine.moved {
		t.Errorf("the counts line says %d done / %d planned / %d moved, the table has %d / %d / %d",
			countsLine.done, countsLine.planned, countsLine.moved, done, planned, moved)
	}
	if total := done + planned + moved; total != countsLine.total {
		t.Errorf("the counts line says %d catalogued, the table has %d", countsLine.total, total)
	}
}

// documentedSeverities is not asserted here on purpose: severity is a judgement
// that changes with evidence, and pinning it would make this test an obstacle to
// the thinking rather than to the drift.

// isCheckID reports whether an ID in the catalogue is a check. Server settings
// (S) are a different kind of row: they describe a variable, not a verdict.
func isCheckID(id string) bool {
	if id == "" {
		return false
	}
	switch {
	case id[0] >= 'A' && id[0] <= 'E', id[0] == 'L', id[0] == 'R':
		return true
	}
	return false
}

type catalogueRow struct {
	severity string
	status   string
}

type counts struct {
	done, planned, moved, total int
}

var (
	checkIDPattern = regexp.MustCompile(`^[A-Z][0-9]+[a-z]?$`)
	countsPattern  = regexp.MustCompile(`\*\*([0-9]+) implemented check IDs\*\* out of \*\*([0-9]+) catalogued\*\*`)
	countsParts    = regexp.MustCompile(`([0-9]+) ` + "`(done|planned|moved)`")
)

func parseCatalogue(t *testing.T, doc string) (map[string]catalogueRow, counts) {
	t.Helper()
	out := map[string]catalogueRow{}

	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(line, "| ") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 6 {
			continue
		}
		id := strings.Trim(strings.TrimSpace(cells[1]), "`")
		if !checkIDPattern.MatchString(id) {
			continue
		}
		out[id] = catalogueRow{
			severity: strings.TrimSpace(cells[3]),
			status:   strings.Trim(strings.TrimSpace(cells[4]), "`"),
		}
	}

	var c counts
	m := countsPattern.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("docs/CHECKS.md no longer carries a counts line this test can read")
	}
	c.done, _ = strconv.Atoi(m[1])
	c.total, _ = strconv.Atoi(m[2])
	for _, part := range countsParts.FindAllStringSubmatch(doc, -1) {
		n, _ := strconv.Atoi(part[1])
		switch part[2] {
		case "done":
			c.done = n
		case "planned":
			c.planned = n
		case "moved":
			c.moved = n
		}
	}
	return out, c
}

// emittedIDs reads both spellings the package uses: `chkX = "D2"` constants that
// are referenced somewhere, and `ID: "R5"` literals written inline. A constant that
// is declared and never referenced emits nothing - C4 and C6 are exactly that,
// superseded by the engine matrix and kept as tombstones so the IDs are not reused.
//
// Reading the source, rather than one plan's output, is the same reasoning as
// remedy_test: a single scenario only reaches a handful of checks.
func emittedIDs(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	consts := map[string]string{} // chk name -> ID
	declaredAt := map[token.Pos]bool{}
	type parsed struct {
		file *ast.File
		fset *token.FileSet
	}
	var parsedFiles []parsed

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		parsedFiles = append(parsedFiles, parsed{file, fset})
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range spec.Names {
				if !strings.HasPrefix(name.Name, "chk") || i >= len(spec.Values) {
					continue
				}
				declaredAt[name.Pos()] = true
				lit, ok := spec.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if value, err := strconv.Unquote(lit.Value); err == nil {
					consts[name.Name] = value
				}
			}
			return true
		})
	}

	used := map[string]bool{}
	inline := map[string]bool{}
	for _, p := range parsedFiles {
		ast.Inspect(p.file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.Ident:
				// The declaration itself is not a use: a constant kept as a
				// tombstone (C4, C6) would otherwise look like it still fires.
				if _, ok := consts[node.Name]; ok && !declaredAt[node.Pos()] {
					used[node.Name] = true
				}
			case *ast.KeyValueExpr:
				key, ok := node.Key.(*ast.Ident)
				if !ok || key.Name != "ID" {
					return true
				}
				if lit, ok := node.Value.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if value, err := strconv.Unquote(lit.Value); err == nil {
						inline[value] = true
					}
				}
			}
			return true
		})
	}

	out := map[string]bool{}
	for name, value := range consts {
		if used[name] {
			out[value] = true
		}
	}
	for id := range inline {
		out[id] = true
	}
	return out
}
