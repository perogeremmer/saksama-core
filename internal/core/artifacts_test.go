package core

import (
	"sort"
	"strings"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// The names recorded here are what cleanup is allowed to drop, so they have to be
// the names the engines actually create. Observed, not assumed:
//
//	gh-ost 1.1.11  _t_gho, _t_ghc (_t_del after a cut-over), and no triggers at all
//	pt-osc 3.7.2   _t_new, _t_old during a swap, plus pt_osc_<db>_<t>_{ins,upd,del}
//
// gh-ost is triggerless by design: it reads the binary log. Recording trigger names
// for it made cleanup claim it had dropped three objects that never existed, while
// the changelog table it really leaves behind was unowned and never dropped.
func TestRecordedArtifactsMatchWhatTheEnginesCreate(t *testing.T) {
	cases := []struct {
		engine analyzer.Engine
		want   []string
	}{
		{
			engine: analyzer.EngineGhOst,
			want: []string{
				"table _t_gho", "table _t_ghc", "table _t_del",
			},
		},
		{
			engine: analyzer.EnginePtOSC,
			want: []string{
				"table _t_new", "table _t_old",
				"trigger pt_osc_shop_t_ins", "trigger pt_osc_shop_t_upd", "trigger pt_osc_shop_t_del",
			},
		},
	}

	for _, c := range cases {
		sess := &state.Session{Database: "shop", Table: "t"}
		recordExpectedArtifacts(sess, c.engine, Request{Database: "shop"})

		var got []string
		for _, a := range sess.Artifacts {
			got = append(got, a.Kind+" "+a.Name)
		}
		sort.Strings(got)

		want := append([]string(nil), c.want...)
		sort.Strings(want)

		if len(got) != len(want) {
			t.Fatalf("%s recorded %v, want %v", c.engine, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s recorded %v, want %v", c.engine, got, want)
				break
			}
		}
	}
}

// Two state directories both start at "s1". A cut-over gate named after the
// session alone would let one run release the other's swap, and a late cleanup
// from the first would delete the second's gate file while its engine waited.
func TestCutoverGatePathIsUniquePerStateDirectory(t *testing.T) {
	a := cutoverGatePath("/home/a/.local/state/saksama", "s1")
	b := cutoverGatePath("/home/b/.local/state/saksama", "s1")
	if a == b {
		t.Fatalf("two state directories produced the same gate path: %s", a)
	}
	if !strings.Contains(a, "s1") {
		t.Errorf("the session id must stay readable in the path: %s", a)
	}
	// Stable for the same inputs, or `cutover` could not find the file it recorded.
	if a != cutoverGatePath("/home/a/.local/state/saksama", "s1") {
		t.Error("the gate path must be stable for one session")
	}
}
