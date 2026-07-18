package control

import "github.com/jitokim/fleetops/internal/sessions"

// The interrupt (Esc) path lives in its own file, apart from the text path in
// hostsend.go, because it is the ONLY verb that delivers a raw control
// character and so is the one path where a mangled byte would be hardest to
// notice — a failed interrupt looks exactly like a loop that ignored you. It
// shares the text path's script skeleton, GUID/tty validation and verdict
// handling verbatim (that is the safety-critical machinery, and duplicating it
// would be the drift hazard this codebase repeatedly warns about); what differs
// is exactly one statement, isolated below.

// Interrupt delivers a raw Esc to the session without submitting — stop the
// current turn, leave the process alive.
//
// Same refusal and verdict semantics as SendText: the GUID and tty are
// validated before any exec, the session's own tty is verified inside the
// osascript round trip, and a miss/mismatch/unrecognized verdict is a failure
// rather than a silent no-op.
func (a iterm2SendAdapter) Interrupt(entry sessions.SessionEntry) error {
	guid, tty, err := iterm2SendTarget(entry)
	if err != nil {
		return err
	}
	return iterm2SendVerdict(iterm2InterruptArgv(guid, tty))
}

// iterm2InterruptArgv builds the argv for an interrupt. Layout:
//
//	osascript -e <script> -- /dev/<tty>
//
// There is no payload argument: the Esc is a class-A CONSTANT baked into the
// fixed script (see iterm2EscStmt), not data. item 1 of argv is still the
// expected tty, so the binding guard is identical to the text path's.
func iterm2InterruptArgv(guid sessionGUID, tty string) []string {
	return iterm2SendArgvPrefix(guid, iterm2EscStmt, tty)
}

// iterm2EscStmt writes a raw ESC (0x1b) with NO trailing newline — an
// interrupt, not a submit.
//
// Esc is written as the AppleScript literal `(character id 27)` rather than
// passed through argv, even though argv is verified to carry control bytes
// intact. It is a fixed constant in fleetops's own source, not user data, so it
// is class A by definition; expressing it as a literal means the one verb whose
// payload is a single invisible byte does not depend on byte fidelity across
// the Apple Event boundary at all. Zero injection surface, and strictly simpler
// to reason about.
//
// `newline no` is essential: a trailing CR here would submit whatever is
// sitting in the prompt instead of aborting the turn. ESC delivery to the pty
// through `write` is verified at the byte level (.notes/design-iterm2-tier1.md
// §8b/E4 — `od` on a `stty raw` capture read `033`).
const iterm2EscStmt = "write aSession text (character id 27) newline no"
