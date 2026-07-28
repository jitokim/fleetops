//go:build unix && !aix

package events

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This file drives a REAL cross-process reproduction of the rotation-clobber
// bug the flock closes. It re-execs the test binary itself as independent OS
// processes (not goroutines): when the env vars below are set, TestMain runs
// the worker branch and exits before any test runs, so each child is a genuine
// separate process racing Append on a shared history dir — exactly the "two
// cockpits pointed at the same ~/.fleetops/history" scenario appendMu could
// never cover.
const (
	xprocEnvDir     = "FLEETOPS_EVENTS_XPROC_DIR"     // set => this process is a worker
	xprocEnvSession = "FLEETOPS_EVENTS_XPROC_SESSION" // session id to append to
	xprocEnvWriter  = "FLEETOPS_EVENTS_XPROC_WRITER"  // this worker's index (for unique TS)
	xprocEnvMaxSize = "FLEETOPS_EVENTS_XPROC_MAXSIZE" // maxFileSize override for the child
	xprocEnvGate    = "FLEETOPS_EVENTS_XPROC_GATE"    // barrier file the worker waits to appear
)

// TestMain gives the test binary a second identity: a single history writer,
// selected by env, that a parent test can spawn as a real OS process. When the
// worker env is absent it is an ordinary test main.
func TestMain(m *testing.M) {
	if dir := os.Getenv(xprocEnvDir); dir != "" {
		os.Exit(runXProcWriter(dir))
	}
	os.Exit(m.Run())
}

// runXProcWriter is the worker branch: shrink maxFileSize as instructed, wait
// on the barrier so all workers rush Append together (widening the race window
// the flock has to survive), then append exactly one event.
func runXProcWriter(dir string) int {
	if s := os.Getenv(xprocEnvMaxSize); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			maxFileSize = v
		}
	}
	waitForGate(os.Getenv(xprocEnvGate))

	writer, _ := strconv.Atoi(os.Getenv(xprocEnvWriter))
	ev := Event{
		TS:        int64(writer), // small, distinct, and disjoint from canary TSs
		SessionID: os.Getenv(xprocEnvSession),
		ToState:   "running",
		Trigger:   TriggerScan,
		Actor:     ActorSystem,
	}
	if err := Append(dir, ev); err != nil {
		fmt.Fprintln(os.Stderr, "worker append:", err)
		return 1
	}
	return 0
}

// waitForGate blocks until the barrier file appears (bounded: it gives up after
// a second so a lost signal can never wedge a worker forever).
func waitForGate(gate string) {
	if gate == "" {
		return
	}
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(gate); err == nil {
			return
		}
		time.Sleep(1 * time.Millisecond)
	}
}

// TestAppend_CrossProcess_RotationDoesNotClobberBackup is the honest,
// real-cross-process reproduction. Each trial seeds one session's history file
// with a canary generation already OVER maxFileSize, then unleashes N separate
// OS processes that all Append the same session at once. The first append must
// rotate the oversize seed into the ".1" backup; the rest each add a tiny event
// that keeps the live file well under maxFileSize, so NO second rotation is
// due. The single-rotation contract therefore REQUIRES every canary event to
// survive in ".1".
//
// Without the flock this fails, and the observed failure path is the concurrent
// rotation HARD-ERRORING a second process's Append, not (primarily) a silent
// backup clobber: when several processes race the same stat-then-rename, a
// later os.Rename loses (the source has already been renamed away), Append
// returns that error, and the worker exits non-zero. Stubbing
// acquireHistoryLock to a no-op and running this repeatedly, every run tripped
// invariant (A) — a worker hard-failing its Append — so invariant (B), canary
// survival, was rarely even reached. Both are therefore asserted SEPARATELY and
// non-fatally below, so whichever way the missing lock manifests is credited:
//
//	(A) no worker process hard-failed its Append during the concurrent rotation
//	(B) every seeded canary event survived rotation into ".1"
//
// With the flock, only the first process rotates, no Append errors, and every
// canary is preserved — deterministically, every trial, which is why these
// assertions are safe to ship.
func TestAppend_CrossProcess_RotationDoesNotClobberBackup(t *testing.T) {
	const (
		childMaxSize = 8000
		canaryEvents = 25
		canaryBase   = int64(9_000_000)
		workers      = 16
		trials       = 5
	)

	for trial := 0; trial < trials; trial++ {
		dir := t.TempDir()
		session := "sess-xproc"
		path := filepath.Join(dir, session+".jsonl")

		// Seed an oversize canary generation (parseable JSONL) so the very next
		// Append is obliged to rotate it into ".1".
		seedCanary(t, path, canaryBase, canaryEvents, childMaxSize)

		gate := filepath.Join(dir, "GO")
		cmds := make([]*exec.Cmd, workers)
		for w := 0; w < workers; w++ {
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(),
				xprocEnvDir+"="+dir,
				xprocEnvSession+"="+session,
				xprocEnvWriter+"="+strconv.Itoa(w),
				xprocEnvMaxSize+"="+strconv.Itoa(childMaxSize),
				xprocEnvGate+"="+gate,
			)
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatalf("trial %d: start worker %d: %v", trial, w, err)
			}
			cmds[w] = cmd
		}
		// Release the barrier: all workers rush Append at once.
		if err := os.WriteFile(gate, []byte("go"), 0o644); err != nil {
			t.Fatalf("trial %d: open gate: %v", trial, err)
		}
		// Invariant (A): no worker process hard-failed. A losing os.Rename during
		// a concurrent rotation makes a second process's Append return an error
		// and its worker exit non-zero — the reproducible way the missing lock
		// bites. Recorded non-fatally (t.Errorf, not t.Fatalf) so every worker is
		// still reaped and invariant (B) is still evaluated in the same trial.
		for w, cmd := range cmds {
			if err := cmd.Wait(); err != nil {
				t.Errorf("trial %d: invariant A violated — worker %d hard-failed its Append during concurrent rotation: %v", trial, w, err)
			}
		}

		got, err := ReadAll(dir)
		if err != nil {
			t.Fatalf("trial %d: ReadAll: %v", trial, err) // test-harness failure, not an invariant
		}
		present := make(map[int64]bool, len(got[session]))
		for _, ev := range got[session] {
			present[ev.TS] = true
		}
		// Invariant (B): every seeded canary event survived rotation into ".1".
		// The other way a missing lock can bite: two processes both rotate and a
		// later rename clobbers the fresh ".1" with a tiny just-written file,
		// silently dropping the whole canary generation. One missing event is
		// proof enough — report it and move to the next trial.
		for i := int64(0); i < canaryEvents; i++ {
			ts := canaryBase + i
			if !present[ts] {
				t.Errorf("trial %d: invariant B violated — canary event TS=%d missing; the .1 backup was clobbered by a concurrent cross-process rotation (%d/%d canary events survived)",
					trial, ts, countCanary(present, canaryBase, canaryEvents), canaryEvents)
				break
			}
		}
	}
}

// seedCanary writes canaryEvents parseable Event lines to path, padded so the
// file exceeds minSize (forcing the next Append to rotate it), and fails the
// test if it did not actually clear minSize.
func seedCanary(t *testing.T, path string, base int64, count int, minSize int) {
	t.Helper()
	var b strings.Builder
	pad := strings.Repeat("c", 300) // fat Detail so a handful of events > minSize
	for i := 0; i < count; i++ {
		line, err := json.Marshal(Event{
			TS:        base + int64(i),
			SessionID: "sess-xproc",
			ToState:   "running",
			Trigger:   TriggerScan,
			Actor:     ActorSystem,
			Detail:    pad,
		})
		if err != nil {
			t.Fatalf("marshal canary %d: %v", i, err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if b.Len() <= minSize {
		t.Fatalf("seed is %d bytes, need > %d to force a rotation — raise the pad or count", b.Len(), minSize)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
}

func countCanary(present map[int64]bool, base int64, count int) int {
	n := 0
	for i := int64(0); i < int64(count); i++ {
		if present[base+i] {
			n++
		}
	}
	return n
}
