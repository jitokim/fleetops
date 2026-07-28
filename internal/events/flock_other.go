//go:build !unix || aix

// Fallback for platforms with no flock(2) available through
// golang.org/x/sys/unix: Windows, Plan9, js/wasm, and AIX (which the "unix"
// meta-tag covers but x/sys/unix has no Flock for — see flock_unix.go). Keeps
// internal/events building on every GOOS.
package events

// acquireHistoryLock is a documented no-op where flock is unavailable: the
// cross-process rotation race stays theoretically possible on these
// platforms, but the in-process appendMu still fully serializes the common
// single-process case (one cockpit), which is the only case fleetops targets
// on them today. The signature matches flock_unix.go so Append is identical
// across builds; lockPath is accepted and ignored.
func acquireHistoryLock(lockPath string) (release func(), err error) {
	return func() {}, nil
}
