package cli

import (
	"runtime/debug"
	"strings"
)

// buildVersion resolves what `saksama version` reports.
//
// The release pipeline injects Version with -ldflags, and an injected value
// always wins. It does not always arrive: `go install
// github.com/perogeremmer/saksama-core/cmd/saksama@v0.2.0` - the install path
// the README opens with - passes no -ldflags at all, so the "dev" default would
// survive into a genuine release binary and there would be no way to tell it
// apart from a working copy.
//
// The toolchain does record the version it resolved, so that is the fallback.
// For `go install ...@v0.2.0` it is v0.2.0; for a plain `go build` in a git
// checkout it is the nearest tag, plus "+dirty" when the tree has uncommitted
// work. Only a build with no version at all - "(devel)", or no VCS metadata -
// falls back to "dev", which stays the honest answer when nothing is known.
// A build from a working tree with no tags is exactly that case.
func buildVersion(injected string) string {
	if injected == "" {
		injected = "dev"
	}
	if injected != "dev" {
		return injected
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return injected
	}
	if v := usableVersion(info.Main.Version); v != "" {
		return v
	}
	return injected
}

// usableVersion normalises a toolchain-recorded version, and reports "" for the
// values that are not versions at all. goreleaser injects "0.2.0" while module
// versions carry "v0.2.0"; one shape, so two install paths cannot disagree
// about the same release.
func usableVersion(v string) string {
	if v == "" || v == "(devel)" {
		return ""
	}
	return strings.TrimPrefix(v, "v")
}
