//go:build unix && !aix

// This file carries the unix implementation of the cross-process history
// lock. The build tag is "unix && !aix", not a bare "unix": the meta-tag
// "unix" also matches AIX, but golang.org/x/sys/unix ships no Flock there,
// so AIX falls through to the no-op fallback in flock_other.go alongside
// Windows/Plan9/js. Every other unix (linux, darwin, *bsd, illumos,
// solaris) has flock(2) and lands here.
package events

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// acquireHistoryLock takes an exclusive, cross-process advisory lock keyed on
// lockPath and returns a release closure the caller must call (defer) to drop
// it. It is the flock(2) that closes the gap appendMu cannot: appendMu only
// serializes goroutines WITHIN one fleetops process, but two cockpits pointed
// at the same ~/.fleetops/history are separate processes and would otherwise
// race Append's stat-then-rename rotation, clobbering the ".1" backup (silent
// audit-data loss).
//
// The lock is taken on a DEDICATED lockfile (<session>.jsonl.lock), never on
// the history file itself. That is deliberate: rotation renames the history
// file out from under itself (path -> path.1), and an flock follows the open
// file description, not the path — so a lock held on the history fd would keep
// protecting the renamed-away inode while a second process opens a fresh file
// at the same path and locks a DIFFERENT inode, defeating the exclusion across
// exactly the rename we need to guard. A separate, never-renamed lockfile has
// a stable inode, so the lock spans the whole rotate-then-write critical
// section correctly. ReadAll/Read ignore the ".lock" file (it matches neither
// the ".jsonl" nor ".jsonl.1" suffix they look for), so it never pollutes
// history output.
//
// Blocking LOCK_EX, not LOCK_NB: on contention we WAIT rather than skip, so no
// event is dropped. The wait is crash-safe: flock is released by the kernel
// when the fd is closed, INCLUDING on process death, so a CRASHED cockpit can
// never wedge another. It is not, however, unconditionally bounded — a peer
// that is alive but stuck inside its critical section still holds the lock, and
// we will wait for it (that is the exclusion working as intended, not a bug).
// The critical section itself is tiny — a stat, an optional rename, one line
// write — so a healthy holder releases promptly. Append stays best-effort: an
// acquisition error is returned (the caller swallows it, per the package doc),
// never panicked.
//
// unix.Flock is a bare syscall wrapper with no EINTR handling of its own, so
// the blocking LOCK_EX wait is retried here: if a signal interrupts the wait
// the kernel returns EINTR, which is not a lock failure — resume waiting rather
// than fail the append (a returned error is swallowed upstream, i.e. a silently
// dropped event, the exact failure class this lock exists to close). Any other
// error is genuine and returned.
func acquireHistoryLock(lockPath string) (release func(), err error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if err == nil {
			break
		}
		if errors.Is(err, unix.EINTR) {
			continue // signal interrupted the blocking wait — resume it
		}
		f.Close()
		return nil, err
	}
	return func() {
		// Closing the fd releases the flock; LOCK_UN first is belt-and-braces
		// so the lock is gone the instant we return, not merely at GC/close.
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}
