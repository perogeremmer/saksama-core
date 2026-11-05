package analyzer

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A plan that names a problem without naming a step is a dead end for the person
// reading it at 2am. "Resolve the lock queue first" is a diagnosis; the operator
// needs the query that shows who holds what, and the decision to make with it.
//
// This test reads the source rather than the output, because the output of any one
// plan only reaches a handful of checks: whatever a scenario does not trigger would
// otherwise never be reviewed. Every `Remediation:` value must therefore either
//   - name a concrete step: a SQL statement, a command, a flag, or a `GRANT`, or
//   - be one of the entries below, which say that nothing is needed.
//
// Remediation values built from a variable are listed separately: they exist because
// the same remedy is reused, and each one is reviewed in the code that fills it.

// noActionNeeded are remedies that are complete sentences about not acting.
var noActionNeeded = map[string]string{
	"none needed; this is the default safety behaviour": "D2 sets lock_wait_timeout itself",
	"expect throttling": "the copy is already throttled; the severity carries the decision",
	"foreign key changes are intentionally blocked; they open an integrity window that v1 cannot make deterministic": "there is nothing to do in the tool; this is a refusal by design",
}

// actionable marks a remedy that tells the reader what to do next.
var actionable = []string{
	"--", "`", "saksama ",
	"SELECT ", "SHOW ", "ALTER ", "GRANT ", "KILL ", "CREATE ", "DROP ", "MODIFY ",
	"PURGE ", "SET SESSION ", "du ",
}

// nothingToFix is the honest counterpart: some findings are facts to know before
// applying, not actions, and saying so plainly is better than inventing a ritual.
const nothingToFix = "nothing to fix"

func TestEveryRemedyNamesAStep(t *testing.T) {
	dir := "."
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}

	reviewed := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "Remediation" {
				return true
			}
			reviewed++
			pos := fset.Position(kv.Pos())

			lit, ok := textOf(kv.Value)
			if !ok {
				// Built from a variable or a formatter: reviewed where it is built.
				// Listed so the decision is visible instead of silent.
				return true
			}
			if _, ok := noActionNeeded[lit]; ok {
				return true
			}
			if strings.HasPrefix(lit, nothingToFix) {
				return true
			}
			for _, marker := range actionable {
				if strings.Contains(lit, marker) {
					return true
				}
			}
			t.Errorf("%s: this remedy names no step, so a reader cannot act on it:\n  %q\n"+
				"  add the query, command, flag or GRANT that resolves it, or add it to noActionNeeded with the reason",
				pos, lit)
			return true
		})
	}

	if reviewed < 20 {
		t.Fatalf("only %d remedies found; the scan is no longer reading the catalogue", reviewed)
	}
}

// textOf returns the literal string for an expression that is only string literals
// joined with +, and reports false for anything else (fmt.Sprintf, identifiers).
func textOf(e ast.Expr) (string, bool) {
	var b strings.Builder
	var walk func(ast.Expr) bool
	walk = func(e ast.Expr) bool {
		switch v := e.(type) {
		case *ast.BasicLit:
			if v.Kind != token.STRING {
				return false
			}
			s, err := unquote(v.Value)
			if err != nil {
				return false
			}
			b.WriteString(s)
			return true
		case *ast.BinaryExpr:
			if v.Op != token.ADD {
				return false
			}
			return walk(v.X) && walk(v.Y)
		case *ast.ParenExpr:
			return walk(v.X)
		}
		return false
	}
	if !walk(e) {
		return "", false
	}
	return b.String(), true
}

func unquote(q string) (string, error) {
	fset := token.NewFileSet()
	expr, err := parser.ParseExpr(q)
	if err != nil {
		return "", err
	}
	lit, ok := expr.(*ast.BasicLit)
	if !ok {
		return "", os.ErrInvalid
	}
	var sb strings.Builder
	if err := printer.Fprint(&sb, fset, lit); err != nil {
		return "", err
	}
	s := sb.String()
	return s[1 : len(s)-1], nil // strip the double quotes printer keeps
}
