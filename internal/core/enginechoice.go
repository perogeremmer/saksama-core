package core

import (
	"strings"

	"github.com/perogeremmer/saksama-core/internal/state"
)

// EngineScopeFlags renders a recorded choice's scope as the flags that reproduce
// it: "", "-d shop", "-d shop -t orders", or "--global".
//
// It lives here, next to CommandLineFor, for the same reason: the CLI prints it as
// "check it" and "undo it", and a frontend that records a choice prints it as the
// equivalent command. Two spellings of the same scope would drift apart, and the
// flags people copy are the ones that must not.
func EngineScopeFlags(c state.EngineChoice) string {
	switch c.Scope {
	case state.ScopeDatabase:
		return " -d " + c.Database
	case state.ScopeTable:
		return " -d " + c.Database + " -t " + c.Table
	}
	return " --global"
}

// EngineChoiceCommandLine is the non-interactive command that records this
// choice, including the note that explains it.
func EngineChoiceCommandLine(c state.EngineChoice) string {
	args := []string{"saksama", "engine", "set", c.Engine}
	args = append(args, strings.Fields(strings.TrimSpace(EngineScopeFlags(c)))...)
	if c.Note != "" {
		args = append(args, "--note", c.Note)
	}
	return joinQuoted(args)
}

// EngineClearCommandLine is the command that forgets this choice.
func EngineClearCommandLine(c state.EngineChoice) string {
	args := []string{"saksama", "engine", "clear"}
	args = append(args, strings.Fields(strings.TrimSpace(EngineScopeFlags(c)))...)
	return joinQuoted(args)
}
