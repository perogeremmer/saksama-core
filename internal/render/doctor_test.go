package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/enginesetup"
)

func TestDoctorShowsEachFailureWithItsFix(t *testing.T) {
	lists := []enginesetup.Checklist{
		{
			Engine: "gh-ost", Version: "1.1.11", Evidence: "verified", Target: "linux/amd64",
			Items: []enginesetup.Item{
				{Name: "platform", State: enginesetup.StateOK, Detail: "gh-ost-linux-amd64.tar.gz is published for linux/amd64"},
				{Name: "binary", State: enginesetup.StateMissing,
					Detail: "not found on PATH or in /home/x/.local/share/saksama/bin",
					Fix:    "saksama engine install gh-ost"},
				{Name: "server", State: enginesetup.StateUnknown, Detail: "not checked: no server given"},
			},
			Ready: false, NotChecked: 1, Verdict: "cannot run: binary",
		},
	}

	var buf bytes.Buffer
	if err := Doctor(&buf, lists, true); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{
		"GH-OST 1.1.11 (verified)",
		"linux/amd64",
		"[  ok] platform",
		"[FAIL] binary",
		"-> saksama engine install gh-ost",
		"[  ??] server",
		"verdict  cannot run: binary",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	for _, r := range out {
		if r > 127 {
			t.Fatalf("non-ASCII rune %q in doctor output", r)
		}
	}
}

func TestDoctorSeparatesEngines(t *testing.T) {
	lists := []enginesetup.Checklist{
		{Engine: "gh-ost", Version: "1.1.11", Ready: true, Verdict: "ready"},
		{Engine: "pt-osc", Version: "3.7.2", Ready: false, Verdict: "cannot run: DBD::mysql"},
	}
	var buf bytes.Buffer
	if err := Doctor(&buf, lists, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Count(out, "verdict") != 2 {
		t.Fatalf("expected one verdict per engine:\n%s", out)
	}
	if !strings.Contains(out, "PT-OSC 3.7.2") {
		t.Fatalf("second engine missing:\n%s", out)
	}
}
