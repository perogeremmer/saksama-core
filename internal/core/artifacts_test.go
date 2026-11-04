package core

import (
	"sort"
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
