//go:build !linux

package process

// isZombie has no portable implementation outside Linux. Callers fall back to
// signal probing, which is correct enough on those platforms.
func isZombie(int) bool { return false }
