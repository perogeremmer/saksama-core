//go:build !linux

package process

// isZombie has no portable implementation outside Linux. Callers fall back to
// signal probing, which is correct enough on those platforms.
//
// This file exists so the package compiles everywhere, not because anything else
// is supported: saksama runs where the copy runs, ci.yml only exercises Linux, and
// the release publishes Linux only. Outside Linux the pid identity check that
// `cancel` and `retire` rely on is absent, so a recycled pid could be trusted.
// docs/COMPATIBILITY.md says so in the host table.
func isZombie(int) bool { return false }
