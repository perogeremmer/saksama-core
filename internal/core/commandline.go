package core

import (
	"fmt"
	"regexp"
	"strings"
)

// shellSafe matches the characters that never need quoting in a shell.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// CommandLine returns the non-interactive command that does exactly what this
// request does.
//
// It exists so an interactive frontend can never drift from the flags: the wizard
// prints this line before it acts, which teaches the operator the real interface,
// makes the action reproducible in CI, and keeps one source of truth for how a
// request is spelled.
func (req Request) CommandLine() string {
	return req.CommandLineFor("apply")
}

// CommandLineFor is CommandLine with a different command name, so a preview can
// print `saksama plan --live ...` and an execution can print `saksama apply ...`
// from the same request, without either of them re-spelling the flags.
func (req Request) CommandLineFor(verb string) string {
	if verb == "" {
		verb = "apply"
	}
	args := append([]string{"saksama"}, strings.Fields(verb)...)

	if req.ResumedFrom != "" && verb == "apply" {
		args = append(args, "--session", req.ResumedFrom)
	} else {
		if req.Database != "" {
			args = append(args, "-d", req.Database)
		}
		if req.Table != "" {
			args = append(args, "-t", req.Table)
		}
		if req.Alter != "" {
			args = append(args, "-a", req.Alter)
		}
	}

	if req.Conn.Host != "" {
		args = append(args, "-h", req.Conn.Host)
	}
	if req.Conn.Port != "" {
		args = append(args, "-P", req.Conn.Port)
	}
	if req.Conn.User != "" {
		args = append(args, "-u", req.Conn.User)
	}
	if req.Production {
		args = append(args, "--production", "--confirm", req.Conn.Describe())
	}
	// Without this, the command a 5.6 production preview prints is the one that
	// would be refused: the operator would read "equivalent" and copy a refusal.
	if req.AcceptUnknownMDL {
		args = append(args, "--accept-unknown-mdl")
	}
	if req.EngineHint != "" && req.EngineHintSource == "flag" {
		args = append(args, "--engine", string(req.EngineHint))
	}
	if req.FreeDisk > 0 {
		args = append(args, "--free-disk", fmt.Sprint(req.FreeDisk))
	}
	if req.DiskTotal > 0 {
		args = append(args, "--disk-total", fmt.Sprint(req.DiskTotal))
	}
	if req.LockWaitTimeoutSeconds > 0 && req.LockWaitTimeoutSeconds != 10 {
		args = append(args, "--lock-wait-timeout", fmt.Sprint(req.LockWaitTimeoutSeconds))
	}
	if req.HoldCutover {
		args = append(args, "--hold-cutover")
	}
	if req.Detach {
		args = append(args, "--detach")
	}
	if req.DryRun {
		args = append(args, "--dry-run")
	}
	if req.Force {
		args = append(args, "--force")
	}

	return joinQuoted(args)
}

// joinQuoted spell a command so it can be pasted into a shell unchanged.
func joinQuoted(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, shellQuote(a))
	}
	return strings.Join(quoted, " ")
}

// shellQuote wraps a value only when a shell would otherwise split or expand it.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
