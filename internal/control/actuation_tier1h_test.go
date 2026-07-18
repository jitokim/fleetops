package control

import (
	"errors"
	"testing"

	"github.com/jitokim/fleetops/internal/sessions"
)

// testSendHostApp is a host_app that exists only for these tests, registered
// into hostAppSendAdapters for the duration of one test. Using a fake host
// rather than "iTerm.app" keeps the DISPATCH tests independent of iTerm2's real
// adapter — what is under test here is tier ordering, not osascript.
const testSendHostApp = "TestHost.app"

// withFakeSendAdapter registers adapter under testSendHostApp and restores the
// registry afterwards.
func withFakeSendAdapter(t *testing.T, adapter SendAdapter) {
	t.Helper()
	orig, had := hostAppSendAdapters[testSendHostApp]
	t.Cleanup(func() {
		if had {
			hostAppSendAdapters[testSendHostApp] = orig
			return
		}
		delete(hostAppSendAdapters, testSendHostApp)
	})
	hostAppSendAdapters[testSendHostApp] = adapter
}

// tier1hEntry is a registry entry that passes the binding gate and names a host
// with a send adapter.
func tier1hEntry() sessions.SessionEntry {
	return sessions.SessionEntry{
		PID:      42,
		TTY:      "ttys012",
		HostApp:  testSendHostApp,
		WindowID: "w0t1p0:ABC-123",
	}
}

// writeTier1hSession writes tier1hEntry (with any overrides applied) and pins
// the binding probe to a confirming answer.
func writeTier1hSession(t *testing.T, entry sessions.SessionEntry, pidTTY string) string {
	t.Helper()
	dir := t.TempDir()
	if err := sessions.WriteSession(dir, "sess-1", entry); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	orig := pidTTYFn
	t.Cleanup(func() { pidTTYFn = orig })
	pidTTYFn = func(int) string { return pidTTY }
	return dir
}

// TestTierOneH_TierOneAHits_SendAdapterNeverCalled is the wrong-pane SAFETY
// property, asserted directly. A multiplexer running INSIDE the host terminal
// (tmux in iTerm2) must be addressed by its precise pane, never by the
// enclosing host session — so whenever Tier 1a resolves, Tier 1h must not even
// be consulted.
func TestTierOneH_TierOneAHits_SendAdapterNeverCalled(t *testing.T) {
	f := &fakeSendAdapter{}
	withFakeSendAdapter(t, f)
	dir := writeTier1hSession(t, tier1hEntry(), "ttys012")

	tmux := &fakeResolveTTYCtl{fakeResolveCtl: &fakeResolveCtl{t: t, name: "tmux", available: true}, locateByTTYOK: true}
	withBackends(t, tmux)

	act, _, found := ResolveActuationTarget(dir, "sess-1", "-x-proj")

	if !found {
		t.Fatal("expected found=true — Tier 1a hits")
	}
	if act.Tier() != actuationTierMultiplexer {
		t.Errorf("tier = %q, want %q — Tier 1a must win over 1h", act.Tier(), actuationTierMultiplexer)
	}
	if act.Backend() != "tmux" {
		t.Errorf("backend = %q, want tmux", act.Backend())
	}
	// The decisive assertion: 1h was never even resolved, let alone used.
	if len(f.sentTexts) != 0 || f.interruptCalled {
		t.Error("the send adapter was used even though Tier 1a resolved — a multiplexer inside the host window would be typed into via the wrong pane")
	}
}

// TestTierOneH_TierOneAMisses_ResolvesHostSend: 1a found nothing (no backend
// implements TTYLocator, or none matched), the entry names a host with an
// adapter → 1h resolves.
func TestTierOneH_TierOneAMisses_ResolvesHostSend(t *testing.T) {
	withFakeSendAdapter(t, &fakeSendAdapter{})
	dir := writeTier1hSession(t, tier1hEntry(), "ttys012")

	// orca has no TTYLocator (its real shape) and would match Tier 1b's cwd
	// probe — so if 1h did NOT resolve, 1b would, and the tier label would
	// betray it.
	orca := &fakeResolveCtl{t: t, name: "orca", available: true, locateClaudeOK: true}
	withBackends(t, orca)

	act, backendAvailable, found := ResolveActuationTarget(dir, "sess-1", "-x-proj")

	if !found || !backendAvailable {
		t.Fatalf("found=%v backendAvailable=%v, want both true", found, backendAvailable)
	}
	if act.Tier() != actuationTierHostSend {
		t.Errorf("tier = %q, want %q — 1h must resolve before 1b", act.Tier(), actuationTierHostSend)
	}
	if act.Backend() != "fakehost" {
		t.Errorf("backend = %q, want fakehost", act.Backend())
	}
}

// TestTierOneH_ResolvesBeforeTierOneB pins the ordering against a backend whose
// cwd probe WOULD hit: 1h is session-exact, 1b is a many-to-one guess, so the
// precise tier must not be shadowed by the guessing one.
func TestTierOneH_ResolvesBeforeTierOneB(t *testing.T) {
	withFakeSendAdapter(t, &fakeSendAdapter{})
	dir := writeTier1hSession(t, tier1hEntry(), "ttys012")

	// This backend would resolve via Tier 1b (locateClaudeOK) but not 1a.
	orca := &fakeResolveCtl{t: t, name: "orca", available: true, locateClaudeOK: true}
	withBackends(t, orca)

	act, _, found := ResolveActuationTarget(dir, "sess-1", "-x-proj")

	if !found {
		t.Fatal("expected found=true")
	}
	if act.Tier() == actuationTierMultiplexer && act.Backend() == "orca" {
		t.Error("Tier 1b shadowed Tier 1h — a cwd guess beat a session-exact identifier")
	}
	if act.Tier() != actuationTierHostSend {
		t.Errorf("tier = %q, want %q", act.Tier(), actuationTierHostSend)
	}
}

// TestTierOneH_BindingInvalid_SkipsHostSend: 1h sits behind the SAME pid↔tty
// binding gate as 1a. A dead or moved session must not reach the host adapter —
// the registry entry may be stale and the host may have recycled the tab.
func TestTierOneH_BindingInvalid_SkipsHostSend(t *testing.T) {
	for name, pidTTY := range map[string]string{
		"dead pid":             "",        // no controlling tty at all
		"moved to another tty": "ttys999", // pid recycled onto a different tty
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeSendAdapter{}
			withFakeSendAdapter(t, f)
			dir := writeTier1hSession(t, tier1hEntry(), pidTTY)

			orca := &fakeResolveCtl{t: t, name: "orca", available: true, locateClaudeOK: true}
			withBackends(t, orca)

			act, _, found := ResolveActuationTarget(dir, "sess-1", "-x-proj")

			if !found {
				t.Fatal("expected found=true via Tier 1b — the cwd chain still applies")
			}
			if act.Tier() != actuationTierMultiplexer {
				t.Errorf("tier = %q, want %q — an invalid binding must skip 1h entirely", act.Tier(), actuationTierMultiplexer)
			}
		})
	}
}

// TestTierOneH_EmptyTTY_SkipsHostSend: 1h's own guard needs a recorded tty to
// compare the host's against, and the shared binding gate already requires one.
// A headless/piped session (no controlling terminal) must fall through.
func TestTierOneH_EmptyTTY_SkipsHostSend(t *testing.T) {
	f := &fakeSendAdapter{}
	withFakeSendAdapter(t, f)

	entry := tier1hEntry()
	entry.TTY = ""
	dir := writeTier1hSession(t, entry, "ttys012")

	orca := &fakeResolveCtl{t: t, name: "orca", available: true, locateClaudeOK: true}
	withBackends(t, orca)

	act, _, found := ResolveActuationTarget(dir, "sess-1", "-x-proj")

	if !found {
		t.Fatal("expected found=true via Tier 1b")
	}
	if act.Tier() != actuationTierMultiplexer {
		t.Errorf("tier = %q, want %q — no recorded tty means no 1h binding to verify", act.Tier(), actuationTierMultiplexer)
	}
}

// TestTierOneH_UnknownHostApp_ByteIdenticalToToday is the PURE-SUPERSET
// regression guard for every existing orca/cmux/tmux user. An unknown, empty,
// or multiplexer host_app must skip 1h silently and resolve exactly as it did
// before this tier existed. Asserted explicitly rather than left as an
// implication.
func TestTierOneH_UnknownHostApp_ByteIdenticalToToday(t *testing.T) {
	for _, hostApp := range []string{"", "Apple_Terminal", "tmux", "vscode", "TestHost.app.evil"} {
		t.Run("host_app="+hostApp, func(t *testing.T) {
			withFakeSendAdapter(t, &fakeSendAdapter{})

			entry := tier1hEntry()
			entry.HostApp = hostApp
			dir := writeTier1hSession(t, entry, "ttys012")

			orca := &fakeResolveCtl{t: t, name: "orca", available: true, locateClaudeOK: true}
			withBackends(t, orca)

			act, backendAvailable, found := ResolveActuationTarget(dir, "sess-1", "-x-proj")

			if !found || !backendAvailable {
				t.Fatalf("found=%v backendAvailable=%v, want both true via Tier 1b", found, backendAvailable)
			}
			if act.Tier() != actuationTierMultiplexer {
				t.Errorf("tier = %q, want %q — an unregistered host_app must not divert off the proven path", act.Tier(), actuationTierMultiplexer)
			}
			if target := boundOf(t, act).target; target.ID != "orca:claude" {
				t.Errorf("target.ID = %q, want orca:claude (the unchanged Tier 1b outcome)", target.ID)
			}
		})
	}
}

// TestTierOneH_NoBackendAvailable_StillResolvesHostSend is the tier's whole
// target audience: a fresh macOS user running claude in iTerm2 with NO
// multiplexer installed. Tier 1h needs no orca/tmux/cmux — the host terminal
// writes to its own session — so the availability gate must sit BELOW it, not
// above.
func TestTierOneH_NoBackendAvailable_StillResolvesHostSend(t *testing.T) {
	withFakeSendAdapter(t, &fakeSendAdapter{})
	dir := writeTier1hSession(t, tier1hEntry(), "ttys012")
	withBackends(t, &fakeResolveCtl{t: t, name: "orca", available: false})

	act, backendAvailable, found := ResolveActuationTarget(dir, "sess-1", "-x-proj")

	if !found || !backendAvailable {
		t.Fatalf("found=%v backendAvailable=%v, want both true — 1h needs no multiplexer", found, backendAvailable)
	}
	if act.Tier() != actuationTierHostSend {
		t.Errorf("tier = %q, want %q", act.Tier(), actuationTierHostSend)
	}
	// backendAvailable=true is what suppresses the caller's "no orca/tmux/cmux"
	// hint — showing it while 1h is actively handling the keypress would be a
	// lie to the operator.
}

// TestTierOneH_NoBackendAvailable_WithoutHostSend_StillUnavailable is the other
// half: hoisting 1h above the gate must not widen backendAvailable for anyone
// else. A session with no send-capable host_app on a multiplexer-less machine
// reports exactly what it always did.
func TestTierOneH_NoBackendAvailable_WithoutHostSend_StillUnavailable(t *testing.T) {
	for _, hostApp := range []string{"", "Apple_Terminal", "vscode"} {
		t.Run("host_app="+hostApp, func(t *testing.T) {
			withFakeSendAdapter(t, &fakeSendAdapter{})
			entry := tier1hEntry()
			entry.HostApp = hostApp
			dir := writeTier1hSession(t, entry, "ttys012")
			withBackends(t, &fakeResolveCtl{t: t, name: "orca", available: false})

			act, backendAvailable, found := ResolveActuationTarget(dir, "sess-1", "-x-proj")

			if backendAvailable || found {
				t.Errorf("backendAvailable=%v found=%v, want both false", backendAvailable, found)
			}
			if act != nil {
				t.Errorf("actuator = %v, want nil", act)
			}
		})
	}
}

// TestTierOneH_NoBackendAvailable_BindingInvalid_StillUnavailable: a stale
// registry entry must not become an actuation surface just because no
// multiplexer is installed. The shared pid↔tty binding gate still applies.
func TestTierOneH_NoBackendAvailable_BindingInvalid_StillUnavailable(t *testing.T) {
	withFakeSendAdapter(t, &fakeSendAdapter{})
	dir := writeTier1hSession(t, tier1hEntry(), "ttys999") // pid controls a DIFFERENT tty
	withBackends(t, &fakeResolveCtl{t: t, name: "orca", available: false})

	_, backendAvailable, found := ResolveActuationTarget(dir, "sess-1", "-x-proj")

	if backendAvailable || found {
		t.Errorf("backendAvailable=%v found=%v, want both false", backendAvailable, found)
	}
}

// TestTierOneH_ResolvedActuatorActsOnTheRecordedEntry: the resolved actuator
// must be bound to the SAME entry resolution validated — not re-read, not
// re-derived. Sending through it reaches the adapter with that entry.
func TestTierOneH_ResolvedActuatorActsOnTheRecordedEntry(t *testing.T) {
	f := &entryCapturingSendAdapter{}
	withFakeSendAdapter(t, f)
	entry := tier1hEntry()
	dir := writeTier1hSession(t, entry, "ttys012")
	withBackends(t, &fakeResolveCtl{t: t, name: "orca", available: true})

	act, _, found := ResolveActuationTarget(dir, "sess-1", "-x-proj")
	if !found {
		t.Fatal("expected found=true via Tier 1h")
	}
	if err := act.Resume("do the thing"); err != nil {
		t.Fatalf("Resume = %v, want nil", err)
	}

	if f.gotEntry.WindowID != entry.WindowID || f.gotEntry.TTY != entry.TTY {
		t.Errorf("adapter got entry %+v, want the resolved %+v", f.gotEntry, entry)
	}
	if f.gotText != "do the thing" {
		t.Errorf("adapter got text %q, want %q", f.gotText, "do the thing")
	}
}

type entryCapturingSendAdapter struct {
	gotEntry sessions.SessionEntry
	gotText  string
}

func (f *entryCapturingSendAdapter) Name() string { return "fakehost" }
func (f *entryCapturingSendAdapter) SendText(entry sessions.SessionEntry, text string) error {
	f.gotEntry, f.gotText = entry, text
	return nil
}
func (f *entryCapturingSendAdapter) Interrupt(entry sessions.SessionEntry) error {
	f.gotEntry = entry
	return nil
}

// TestTierOneH_SendFailureSurfacesAsError: a resolved 1h actuator that then
// misses (or refuses on a tty mismatch) must report an ERROR. Resolution
// happens before the host is consulted, so the honest failure has to arrive at
// send time — it must never be swallowed into a silent success.
//
// Surfacing the error is NOT the same as it being terminal. What the caller
// does next is the caller's policy and lives at the TUI dispatch site: r/i
// degrade to Tier 2 (see IsHostSendTier, and
// TestSendPromptCmd_TierOneHSendFails_FallsToTierTwoRedrive), k/p/a have no
// Tier 2 and report it. This layer's only job is to tell the truth.
func TestTierOneH_SendFailureSurfacesAsError(t *testing.T) {
	for name, wantErr := range map[string]error{
		"session closed": ErrSendNoSession,
		"tty mismatch":   ErrSendTTYMismatch,
	} {
		t.Run(name, func(t *testing.T) {
			withFakeSendAdapter(t, &fakeSendAdapter{sendErr: wantErr})
			dir := writeTier1hSession(t, tier1hEntry(), "ttys012")
			withBackends(t, &fakeResolveCtl{t: t, name: "orca", available: true})

			act, _, found := ResolveActuationTarget(dir, "sess-1", "-x-proj")
			if !found {
				t.Fatal("expected found=true via Tier 1h")
			}

			if err := act.Resume("x"); !errors.Is(err, wantErr) {
				t.Errorf("Resume = %v, want %v", err, wantErr)
			}
			if err := act.Approve(); !errors.Is(err, wantErr) {
				t.Errorf("Approve = %v, want %v", err, wantErr)
			}
			if err := act.Interrupt(); !errors.Is(err, wantErr) {
				t.Errorf("Interrupt = %v, want %v", err, wantErr)
			}
		})
	}
}
