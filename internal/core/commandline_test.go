package core

import (
	"strings"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/cred"
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
