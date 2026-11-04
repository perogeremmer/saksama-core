package core

import (
	"strings"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/cred"
	"github.com/perogeremmer/saksama-core/internal/state"
)

func sampleRequest() Request {
	return Request{
		Conn:     cred.Conn{Host: "db.prod", Port: "3306", User: "deployer", Database: "shop"},
		Database: "shop",
		Table:    "orders",
		Alter:    "MODIFY COLUMN status varchar(64) NOT NULL",
	}
}

func TestCommandLineIsRunnableAsTyped(t *testing.T) {
	got := sampleRequest().CommandLineFor("plan --live")
	want := `saksama plan --live -d shop -t orders -a 'MODIFY COLUMN status varchar(64) NOT NULL' -h db.prod -P 3306 -u deployer`
	if got != want {
		t.Fatalf("got:\n  %s\nwant:\n  %s", got, want)
	}
}

// A production request has to name the host twice, once as a flag and once as the
// confirmation, because that is what the non-interactive command requires.
func TestCommandLineCarriesTheProductionConfirmation(t *testing.T) {
	req := sampleRequest()
	req.Production = true
	req.Alter = "ADD INDEX ix_status (status)"

	got := req.CommandLine()
	for _, want := range []string{"saksama apply", "--production", "--confirm db.prod", "-a 'ADD INDEX ix_status (status)'"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "--state-dir") {
		t.Errorf("a password or state directory must never be spelled into the preview: %s", got)
	}
}

func TestCommandLineUsesTheSessionFormWhenItResumes(t *testing.T) {
	req := sampleRequest()
	req.ResumedFrom = "s7"
	got := req.CommandLine()
	if !strings.HasPrefix(got, "saksama apply --session s7") {
		t.Fatalf("got %s", got)
	}
	if strings.Contains(got, "-a ") {
		t.Fatalf("a resumed execution must not repeat the ALTER: %s", got)
	}
}

func TestCommandLineQuotesOnlyWhatNeedsIt(t *testing.T) {
	req := sampleRequest()
	req.Conn.User = "deployer"
	req.Database = "shop"
	got := req.CommandLineFor("apply")
	if !strings.Contains(got, "-d shop -t orders") {
		t.Fatalf("plain identifiers must not be quoted: %s", got)
	}

	req.Table = "weird name"
	if !strings.Contains(req.CommandLine(), `-t 'weird name'`) {
		t.Fatalf("a space needs quoting: %s", req.CommandLine())
	}

	req.Table = "o'brien"
	if !strings.Contains(req.CommandLine(), `'o'\''brien'`) {
		t.Fatalf("a quote inside a value must be escaped: %s", req.CommandLine())
	}
}

func TestCommandLineShowsTheEngineOnlyWhenTheOperatorChoseIt(t *testing.T) {
	req := sampleRequest()
	req.EngineHint = analyzer.EnginePtOSC
	req.EngineHintSource = "recorded"
	if strings.Contains(req.CommandLine(), "--engine") {
		t.Fatalf("a recorded choice is already in the state directory, so repeating it is noise: %s", req.CommandLine())
	}
	req.EngineHintSource = "flag"
	if !strings.Contains(req.CommandLine(), "--engine pt-osc") {
		t.Fatalf("a flag must be reproduced: %s", req.CommandLine())
	}
}

// A 5.6 production preview must print the command that would actually run. Without
// --accept-unknown-mdl the printed line is the one the planner refuses.
func TestCommandLineCarriesTheMDLAcceptance(t *testing.T) {
	req := sampleRequest()
	req.Production = true
	req.AcceptUnknownMDL = true

	got := req.CommandLineFor("plan --live")
	for _, want := range []string{"--production", "--confirm db.prod", "--accept-unknown-mdl"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}

	// And it must not appear when the risk was not accepted: a flag that shows up
	// for everyone would teach nothing about the one case it exists for.
	plain := sampleRequest().CommandLineFor("plan --live")
	if strings.Contains(plain, "--accept-unknown-mdl") {
		t.Errorf("the flag appeared without being set: %s", plain)
	}
}

func TestEngineChoiceCommandLines(t *testing.T) {
	cases := []struct {
		engine, database, table string
		global                  bool
		note                    string
		wantSet                 string
		wantClear               string
	}{
		{"pt-osc", "shop", "orders", false, "foreign keys must be copied",
			`saksama engine set pt-osc -d shop -t orders --note 'foreign keys must be copied'`,
			`saksama engine clear -d shop -t orders`},
		{"gh-ost", "shop", "", false, "",
			"saksama engine set gh-ost -d shop",
			"saksama engine clear -d shop"},
		{"native", "", "", true, "",
			"saksama engine set native --global",
			"saksama engine clear --global"},
	}

	for _, c := range cases {
		choice, err := state.NewChoice(c.engine, c.global, c.database, c.table)
		if err != nil {
			t.Fatal(err)
		}
		choice.Note = c.note
		if got := EngineChoiceCommandLine(choice); got != c.wantSet {
			t.Errorf("set:\n  got  %s\n  want %s", got, c.wantSet)
		}
		if got := EngineClearCommandLine(choice); got != c.wantClear {
			t.Errorf("clear:\n  got  %s\n  want %s", got, c.wantClear)
		}
	}
}
