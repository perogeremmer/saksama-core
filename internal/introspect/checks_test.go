package introspect

import "testing"

// The catalogue returns "(`x` > 0)" and the parser returns "`x` > 0". One spelling
// matters: the probe wraps the expression in WHERE NOT (...) and the plan prints it as
// a query to paste, so a second layer of parentheses here shows up in both.
func TestStripCheckWrapper(t *testing.T) {
	cases := []struct{ in, want string }{
		{"(`x` > 0)", "`x` > 0"},
		{"((`x` > 0))", "`x` > 0"},
		{"json_valid(`j`)", "json_valid(`j`)"},
		{"(`status` in ('a','b')) and (`id` > 0)", "(`status` in ('a','b')) and (`id` > 0)"},
		// Not a wrapper: the opening paren closes early, so stripping it would change
		// the expression's meaning.
		{"(`a`) + (`b`)", "(`a`) + (`b`)"},
		{"(`a` > 0", "(`a` > 0"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripCheckWrapper(c.in); got != c.want {
			t.Errorf("stripCheckWrapper(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
